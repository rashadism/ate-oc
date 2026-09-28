package main

import (
	"flag"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/ateclient"
	"github.com/rashadism/oc-substrate/internal/controller"
	"github.com/rashadism/oc-substrate/internal/registry"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(substratev1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr, storageLocation string
	var leaderElect bool
	var retentionTTL, orphanScanInterval time.Duration
	var frontDoorNamespace, frontDoorSelector string
	var frontDoorPort int
	var egressInterval time.Duration
	var clusterCIDRs, blockedCIDRs, egressNamespace string
	var registryConfig, insecureRegistries string
	var ate ateclient.Config
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "Metrics endpoint address; 0 disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe endpoint address.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&ate.Target, "ateapi-target", "dns:///api.ate-system.svc:443", "ateapi gRPC target.")
	flag.StringVar(&ate.ServerName, "ateapi-server-name", "api.ate-system.svc", "ateapi serving certificate DNS name.")
	flag.StringVar(&ate.CredentialBundle, "ateapi-credential-bundle", "/run/podidentity/credential-bundle.pem",
		"Client key and certificate chain for ateapi mTLS.")
	flag.StringVar(&ate.CAFile, "ateapi-ca-file", "/run/servicedns-ca/trust-bundle.pem",
		"CA bundle for the ateapi serving certificate.")
	flag.DurationVar(&retentionTTL, "retention-ttl", 30*24*time.Hour, "How long deleted components keep actor state.")
	flag.DurationVar(&orphanScanInterval, "orphan-scan-interval", 10*time.Minute, "Untracked-actor scan interval.")
	flag.StringVar(&frontDoorNamespace, "frontdoor-namespace", os.Getenv("POD_NAMESPACE"), "Front door namespace.")
	flag.StringVar(&frontDoorSelector, "frontdoor-selector", "app.kubernetes.io/name=oc-substrate-frontdoor",
		"Label selector for the front door pods.")
	flag.IntVar(&frontDoorPort, "frontdoor-port", 8080, "Front door proxy port.")
	flag.DurationVar(&egressInterval, "egress-sync-interval", 5*time.Second, "How often actor egress is recomputed.")
	flag.StringVar(&clusterCIDRs, "cluster-cidrs", "", "Extra pod/Service ranges, comma-separated.")
	flag.StringVar(&blockedCIDRs, "blocked-cidrs", "", "Ranges actors may never reach, comma-separated.")
	flag.StringVar(&egressNamespace, "egress-namespace", "ate-system", "Namespace of Substrate's egress gateway.")
	flag.StringVar(&registryConfig, "registry-config", "/etc/oc-substrate/registry/.dockerconfigjson",
		"Docker config with registry credentials for resolving image digests; optional.")
	flag.StringVar(&insecureRegistries, "insecure-registries", "", "Registries reached over plain HTTP, comma-separated.")
	flag.StringVar(&storageLocation, "storage-location", "", "Snapshot object-store prefix, per atespace.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	fdSelector, err := labels.Parse(frontDoorSelector)
	if err != nil || frontDoorNamespace == "" {
		setupLog.Error(err, "--frontdoor-namespace and a valid --frontdoor-selector are required")
		os.Exit(1)
	}
	extraRanges, err := parsePrefixes(clusterCIDRs)
	if err != nil {
		setupLog.Error(err, "invalid --cluster-cidrs")
		os.Exit(1)
	}
	blockedRanges, err := parsePrefixes(blockedCIDRs)
	if err != nil {
		setupLog.Error(err, "invalid --blocked-cidrs")
		os.Exit(1)
	}
	creds, err := registry.DockerConfig(registryConfig)
	if err != nil {
		setupLog.Error(err, "invalid --registry-config")
		os.Exit(1)
	}
	resolver := &registry.Resolver{
		Client: &http.Client{Timeout: 30 * time.Second}, Credentials: creds, Insecure: map[string]bool{},
	}
	for _, r := range strings.Split(insecureRegistries, ",") {
		if r = strings.TrimSpace(r); r != "" {
			resolver.Insecure[r] = true
		}
	}
	if storageLocation == "" {
		setupLog.Error(nil, "--storage-location is required")
		os.Exit(1)
	}
	ateClient, err := ateclient.Dial(ate)
	if err != nil {
		setupLog.Error(err, "unable to create ateapi client")
		os.Exit(1)
	}
	defer func() { _ = ateClient.Close() }()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "oc-substrate.substrate.openchoreo.dev",
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {Transform: controller.TrimPod},
		}},
		// Secrets and ConfigMaps are read one at a time, by name, from the
		// namespace an ActorTemplate CR names; nothing lists or watches them
		// cluster-wide, so they never populate the shared cache.
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}, &corev1.ConfigMap{}},
		}},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	actorTemplateR := &controller.ActorTemplateReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Ate: ateClient, StorageLocation: storageLocation, Images: resolver,
	}
	if err := actorTemplateR.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ActorTemplate")
		os.Exit(1)
	}
	actorR := &controller.ActorReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Ate: ateClient,
		Recorder: mgr.GetEventRecorder("oc-substrate"), RetentionTTL: retentionTTL, Now: time.Now,
	}
	if err := actorR.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Actor")
		os.Exit(1)
	}
	retainedR := &controller.RetainedActorReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Ate: ateClient, Now: time.Now,
	}
	if err := retainedR.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RetainedActor")
		os.Exit(1)
	}
	endpointsR := &controller.EndpointsReconciler{
		Client: mgr.GetClient(), FrontDoorNamespace: frontDoorNamespace,
		FrontDoorSelector: fdSelector, FrontDoorPort: int32(frontDoorPort),
	}
	if err := endpointsR.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Endpoints")
		os.Exit(1)
	}
	cellR := &controller.CellPolicyReconciler{
		Client: mgr.GetClient(), EgressNamespace: egressNamespace, EgressLabels: map[string]string{"app": "atenet-egress"},
	}
	if err := cellR.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CellPolicy")
		os.Exit(1)
	}
	if err := mgr.Add(&controller.EgressSyncer{
		Client: mgr.GetClient(), Ate: ateClient, Interval: egressInterval,
		ExtraClusterRanges: extraRanges, Blocked: blockedRanges,
	}); err != nil {
		setupLog.Error(err, "unable to add egress syncer")
		os.Exit(1)
	}
	if err := mgr.Add(&controller.OrphanScanner{
		Client: mgr.GetClient(), Ate: ateClient, Interval: orphanScanInterval, RetentionTTL: retentionTTL, Now: time.Now,
	}); err != nil {
		setupLog.Error(err, "unable to add orphan scanner")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func parsePrefixes(csv string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, c := range strings.Split(csv, ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
