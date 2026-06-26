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
// The scaffold (HOL-1385) starts the manager, serves health and Prometheus
// metrics endpoints, and runs the ext_authz gRPC server as a manager.Runnable
// whose Check is a stub always-Denied (HTTP 403) response. HOL-1386 adds the
// authenticator.holos.run/v1alpha1 API group (the Backend CRD) and registers it
// in the manager's scheme; the OIDC validation, CEL mapping, and impersonation
// output that consume a Backend land in HOL-1387..HOL-1389.
package main

import (
	"crypto/tls"
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
	"github.com/holos-run/holos-paas/internal/authenticator"
	authenticatorctrl "github.com/holos-run/holos-paas/internal/controller/authenticator"
)

// RBAC the authenticator needs: leader-election leases and event recording, plus
// read/status access to the authenticator.holos.run Backend CRs the reconciler
// watches (HOL-1387). These markers generate the authenticator's own RBAC role
// (holos-authenticator-role) via `make authenticator-manifests`, scoped to the
// authenticator packages — distinct from the controller's
// holos-controller-manager-role.
//
// Secret access is granted only as a namespace-scoped `get` on the authorizer's
// own namespace, not cluster-wide. The Check data path (HOL-1388) resolves each
// backend's privileged impersonator credential Secret from the authorizer's
// namespace via the manager's APIReader. The marker below carries a `namespace`
// field so `make authenticator-manifests` emits a namespaced Role (not a
// ClusterRole) in the authorizer's namespace — a cluster-wide `secrets` verb
// would over-grant cluster-wide Secret reads. controller-gen emits only the Role;
// the RoleBinding that binds it to the authorizer's ServiceAccount is part of the
// deploy component in HOL-1389 (the platform wiring), so the Role is inert until
// that phase lands. The Backend reconciler (HOL-1387) still never reads Secrets;
// only this data path does.
//
// The authorizer also mints short-lived impersonator tokens for a ServiceAccount
// in its own namespace via the TokenRequest API (spec.serviceAccountRef, HOL-1400)
// when a Backend selects that credential source instead of a Secret. TokenRequest
// is a create on the serviceaccounts/token sub-resource; the marker is namespaced
// (a Role, not a ClusterRole) so the authorizer can mint only in its own
// namespace, and is further scoped with resourceNames to ONLY the shipped default
// impersonator ServiceAccount (holos-authenticator-impersonator,
// DefaultImpersonatorServiceAccountName). Without the resourceNames scope a
// namespace-wide create on serviceaccounts/token would let any admitted Backend's
// spec.serviceAccountRef.name select a token for ANY ServiceAccount in the
// authorizer namespace — turning that field into a privilege selector were the
// namespace ever to hold another privileged SA. RBAC resourceNames works for the
// create verb here because the token sub-resource is created on a NAMED
// ServiceAccount (like pods/exec), so a mint for any other name is denied
// fail-closed. A Backend needing a different impersonator SA is a deliberate
// platform decision that must widen this Role, not a tenant-selectable default.
// The token is minted WITHOUT a BoundObjectRef (matching `kubectl create token`),
// so create is the only verb needed — no get on serviceaccounts. The default
// impersonator ServiceAccount itself is shipped by the deploy component in the next
// phase (HOL-1401).
//
// +kubebuilder:rbac:groups="",namespace=holos-authenticator,resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",namespace=holos-authenticator,resources=serviceaccounts/token,resourceNames=holos-authenticator-impersonator,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
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

	// Scope the manager's cache (and therefore the Backend reconciler's watch) to
	// the authorizer's own namespace. A Backend is a PLATFORM-owned object: its
	// privileged impersonator credential always resolves from this same namespace
	// (resolveImpersonatorToken reads AuthorizerNamespace(), never the Backend's
	// namespace), and the platform namespace registry denies tenant Argo CD
	// projects this namespace as a destination. Watching cluster-wide would let a
	// tenant create a Backend in its OWN namespace and have the manager reconcile
	// it — performing controller-side OIDC discovery against a tenant-chosen
	// issuerURL (SSRF) and registering a host route into the shared store the
	// ext_authz path serves. Restricting the cache to AuthorizerNamespace() means
	// tenant-namespace Backends are never cached, reconciled, or served, closing
	// both vectors at the wiring layer regardless of the AppProject
	// namespaceResourceWhitelist. It also matches the namespace-scoped `get` on
	// Secrets the authorizer's RBAC grants.
	authorizerNamespace := authenticator.AuthorizerNamespace()
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "holos-authenticator.holos.run",
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				authorizerNamespace: {},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// The host-keyed store of ready backends is constructed once here and shared:
	// the BackendReconciler is its sole writer (registering backends as they pass
	// OIDC discovery + CEL compilation) and the ext_authz gRPC server reads it by
	// the request's :authority/Host. Injecting one instance into both is how the
	// reconciler's reconciled state reaches the data path without an import cycle
	// (both depend on internal/authenticator, not on each other).
	//
	// The store is process-local. Because the ext_authz gRPC server must answer
	// Envoy on every replica (NeedLeaderElection=false), the reconciler that fills
	// the store also runs on every replica (it sets NeedLeaderElection=false in
	// SetupWithManager) rather than only on the elected leader — otherwise a
	// non-leader replica would serve from an empty store and deny every request.
	// With --leader-elect this means each replica independently reconciles the same
	// Backends into its own store; there is no external system to coordinate, so
	// that is safe.
	store := authenticator.NewStore()

	// Register the Backend reconciler: it watches Backend CRs, performs OIDC
	// discovery, compiles the group-mapping CEL expression, sets Gateway-API
	// status, and maintains the shared store (HOL-1387).
	if err := (&authenticatorctrl.BackendReconciler{
		Client: mgr.GetClient(),
		Store:  store,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up Backend reconciler")
		os.Exit(1)
	}

	// Register the ext_authz gRPC server as a manager.Runnable so it shares the
	// manager's lifecycle and leader-election context. It does not require
	// leadership (NeedLeaderElection returns false): every replica must answer
	// Envoy, not only the elected leader. The Check (HOL-1388) routes by Host
	// through the shared store, validates the caller's OIDC token, and returns
	// Kubernetes impersonation headers, resolving each backend's privileged
	// credential from the authorizer's own namespace — either reading the credential
	// Secret via the manager's APIReader (a non-caching reader) or minting a
	// short-lived ServiceAccount token via the manager's writable client (the
	// TokenRequest create the APIReader cannot perform), per the backend's
	// serviceAccountRef/credentialsSecretRef source (HOL-1400).
	grpcServer := &authenticator.GRPCServer{
		Addr: grpcAddr,
		Check: authenticator.NewCheckServer(
			store,
			mgr.GetAPIReader(),
			mgr.GetClient(),
			authorizerNamespace,
			ctrl.Log.WithName("ext-authz"),
		),
		Log: ctrl.Log.WithName("grpc-server"),
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
