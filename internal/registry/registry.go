// Package registry resolves image references to digests with the OCI
// distribution API. Substrate only accepts digest-pinned images, since a
// snapshot is only valid for the image it was taken from.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	dockerHub    = "docker.io"
	dockerHubAPI = "registry-1.docker.io"
)

const manifestTypes = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

type Ref struct {
	Registry   string // host[:port], as used for the API
	Repository string
	Tag        string
	Digest     string
	original   string
}

// Parse splits an image reference the way docker does: a first component with
// a dot, a colon or "localhost" is a registry; Docker Hub is the default.
func Parse(image string) (Ref, error) {
	r := Ref{original: image}
	rest := image
	if name, digest, ok := strings.Cut(rest, "@"); ok {
		rest, r.Digest = name, digest
	}
	host, path, ok := strings.Cut(rest, "/")
	if !ok || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		host, path = dockerHub, rest
	}
	if i := strings.LastIndex(path, ":"); i > 0 {
		path, r.Tag = path[:i], path[i+1:]
	}
	if host == dockerHub {
		host = dockerHubAPI
		if !strings.Contains(path, "/") {
			path = "library/" + path
		}
	}
	if path == "" {
		return Ref{}, fmt.Errorf("invalid image reference %q", image)
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	r.Registry, r.Repository = host, path
	return r, nil
}

// Pinned returns the reference with the digest appended, keeping the
// registry and repository as written.
func (r Ref) Pinned(digest string) string {
	name, _, _ := strings.Cut(r.original, "@")
	return name + "@" + digest
}

type Resolver struct {
	Client *http.Client
	// Credentials returns basic-auth credentials for a registry host, if any.
	Credentials func(host string) (user, password string, ok bool)
	// Insecure lists registries reached over plain HTTP.
	Insecure map[string]bool
}

// Resolve returns image pinned by digest. Already pinned images are returned as is.
func (res *Resolver) Resolve(ctx context.Context, image string) (string, error) {
	ref, err := Parse(image)
	if err != nil {
		return "", err
	}
	if ref.Digest != "" {
		return image, nil
	}
	scheme := "https"
	if res.Insecure[ref.Registry] {
		scheme = "http"
	}
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, ref.Registry, ref.Repository, ref.Tag)

	resp, err := res.do(ctx, http.MethodHead, u, "")
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		token, err := res.token(ctx, ref.Registry, resp.Header.Get("WWW-Authenticate"))
		if err != nil {
			return "", fmt.Errorf("%s: %w", image, err)
		}
		if resp, err = res.do(ctx, http.MethodHead, u, token); err != nil {
			return "", err
		}
		_ = resp.Body.Close()
		if d := resp.Header.Get("Docker-Content-Digest"); resp.StatusCode == http.StatusOK && d != "" {
			return ref.Pinned(d), nil
		}
		return res.digestFromBody(ctx, image, ref, u, token, resp)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); resp.StatusCode == http.StatusOK && d != "" {
		return ref.Pinned(d), nil
	}
	return res.digestFromBody(ctx, image, ref, u, "", resp)
}

// digestFromBody hashes the manifest when a registry omits the digest header on HEAD.
func (res *Resolver) digestFromBody(ctx context.Context, image string, ref Ref, u, auth string, head *http.Response) (string, error) {
	if head.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: registry answered %s", image, head.Status)
	}
	resp, err := res.do(ctx, http.MethodGet, u, auth)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: registry answered %s", image, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return ref.Pinned("sha256:" + hex.EncodeToString(sum[:])), nil
}

func (res *Resolver) do(ctx context.Context, method, u, auth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestTypes)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	c := res.Client
	if c == nil {
		c = http.DefaultClient
	}
	return c.Do(req)
}

// token answers a 401 challenge: Basic uses the stored credentials, Bearer
// fetches a token (anonymously unless credentials exist).
func (res *Resolver) token(ctx context.Context, host, challenge string) (string, error) {
	user, pass, haveCreds := "", "", false
	if res.Credentials != nil {
		user, pass, haveCreds = res.Credentials(host)
	}
	scheme, params, _ := strings.Cut(challenge, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if !haveCreds {
			return "", errors.New("registry requires credentials")
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), nil
	case "bearer":
	default:
		return "", fmt.Errorf("unsupported registry auth challenge %q", challenge)
	}
	p := parseChallenge(params)
	realm, err := url.Parse(p["realm"])
	if err != nil || p["realm"] == "" {
		return "", fmt.Errorf("bad registry auth challenge %q", challenge)
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if p[k] != "" {
			q.Set(k, p[k])
		}
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if haveCreds {
		req.SetBasicAuth(user, pass)
	}
	c := res.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token endpoint answered %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Token == "" {
		body.Token = body.AccessToken
	}
	if body.Token == "" {
		return "", errors.New("registry token endpoint returned no token")
	}
	return "Bearer " + body.Token, nil
}

func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		var kv string
		kv, s = splitParam(s)
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok {
			out[strings.ToLower(k)] = strings.Trim(v, `"`)
		}
	}
	return out
}

// splitParam cuts at the first comma outside quotes (scopes contain commas).
func splitParam(s string) (string, string) {
	quoted := false
	for i, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

// DockerConfig loads credentials from a docker config.json, such as a mounted
// kubernetes.io/dockerconfigjson Secret. A missing file means no credentials.
func DockerConfig(path string) (func(host string) (string, string, bool), error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return func(string) (string, string, bool) { return "", "", false }, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	creds := map[string][2]string{}
	for host, a := range cfg.Auths {
		user, pass := a.Username, a.Password
		if a.Auth != "" {
			if dec, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
				user, pass, _ = strings.Cut(string(dec), ":")
			}
		}
		host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
		host, _, _ = strings.Cut(host, "/")
		if host == "index.docker.io" || host == dockerHub {
			host = dockerHubAPI
		}
		creds[host] = [2]string{user, pass}
	}
	return func(host string) (string, string, bool) {
		c, ok := creds[host]
		return c[0], c[1], ok
	}, nil
}
