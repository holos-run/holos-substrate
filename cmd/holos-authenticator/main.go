// Command holos-authenticator is the Holos Authenticator (ADR-23): an Istio/Envoy
// gRPC external authorizer (envoy.service.auth.v3.Authorization) that fronts one
// or more Kubernetes API server backends — in-cluster or external — and (in later
// phases) authenticates end users via OIDC, maps token claims to Kubernetes
// groups via a CEL expression, and returns Kubernetes impersonation headers so
// Envoy forwards each request to the API server with no other reverse proxy in
// the path.
//
// Like holos-controller (ADR-18) and unlike holos-paas (a Fisk multi-service
// CLI, ADR-17), this binary uses the conventional kubebuilder wiring — the
// standard library flag package plus controller-runtime's zap log flags —
// because that is the idiom every controller-runtime manager and operator
// tutorial assumes, and it keeps the manager legible to the kubebuilder
// toolchain. Fisk is for the user-facing CLI, not the manager process.
//
// This phase (HOL-1385) ships only the scaffold: the manager starts, serves
// health and Prometheus metrics endpoints, registers the core Kubernetes scheme,
// and runs the ext_authz gRPC server as a manager.Runnable whose Check is a stub
// always-Denied (HTTP 403) response. The authenticator.holos.run API group, OIDC
// validation, CEL mapping, and impersonation output land in HOL-1386..HOL-1389.
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

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
	"github.com/holos-run/holos-paas/internal/authenticator"
)

// RBAC the authenticator needs: leader-election leases and event recording, plus
// access to the authenticator.holos.run Backend CRs it watches and the credential
// Secrets they reference. These markers generate the authenticator's own RBAC role
// (holos-authenticator-role) via `make authenticator-manifests`, scoped to the
// authenticator packages — distinct from the controller's
// holos-controller-manager-role. The Backend reconciler that consumes the
// backends verbs lands in HOL-1387; the read access is declared with the CRD here.
//
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=authenticator.holos.run,resources=backends,verbs=get;list;watch
// +kubebuilder:rbac:groups=authenticator.holos.run,resources=backends/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=authenticator.holos.run,resources=backends/finalizers,verbs=update

// version is the build version stamped into the binary at link time with
// -ldflags "-X main.version=<v>". The Makefile sets it from `git describe`,
// following the leading-v vMAJOR.MINOR.PATCH tag convention (e.g. v0.2.0); it
// defaults to "dev" for an un-stamped `go build`. It is logged once at manager
// startup so an operator can correlate a running authenticator with the source
// revision it was built from.
var version = "dev"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	// Register the Kubernetes core types so the manager's client and cache can
	// serve them, plus the authenticator.holos.run group (HOL-1386) whose Backend
	// CRs configure the authorizer's API server backends. Unlike holos-controller
	// this binary registers no quay.holos.run or keycloak.holos.run groups.
	utilruntimeMust(clientgoscheme.AddToScheme(scheme))
	utilruntimeMust(authenticatorv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var grpcAddr string
	var enableLeaderElection bool
	var secureMetrics bool

	// Conventional kubebuilder flags. The metrics endpoint defaults to :8080
	// (Prometheus scrape) and the health/readiness probe endpoint to :8081. The
	// ext_authz gRPC server Envoy connects to defaults to :9000.
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the Prometheus metrics endpoint binds to. Set to 0 to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the health and readiness probe endpoint binds to.")
	flag.StringVar(&grpcAddr, "grpc-bind-address", ":9000",
		"The address the Envoy ext_authz gRPC server binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Ensures only one active manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set, serve the metrics endpoint over HTTPS with authn/authz.")

	// zap.Options{Development: false} selects the production encoder, which is
	// JSON. Binding the options to the flag set makes JSON the default encoding
	// while still allowing operators to override via --zap-* flags; the JSON
	// output is suitable for Datadog/LGTM ingestion.
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
		LeaderElectionID:       "holos-authenticator.holos.run",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Register the ext_authz gRPC server as a manager.Runnable so it shares the
	// manager's lifecycle and leader-election context. It does not require
	// leadership (NeedLeaderElection returns false): every replica must answer
	// Envoy, not only the elected leader. The Check is the scaffold stub
	// (always-Denied 403); HOL-1388 replaces it with the real decision.
	grpcServer := &authenticator.GRPCServer{
		Addr:  grpcAddr,
		Check: authenticator.NewCheckServer(ctrl.Log.WithName("ext-authz")),
		Log:   ctrl.Log.WithName("grpc-server"),
	}
	if err := mgr.Add(grpcServer); err != nil {
		setupLog.Error(err, "unable to add ext_authz gRPC server to manager")
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

	setupLog.Info("starting manager", "version", version, "grpc-bind-address", grpcAddr)
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
