// Command holos-controller is the Holos Controller (ADR-18): the in-cluster
// controller-runtime manager that reconciles the quay.holos.run API group's
// Organization and Repository custom resources (ADR-19) against the in-cluster
// Quay registry, filling the data-plane gaps the upstream Quay operator leaves
// open.
//
// Unlike holos-paas (a Fisk multi-service CLI, ADR-17), this binary uses the
// conventional kubebuilder wiring — the standard library flag package plus
// controller-runtime's zap log flags — because that is the idiom every
// controller-runtime manager, RBAC scaffold, and operator tutorial assumes, and
// it keeps the manager legible to the kubebuilder toolchain. Fisk is for the
// user-facing CLI, not the manager process.
//
// This phase (HOL-1309) is a scaffold: the manager starts, serves health and
// Prometheus metrics endpoints, and registers the scheme, but carries no
// reconcile logic. The Organization and Repository reconcilers are wired in
// later phases (HOL-1311, HOL-1312).
package main

import (
	"crypto/tls"
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
)

// RBAC the manager needs once the reconcilers land (HOL-1311, HOL-1312): full
// control over the quay.holos.run resources and their status, read access to
// the credential and webhook-URL Secrets the specs reference, and leader-election
// + event-recording permissions. The markers live here so controller-gen emits
// config/rbac/role.yaml; the reconcilers added in later phases use exactly these
// verbs.
//
// Secrets are intentionally limited to get (not list/watch): the reconcilers
// resolve only the specific credential/webhook-URL Secrets a CR names, via the
// manager's non-caching APIReader, so the controller never enumerates Secrets
// cluster-wide.
//
// +kubebuilder:rbac:groups=quay.holos.run,resources=organizations;repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=quay.holos.run,resources=organizations/status;repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=quay.holos.run,resources=organizations/finalizers;repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	// Register the Kubernetes core types and the quay.holos.run API group so
	// the manager's client and cache can serve both.
	utilruntimeMust(clientgoscheme.AddToScheme(scheme))
	utilruntimeMust(quayv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var secureMetrics bool

	// Conventional kubebuilder flags. The metrics endpoint defaults to :8080
	// (Prometheus scrape) and the health/readiness probe endpoint to :8081.
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the Prometheus metrics endpoint binds to. Set to 0 to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the health and readiness probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Ensures only one active manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set, serve the metrics endpoint over HTTPS with authn/authz.")

	// zap.Options{Development: false} selects the production encoder, which is
	// JSON. Binding the options to the flag set makes JSON the default
	// encoding while still allowing operators to override via --zap-* flags;
	// the JSON output is suitable for Datadog/LGTM ingestion (AC #5).
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	metricsOptions := metricsserver.Options{
		BindAddress: metricsAddr,
	}
	if secureMetrics {
		// Serve metrics over HTTPS and gate scrapes behind Kubernetes
		// authentication and authorization (TokenReview + SubjectAccessReview),
		// so the documented "authn/authz" actually holds. This requires the
		// metrics-reader RBAC granted in config/rbac.
		metricsOptions.SecureServing = true
		metricsOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
		metricsOptions.TLSOpts = []func(*tls.Config){}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "holos-controller.holos.run",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Reconcilers are registered here in later phases (HOL-1311, HOL-1312).
	// This scaffold starts an otherwise-empty manager that serves health and
	// metrics endpoints.

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

// utilruntimeMust panics if err is non-nil. It mirrors
// k8s.io/apimachinery/pkg/util/runtime.Must for the small set of scheme
// registrations in init, keeping the import surface of this package minimal.
func utilruntimeMust(err error) {
	if err != nil {
		panic(err)
	}
}
