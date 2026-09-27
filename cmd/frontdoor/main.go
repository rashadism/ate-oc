package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/frontdoor"
)

func main() {
	addr := flag.String("listen-address", ":8080", "Proxy listen address.")
	healthAddr := flag.String("health-address", ":8081", "Health endpoint address.")
	routerHTTP := flag.String("router-http", "atenet-router.ate-system.svc:80", "Substrate router plain HTTP address.")
	routerConnect := flag.String("router-connect", "atenet-router.ate-system.svc:8081", "Router CONNECT address.")
	clusterDomain := flag.String("cluster-domain", "cluster.local", "Cluster DNS domain.")
	egressNS := flag.String("egress-namespace", "ate-system", "Namespace of Substrate's egress gateway.")
	flag.Parse()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(substratev1alpha1.AddToScheme(scheme))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := ctrl.GetConfigOrDie()
	c, err := cache.New(cfg, cache.Options{
		Scheme:   scheme,
		ByObject: map[client.Object]cache.ByObject{&corev1.Pod{}: {Transform: frontdoor.TrimPod}},
	})
	if err != nil {
		fatal("create cache", err)
	}
	if err := frontdoor.Indexes(ctx, c); err != nil {
		fatal("index cache", err)
	}
	for _, o := range []client.Object{&corev1.Pod{}, &corev1.Namespace{}, &substratev1alpha1.Actor{}} {
		if _, err := c.GetInformer(ctx, o); err != nil {
			fatal("start informer", err)
		}
	}
	go func() {
		if err := c.Start(ctx); err != nil {
			fatal("cache stopped", err)
		}
	}()
	if !c.WaitForCacheSync(ctx) {
		fatal("cache sync", nil)
	}

	live, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fatal("create client", err)
	}
	dir := &frontdoor.ClusterDirectory{
		Reader: c, Live: live, LiveRetry: 2 * time.Second,
		EgressNamespace: *egressNS, EgressLabels: map[string]string{"app": "atenet-egress"},
	}
	proxy := &http.Server{
		Addr: *addr,
		Handler: frontdoor.New(dir, frontdoor.Config{
			RouterHTTP: *routerHTTP, RouterConnect: *routerConnect, ClusterDomain: *clusterDomain,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	health := &http.Server{
		Addr: *healthAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = health.ListenAndServe() }()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = proxy.Shutdown(shutdown)
		_ = health.Shutdown(shutdown)
	}()

	slog.Info("starting frontdoor", "addr", *addr)
	if err := proxy.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal("frontdoor stopped", err)
	}
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
