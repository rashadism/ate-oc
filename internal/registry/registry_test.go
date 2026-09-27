package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const user, password = "bot", "pw"

func TestParse(t *testing.T) {
	tests := []struct{ in, registry, repo, tag, digest string }{
		{"nginx", "registry-1.docker.io", "library/nginx", "latest", ""},
		{"traefik/whoami:v1", "registry-1.docker.io", "traefik/whoami", "v1", ""},
		{"docker.io/library/redis:7", "registry-1.docker.io", "library/redis", "7", ""},
		{"ghcr.io/openchoreo/samples/greeter-service:latest", "ghcr.io", "openchoreo/samples/greeter-service", "latest", ""},
		{"localhost:5000/app", "localhost:5000", "app", "latest", ""},
		{"registry.local:5000/team/app:1.2@sha256:ab", "registry.local:5000", "team/app", "1.2", "sha256:ab"},
	}
	for _, tt := range tests {
		r, err := Parse(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if r.Registry != tt.registry || r.Repository != tt.repo || r.Tag != tt.tag || r.Digest != tt.digest {
			t.Errorf("Parse(%q) = %+v", tt.in, r)
		}
	}
	if r, _ := Parse("ghcr.io/x/app:1"); r.Pinned("sha256:ff") != "ghcr.io/x/app:1@sha256:ff" {
		t.Errorf("Pinned keeps the reference as written, got %s", r.Pinned("sha256:ff"))
	}
}

// fakeRegistry serves one manifest behind the given auth mode.
func fakeRegistry(t *testing.T, mode string, digestHeader bool) (*httptest.Server, string) {
	t.Helper()
	manifest := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(manifest)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if mode == "bearer-creds" {
				if u, p, ok := r.BasicAuth(); !ok || u != user || p != password {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			}
			if r.URL.Query().Get("scope") != "repository:team/app:pull" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"token":"t0k"}`))
		case "/v2/team/app/manifests/1.0":
			authorized := false
			switch mode {
			case "anonymous":
				authorized = true
			case "bearer", "bearer-creds":
				authorized = r.Header.Get("Authorization") == "Bearer t0k"
				if !authorized {
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:team/app:pull"`)
				}
			case "basic":
				u, p, ok := r.BasicAuth()
				authorized = ok && u == user && p == password
				if !authorized {
					w.Header().Set("WWW-Authenticate", `Basic realm="reg"`)
				}
			}
			if !authorized {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "image.index") {
				w.WriteHeader(http.StatusNotAcceptable)
				return
			}
			if digestHeader {
				w.Header().Set("Docker-Content-Digest", digest)
			}
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, digest
}

func TestResolve(t *testing.T) {
	creds := func(string) (string, string, bool) { return user, password, true }
	tests := []struct {
		mode         string
		digestHeader bool
		creds        bool
		wantErr      bool
	}{
		{mode: "anonymous", digestHeader: true},
		{mode: "anonymous"},
		{mode: "bearer", digestHeader: true},
		{mode: "bearer-creds", digestHeader: true, creds: true},
		{mode: "bearer-creds", digestHeader: true, wantErr: true},
		{mode: "basic", digestHeader: true, creds: true},
		{mode: "basic", digestHeader: true, wantErr: true},
	}
	for _, tt := range tests {
		name := tt.mode
		if !tt.digestHeader {
			name += " without digest header"
		}
		if tt.creds {
			name += " with credentials"
		}
		t.Run(name, func(t *testing.T) {
			srv, digest := fakeRegistry(t, tt.mode, tt.digestHeader)
			host := strings.TrimPrefix(srv.URL, "http://")
			res := &Resolver{Insecure: map[string]bool{host: true}}
			if tt.creds {
				res.Credentials = creds
			}
			got, err := res.Resolve(context.Background(), host+"/team/app:1.0")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %s", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != host+"/team/app:1.0@"+digest {
				t.Fatalf("got %s", got)
			}
		})
	}

	res := &Resolver{}
	if got, err := res.Resolve(context.Background(), "ghcr.io/x/app@sha256:ab"); err != nil || got != "ghcr.io/x/app@sha256:ab" {
		t.Fatalf("pinned images pass through, got %s %v", got, err)
	}
	srv, _ := fakeRegistry(t, "anonymous", true)
	host := strings.TrimPrefix(srv.URL, "http://")
	if _, err := (&Resolver{Insecure: map[string]bool{host: true}}).Resolve(context.Background(), host+"/team/missing:1"); err == nil {
		t.Fatal("unknown image should fail")
	}
}

func TestDockerConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := `{"auths":{"https://index.docker.io/v1/":{"auth":"dXNlcjpwYXNz"},"asia-docker.pkg.dev":{"username":"_json_key","password":"k"}}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := DockerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if u, p, ok := creds("registry-1.docker.io"); !ok || u != "user" || p != "pass" {
		t.Errorf("docker hub creds = %s/%s/%v", u, p, ok)
	}
	if u, _, ok := creds("asia-docker.pkg.dev"); !ok || u != "_json_key" {
		t.Error("plain username/password entries")
	}
	none, err := DockerConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := none("ghcr.io"); ok {
		t.Error("a missing config means no credentials")
	}
}
