package frontdoor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rashadism/ate-oc/api/v1alpha1"
)

const (
	cellA  = "dp-acme-shop-dev-11111111"
	cellB  = "dp-acme-billing-dev-22222222"
	cellC  = "dp-other-shop-dev-33333333"
	cellP  = "dp-acme-shop-prod-44444444"
	gwNS   = "openchoreo-data-plane"
	someNS = "default"
)

type fakeDir struct {
	callers map[string]Caller
	targets map[string]Target
	labels  map[string]map[string]string
}

func (d *fakeDir) Caller(ip string) Caller { return d.callers[ip] }
func (d *fakeDir) Target(ns, svc string, port int32) (Target, bool) {
	t, ok := d.targets[fmt.Sprintf("%s/%s:%d", ns, svc, port)]
	return t, ok
}
func (d *fakeDir) NamespaceLabels(ns string) map[string]string { return d.labels[ns] }

func directory() *fakeDir {
	vis := func(v ...v1alpha1.EndpointVisibility) []v1alpha1.EndpointVisibility { return v }
	target := func(ns, svc string, port, actorPort int32, v []v1alpha1.EndpointVisibility) Target {
		return Target{Namespace: ns, Service: svc, Port: port, Atespace: ns, Actor: "a-" + svc, ActorPort: actorPort,
			Visibility: v, Ready: true}
	}
	return &fakeDir{
		callers: map[string]Caller{
			"10.0.0.1": {Kind: CallerPod, Namespace: cellA},
			"10.0.0.2": {Kind: CallerPod, Namespace: cellB},
			"10.0.0.3": {Kind: CallerPod, Namespace: cellC},
			"10.0.0.4": {Kind: CallerPod, Namespace: cellP},
			"10.0.0.5": {Kind: CallerPod, Namespace: gwNS, SystemComponent: true},
			"10.0.0.6": {Kind: CallerPod, Namespace: someNS},
			"10.0.0.7": {Kind: CallerEgress, Namespace: "ate-system"},
		},
		targets: map[string]Target{
			cellA + "/private:80":  target(cellA, "private", 80, 80, nil),
			cellA + "/shared:8080": target(cellA, "shared", 8080, 9090, vis(v1alpha1.VisibilityNamespace)),
			cellA + "/public:80":   target(cellA, "public", 80, 80, vis(v1alpha1.VisibilityExternal)),
			cellA + "/cold:80":     {Namespace: cellA, Service: "cold", Port: 80},
			cellA + "/held:80":     {Namespace: cellA, Service: "held", Port: 80, Atespace: cellA, Actor: "a-held", ActorPort: 80, Ready: true, Paused: true},
		},
		labels: map[string]map[string]string{
			cellA: {LabelNamespace: "acme", LabelEnvironment: "dev"},
			cellB: {LabelNamespace: "acme", LabelEnvironment: "dev"},
			cellC: {LabelNamespace: "other", LabelEnvironment: "dev"},
			cellP: {LabelNamespace: "acme", LabelEnvironment: "prod"},
		},
	}
}

type seen struct {
	mu        sync.Mutex
	actor     string
	host      string
	target    string
	authority string
	path      string
}

func (s *seen) record(f func(*seen)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// routers starts a fake Substrate router: a plain HTTP listener and a CONNECT
// listener that serves one HTTP request per tunnel.
func routers(t *testing.T) (Config, *seen) {
	t.Helper()
	s := &seen{}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(func(s *seen) {
			s.actor, s.host, s.target, s.path = r.Header.Get(ActorHeader), r.Host, r.Header.Get(TargetHeader), r.URL.Path
		})
		_, _ = io.WriteString(w, "plain")
	}))
	t.Cleanup(plain.Close)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				br := bufio.NewReader(conn)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				s.record(func(s *seen) { s.authority, s.actor = req.Host, req.Header.Get(ActorHeader) })
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
				inner, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				s.record(func(s *seen) { s.host, s.path = inner.Host, inner.URL.Path })
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 6\r\nConnection: close\r\n\r\ntunnel")
			}()
		}
	}()
	return Config{RouterHTTP: plain.Listener.Addr().String(), RouterConnect: lis.Addr().String(), ClusterDomain: "cluster.local"}, s
}

type result struct {
	aborted bool
	code    int
	body    string
}

func serve(h http.Handler, from, host string, header map[string]string) (res result) {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+"/orders", nil)
	r.RemoteAddr = from + ":41000"
	r.Host = host
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	defer func() {
		if p := recover(); p != nil {
			if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				res.aborted = true
				return
			}
			panic(p)
		}
	}()
	h.ServeHTTP(w, r)
	return result{code: w.Code, body: w.Body.String()}
}

