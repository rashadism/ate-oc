// Package frontdoor proxies traffic for actor-backed components. It stands in
// for the pods OpenChoreo's per-component NetworkPolicy would select: it works
// out the target from the request, re-applies the same visibility rules to the
// caller, and forwards to the Substrate router with the actor header set.
package frontdoor

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rashadism/oc-substrate/api/v1alpha1"
)

const (
	// TargetHeader names the target on gateway traffic, whose Host is public
	// and whose path is already rewritten. Only system components may set it.
	TargetHeader = "X-Oc-Substrate-Target"
	// ActorHeader is what the Substrate router routes on.
	ActorHeader = "Ate-Target-Actor"

	LabelNamespace       = "openchoreo.dev/namespace"
	LabelEnvironment     = "openchoreo.dev/environment"
	LabelSystemComponent = "openchoreo.dev/system-component"
)

type CallerKind int

const (
	CallerUnknown CallerKind = iota
	CallerPod
	// CallerEgress is Substrate's shared egress gateway. Actor callers are
	// authorized at the source by their EgressPolicy, which the operator
	// compiles only for targets their visibility admits.
	CallerEgress
)

type Caller struct {
	Kind            CallerKind
	Namespace       string
	SystemComponent bool
}

type Target struct {
	Namespace  string
	Service    string
	Port       int32
	Atespace   string
	Actor      string
	ActorPort  int32
	Visibility []v1alpha1.EndpointVisibility
	// Ready is false until the actor exists; Paused holds it asleep.
	Ready  bool
	Paused bool
}

// Directory answers lookups from the cluster state the front door watches.
type Directory interface {
	Caller(ip string) Caller
	Target(namespace, service string, port int32) (Target, bool)
	NamespaceLabels(namespace string) map[string]string
}

type Config struct {
	// RouterHTTP is the router's plain HTTP address (actor port 80).
	RouterHTTP string
	// RouterConnect is the router's CONNECT address (other actor ports).
	RouterConnect string
	// ClusterDomain is the Service DNS suffix, usually cluster.local.
	ClusterDomain string
}

type Handler struct {
	dir    Directory
	cfg    Config
	plain  *httputil.ReverseProxy
	tunnel *httputil.ReverseProxy
}

func New(dir Directory, cfg Config) *Handler {
	h := &Handler{dir: dir, cfg: cfg}
	h.plain = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", cfg.RouterHTTP
			pr.Out.Host = pr.In.Host
		},
		ErrorHandler: proxyError,
	}
	h.tunnel = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(targetKey{}).(Target)
			// The URL host keys the connection pool: one tunnel set per actor port.
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(t.Atespace+"."+t.Actor, strconv.Itoa(int(t.ActorPort)))
			pr.Out.Host = pr.In.Host
		},
		Transport: &http.Transport{
			DialContext:     h.dialTunnel,
			IdleConnTimeout: 30 * time.Second,
		},
		ErrorHandler: proxyError,
	}
	return h
}

type targetKey struct{}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	caller := h.dir.Caller(ip)
	if caller.Kind == CallerUnknown {
		deny(r, "unknown caller")
	}

	ns, svc, port, ok := h.resolve(r, caller)
	if !ok {
		deny(r, "unresolvable target")
	}
	t, ok := h.dir.Target(ns, svc, port)
	if !ok {
		deny(r, "no actor for "+ns+"/"+svc+":"+strconv.Itoa(int(port)))
	}
	if !h.allowed(caller, t) {
		deny(r, "not visible to caller in "+caller.Namespace)
	}
	if !t.Ready {
		http.Error(w, "warming up", http.StatusServiceUnavailable)
		return
	}
	if t.Paused {
		http.Error(w, "paused", http.StatusServiceUnavailable)
		return
	}

	r.Header.Del(TargetHeader)
	r.Header.Set(ActorHeader, t.Atespace+"/"+t.Actor)
	if t.ActorPort == 80 {
		h.plain.ServeHTTP(w, r)
		return
	}
	h.tunnel.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, t)))
}

// deny drops the request the way a NetworkPolicy would: no response.
func deny(r *http.Request, reason string) {
	slog.Info("denied", "from", r.RemoteAddr, "host", r.Host, "reason", reason)
	panic(http.ErrAbortHandler)
}

// resolve finds (namespace, service, port). Gateway traffic names it in
// TargetHeader; everything else names the Service in Host, as any in-cluster
// client dialing the Service does.
func (h *Handler) resolve(r *http.Request, c Caller) (string, string, int32, bool) {
	if v := r.Header.Get(TargetHeader); v != "" {
		if !c.SystemComponent {
			return "", "", 0, false
		}
		return parseTargetHeader(v)
	}
	host, portStr, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, portStr = r.Host, "80"
	}
	port, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil {
		return "", "", 0, false
	}
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	svc, ns := labels[0], c.Namespace
	switch {
	case len(labels) == 1:
	case len(labels) == 2, len(labels) == 3 && labels[2] == "svc":
		ns = labels[1]
	case len(labels) > 3 && labels[2] == "svc" && strings.Join(labels[3:], ".") == h.cfg.ClusterDomain:
		ns = labels[1]
	default:
		return "", "", 0, false
	}
	return ns, svc, int32(port), ns != "" && svc != ""
}

func parseTargetHeader(v string) (string, string, int32, bool) {
	ns, rest, ok := strings.Cut(v, "/")
	if !ok {
		return "", "", 0, false
	}
	svc, portStr, ok := strings.Cut(rest, ":")
	if !ok {
		return "", "", 0, false
	}
	port, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil || ns == "" || svc == "" {
		return "", "", 0, false
	}
	return ns, svc, int32(port), true
}

// allowed mirrors the rules of OpenChoreo's openchoreo-<component> policy.
func (h *Handler) allowed(c Caller, t Target) bool {
	switch {
	case c.Kind == CallerEgress:
		return true
	case c.Namespace == t.Namespace:
		return true
	case c.SystemComponent && (slices.Contains(t.Visibility, v1alpha1.VisibilityInternal) ||
		slices.Contains(t.Visibility, v1alpha1.VisibilityExternal)):
		return true
	case slices.Contains(t.Visibility, v1alpha1.VisibilityNamespace):
		cl, tl := h.dir.NamespaceLabels(c.Namespace), h.dir.NamespaceLabels(t.Namespace)
		return tl[LabelNamespace] != "" && cl[LabelNamespace] == tl[LabelNamespace] &&
			tl[LabelEnvironment] != "" && cl[LabelEnvironment] == tl[LabelEnvironment]
	}
	return false
}

// dialTunnel opens a CONNECT tunnel through the router to the actor port
// encoded in addr ("<atespace>.<actor>:<port>").
func (h *Handler) dialTunnel(ctx context.Context, network, addr string) (net.Conn, error) {
	hostPart, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	atespace, actor, ok := strings.Cut(hostPart, ".")
	if !ok {
		return nil, fmt.Errorf("bad tunnel address %q", addr)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, h.cfg.RouterConnect)
	if err != nil {
		return nil, err
	}
	authority := net.JoinHostPort(actor, port)
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s: %s/%s\r\n\r\n", authority, authority, ActorHeader, atespace, actor)
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("router CONNECT: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func proxyError(w http.ResponseWriter, _ *http.Request, err error) {
	http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
}
