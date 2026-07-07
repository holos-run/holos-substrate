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
// The manager starts, serves health and Prometheus metrics endpoints, registers
// the scheme, and runs the Organization (HOL-1311) and Repository (HOL-1312)
// reconcilers.
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

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	securityv1alpha1 "github.com/holos-run/holos-paas/api/security/v1alpha1"
	keycloakcontroller "github.com/holos-run/holos-paas/internal/controller/keycloak"
	quaycontroller "github.com/holos-run/holos-paas/internal/controller/quay"
)

// RBAC the manager needs once the reconcilers land (HOL-1311, HOL-1312): full
// control over the quay.holos.run resources and their status, read-only access
// to security.holos.run ReferenceGrants (declarative cross-namespace policy the
// authorization helper lists — no reconciler, so get;list;watch only), read
// access to the credential and webhook-URL Secrets the specs reference, and
// leader-election + event-recording permissions. The markers live here so
// controller-gen emits config/deploy/holos-controller/rbac/role.yaml; the
// reconcilers added in later phases use exactly these verbs.
//
// Secrets are limited to get and create (never list/watch): the reconcilers
// resolve only the specific credential/webhook-URL Secrets a CR names, via the
// manager's non-caching APIReader, so the controller never enumerates Secrets
// cluster-wide. The create verb (HOL-1347) is for the KeycloakClient reconciler's
// generate-once delivery of a confidential client's secret into the Secret named
// by the resource's spec.secretRef; it create-if-absent only and never updates an
// existing Secret, so the delivered value stays stable (the Runtime Secret
// Handling guardrail). The delivered Secret is owner-referenced for GC, not
// Owns-watched, so no cluster-wide Secret informer is needed.
//
// +kubebuilder:rbac:groups=quay.holos.run,resources=organizations;repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=quay.holos.run,resources=organizations/status;repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=quay.holos.run,resources=organizations/finalizers;repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=keycloak.holos.run,resources=keycloakinstances;keycloakgroups;keycloakgroupmemberships;keycloakusers;keycloakclients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keycloak.holos.run,resources=keycloakinstances/status;keycloakgroups/status;keycloakgroupmemberships/status;keycloakusers/status;keycloakclients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keycloak.holos.run,resources=keycloakinstances/finalizers;keycloakgroups/finalizers;keycloakgroupmemberships/finalizers;keycloakusers/finalizers;keycloakclients/finalizers,verbs=update
// +kubebuilder:rbac:groups=security.holos.run,resources=referencegrants,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// version is the build version stamped into the binary at link time with
// -ldflags "-X main.version=<v>". The Makefile sets it from `git describe`,
// following the leading-v vMAJOR.MINOR.PATCH tag convention (e.g. v0.2.0); it
// defaults to "dev" for an un-stamped `go build`. It is logged once at manager
// startup so an operator can correlate a running controller with the source
// revision it was built from.
var version = "dev"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	// Register the Kubernetes core types and the quay.holos.run,
	// keycloak.holos.run, and security.holos.run API groups so the manager's
	// client and cache can serve all of them. The keycloak.holos.run group is
	// registered-but-unreconciled in this phase (HOL-1344) — its reconcilers land
	// in later phases (HOL-1346, HOL-1347) — so the CRDs install and the binary
	// compiles with the group in the scheme. The security group's ReferenceGrant
	// has no reconciler — it is declarative policy the authorization helper reads —
	// but it must be in the scheme for the client to list it.
	utilruntimeMust(clientgoscheme.AddToScheme(scheme))
	utilruntimeMust(quayv1alpha1.AddToScheme(scheme))
	utilruntimeMust(keycloakv1alpha1.AddToScheme(scheme))
	utilruntimeMust(securityv1alpha1.AddToScheme(scheme))
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
		// metrics-reader RBAC granted in config/deploy/holos-controller/rbac.
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

	// Register the Organization reconciler (HOL-1311). It resolves the Quay
	// superuser credential from the controller's own namespace — read from
	// POD_NAMESPACE (the downward API, set by the deployment in HOL-1313) and
	// otherwise defaulting to holos-controller — so leaving Namespace empty lets
	// SetupWithManager pick the env up. The Repository reconciler is wired in
	// HOL-1312.
	if err := (&quaycontroller.OrganizationReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Organization")
		os.Exit(1)
	}

	// Register the Repository reconciler (HOL-1312). Like the Organization
	// reconciler it resolves the Quay credential from the controller's own
	// namespace (POD_NAMESPACE / holos-controller default), so leaving Namespace
	// empty lets SetupWithManager pick the env up. It additionally resolves the
	// repo_push webhook URL from a Secret in each Repository's own namespace.
	if err := (&quaycontroller.RepositoryReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Repository")
		os.Exit(1)
	}

	// Register the keycloak.holos.run KeycloakInstance and KeycloakGroup
	// reconcilers (HOL-1346). Like the quay reconcilers they resolve the Keycloak
	// admin credential from the controller's own namespace (POD_NAMESPACE /
	// holos-controller default), so leaving Namespace empty lets SetupWithManager
	// pick the env up. The KeycloakUser and KeycloakClient reconcilers are wired in
	// HOL-1347.
	if err := (&keycloakcontroller.InstanceReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KeycloakInstance")
		os.Exit(1)
	}

	if err := (&keycloakcontroller.GroupReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KeycloakGroup")
		os.Exit(1)
	}

	if err := (&keycloakcontroller.MembershipReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KeycloakGroupMembership")
		os.Exit(1)
	}

	// Register the KeycloakUser reconciler (HOL-1347). It pre-provisions a user by
	// email and configures the IdP federated-identity link for first-login
	// auto-link, resolving the Keycloak admin credential from the controller's own
	// namespace like the reconcilers above.
	if err := (&keycloakcontroller.UserReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KeycloakUser")
		os.Exit(1)
	}

	// Register the KeycloakClient reconciler (HOL-1347). It manages the URL-named
	// OIDC client, its client roles, and the client-role→groups-claim mapper, and
	// for a confidential client delivers the generated secret (generate-once) to a
	// Secret in the resource's namespace — the controller's first Secret-creating
	// reconciler, hence the additional secrets create RBAC verb above.
	if err := (&keycloakcontroller.ClientReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KeycloakClient")
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

	setupLog.Info("starting manager", "version", version)
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