func TestVisibility(t *testing.T) {
	cfg, _ := routers(t)
	h := New(directory(), cfg)
	fqdn := func(svc, ns string, port int) string {
		if port == 80 {
			return svc + "." + ns + ".svc.cluster.local"
		}
		return fmt.Sprintf("%s.%s.svc.cluster.local:%d", svc, ns, port)
	}

	tests := []struct {
		name   string
		from   string
		host   string
		header map[string]string
		allow  bool
	}{
		{name: "project: same cell, short name", from: "10.0.0.1", host: "private", allow: true},
		{name: "project: same cell, fqdn", from: "10.0.0.1", host: fqdn("private", cellA, 80), allow: true},
		{name: "project: other cell denied", from: "10.0.0.2", host: fqdn("private", cellA, 80)},
		{name: "namespace: same org and env", from: "10.0.0.2", host: fqdn("shared", cellA, 8080), allow: true},
		{name: "namespace: other org denied", from: "10.0.0.3", host: fqdn("shared", cellA, 8080)},
		{name: "namespace: other env denied", from: "10.0.0.4", host: fqdn("shared", cellA, 8080)},
		{name: "namespace: unlabelled ns denied", from: "10.0.0.6", host: fqdn("shared", cellA, 8080)},
		{name: "external: gateway via header", from: "10.0.0.5", host: "dev-acme.apps.example.com",
			header: map[string]string{TargetHeader: cellA + "/public:80"}, allow: true},
		{name: "external: gateway to project-only endpoint denied", from: "10.0.0.5", host: "dev-acme.apps.example.com",
			header: map[string]string{TargetHeader: cellA + "/private:80"}},
		{name: "external: non-gateway caller denied", from: "10.0.0.6", host: fqdn("public", cellA, 80)},
		{name: "header from a non system component is refused", from: "10.0.0.2",
			host: fqdn("shared", cellA, 8080), header: map[string]string{TargetHeader: cellA + "/private:80"}},
		{name: "header from egress is refused", from: "10.0.0.7", host: fqdn("private", cellA, 80),
			header: map[string]string{TargetHeader: cellA + "/private:80"}},
		{name: "egress is pre-authorized", from: "10.0.0.7", host: fqdn("private", cellA, 80), allow: true},
		{name: "unknown caller denied", from: "10.9.9.9", host: fqdn("private", cellA, 80)},
		{name: "unknown target denied", from: "10.0.0.1", host: "nope"},
		{name: "wrong port denied", from: "10.0.0.1", host: "private:8080"},
		{name: "foreign domain denied", from: "10.0.0.1", host: "private.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := serve(h, tt.from, tt.host, tt.header)
			if tt.allow && (res.aborted || res.code != http.StatusOK) {
				t.Fatalf("want allowed, got %+v", res)
			}
			if !tt.allow && !res.aborted {
				t.Fatalf("want the connection dropped, got %+v", res)
			}
		})
	}
}

func TestProxying(t *testing.T) {
	cfg, s := routers(t)
	h := New(directory(), cfg)

	t.Run("port 80 goes to the router with the actor header", func(t *testing.T) {
		res := serve(h, "10.0.0.5", "dev-acme.apps.example.com", map[string]string{
			TargetHeader: cellA + "/public:80",
			ActorHeader:  "spoofed/actor",
		})
		if res.body != "plain" {
			t.Fatalf("res = %+v", res)
		}
		if s.actor != cellA+"/a-public" || s.target != "" || s.host != "dev-acme.apps.example.com" || s.path != "/orders" {
			t.Fatalf("router saw actor=%q target=%q host=%q path=%q", s.actor, s.target, s.host, s.path)
		}
	})

	t.Run("other ports tunnel with CONNECT to the actor port", func(t *testing.T) {
		host := "shared." + cellA + ".svc.cluster.local:8080"
		res := serve(h, "10.0.0.2", host, nil)
		if res.body != "tunnel" {
			t.Fatalf("res = %+v", res)
		}
		if s.authority != "a-shared:9090" || s.actor != cellA+"/a-shared" || s.host != host || s.path != "/orders" {
			t.Fatalf("router saw authority=%q actor=%q host=%q path=%q", s.authority, s.actor, s.host, s.path)
		}
	})

	t.Run("no actor yet answers warming up", func(t *testing.T) {
		if res := serve(h, "10.0.0.1", "cold", nil); res.code != http.StatusServiceUnavailable || res.body != "warming up\n" {
			t.Fatalf("res = %+v", res)
		}
	})

	t.Run("paused answers 503 without waking", func(t *testing.T) {
		if res := serve(h, "10.0.0.1", "held", nil); res.code != http.StatusServiceUnavailable || res.body != "paused\n" {
			t.Fatalf("res = %+v", res)
		}
	})
}

func TestParseTargetHeader(t *testing.T) {
	for _, v := range []string{"", "ns", "ns/svc", "ns/svc:x", "/svc:80", "ns/:80"} {
		if _, _, _, ok := parseTargetHeader(v); ok {
			t.Errorf("%q should not parse", v)
		}
	}
	if ns, svc, port, ok := parseTargetHeader("ns/orders:8080"); !ok || ns != "ns" || svc != "orders" || port != 8080 {
		t.Fatal("valid header did not parse")
	}
}
