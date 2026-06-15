package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/choria-io/fisk"
)

// deployHelpLong is the detailed help shown by `holos-paas deploy --help`.
const deployHelpLong = `Run the demo build-and-publish workflow (ADR-16) that hands a new release to
Kargo.

Given an application container image reference (a tag or a digest), deploy:

  1. resolves the image to an immutable digest and renders the platform pinned
     to it (holos render platform --inject app_image=...);
  2. packages the rendered manifests with Kustomize into an OCI artifact and
     publishes (oras push) that artifact to the manifests repository in the
     in-cluster Quay registry; and
  3. prints the pushed artifact digest, which a Kargo Warehouse watches to
     create Freight and promote a Stage with argocd-update.

deploy is a thin, documented front end over scripts/publish, the canonical
client-side publish workflow, so the shell workflow stays the single source of
truth. It locates scripts/publish by walking up from the working directory;
override with --repo-root or --publish-script when running outside the repo.

Registry credentials are read from the environment, never flags, so they are
not exposed on the command line or in shell history:

  ORAS_USERNAME / ORAS_PASSWORD          push creds for the manifests repo
  ORAS_SRC_USERNAME / ORAS_SRC_PASSWORD  pull creds for the app image registry

See holos/docs/oci-publish-workflow.md for the full credential and transport
reference.`

// deployCheat is the task-oriented summary shown by `holos-paas cheat deploy`.
const deployCheat = `# Publish a release for Kargo to promote (ADR-16)
holos-paas deploy --app-image quay.holos.localhost/holos/holos-paas:v1.2.3

# Preview the underlying scripts/publish invocation without running it
holos-paas deploy --app-image quay.holos.localhost/holos/holos-paas:v1.2.3 --dry-run

# Publish to a non-default manifests repository
holos-paas deploy \
  --app-image quay.holos.localhost/holos/holos-paas@sha256:... \
  --manifests-repo quay.holos.localhost/holos/holos-paas-manifests`

// deployOptions holds the parsed deploy flags. It is the single source of truth
// for translating flags into a scripts/publish invocation; invocation() is pure
// so it can be exercised without touching the filesystem or a registry.
type deployOptions struct {
	appImage      string
	manifestsRepo string
	repoRoot      string
	publishScript string
	artifactTag   string
	forcePush     bool
	keepWorkdir   bool
	insecure      bool
	plainHTTP     bool
	dryRun        bool
}

// publishInvocation is a resolved scripts/publish call: the script path, its
// positional arguments, and the environment variables layered on top of the
// caller's environment.
type publishInvocation struct {
	Script string
	Args   []string
	Env    []string // KEY=value entries added to (and overriding) the process env.
}

// String renders the invocation as a copy-pasteable shell command for --dry-run.
// Each token is shell-quoted so a script path, image ref, or override value
// containing spaces or shell metacharacters round-trips correctly when pasted.
func (p publishInvocation) String() string {
	parts := make([]string, 0, len(p.Env)+len(p.Args)+1)
	for _, e := range p.Env {
		parts = append(parts, shellQuote(e))
	}
	parts = append(parts, shellQuote(p.Script))
	for _, a := range p.Args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuoteSafe is the set of characters that need no quoting in POSIX shell
// word context, including '=' and ',' so KEY=value env entries stay readable.
const shellQuoteSafe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_=:/.@%+-,"

// shellQuote returns s safe to paste as a single POSIX shell word. Tokens made
// only of unambiguous characters are returned unquoted; anything else is
// single-quoted with embedded single quotes escaped as '\”.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool { return !strings.ContainsRune(shellQuoteSafe, r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// invocation translates the options into a scripts/publish call. repoRoot must
// be resolved by the caller; invocation only joins paths and maps flags to
// scripts/publish's positional args and environment overrides.
func (o deployOptions) invocation() (publishInvocation, error) {
	if o.appImage == "" {
		return publishInvocation{}, errors.New("--app-image is required")
	}

	script := o.publishScript
	if script == "" {
		script = filepath.Join(o.repoRoot, "scripts", "publish")
	}

	inv := publishInvocation{Script: script, Args: []string{o.appImage}}
	if o.manifestsRepo != "" {
		inv.Args = append(inv.Args, o.manifestsRepo)
	}

	// Map boolean and value flags to the environment overrides scripts/publish
	// documents. Only non-zero options are emitted so the dry-run output and the
	// child environment stay minimal and predictable.
	if o.forcePush {
		inv.Env = append(inv.Env, "FORCE_PUSH=1")
	}
	if o.keepWorkdir {
		inv.Env = append(inv.Env, "KEEP_WORKDIR=1")
	}
	if o.insecure {
		inv.Env = append(inv.Env, "ORAS_INSECURE=1")
	}
	if o.plainHTTP {
		inv.Env = append(inv.Env, "ORAS_PLAIN_HTTP=1")
	}
	if o.artifactTag != "" {
		inv.Env = append(inv.Env, "ARTIFACT_TAG="+o.artifactTag)
	}

	return inv, nil
}

// findRepoRoot walks up from start until it finds a directory containing
// scripts/publish, the marker for the holos-paas repository root.
func findRepoRoot(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "publish")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate scripts/publish at or above %q; run from the repository or pass --repo-root", start)
		}
		dir = parent
	}
}

// run resolves the repository root (unless an explicit script path was given),
// builds the scripts/publish invocation, and either prints it (--dry-run) or
// executes it, streaming child output through stdout and stderr.
func (o *deployOptions) run(ctx context.Context, stdout, stderr io.Writer) error {
	if o.publishScript == "" && o.repoRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("determining working directory: %w", err)
		}
		root, err := findRepoRoot(wd)
		if err != nil {
			return err
		}
		o.repoRoot = root
	}

	inv, err := o.invocation()
	if err != nil {
		return err
	}

	if o.dryRun {
		_, err := fmt.Fprintln(stdout, inv.String())
		return err
	}

	cmd := exec.CommandContext(ctx, inv.Script, inv.Args...)
	cmd.Env = append(os.Environ(), inv.Env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", inv.Script, err)
	}
	return nil
}

// registerDeploy adds the deploy subcommand and its flags to app. Every flag
// carries a documented help string, and each value-taking flag a PlaceHolder;
// this function is the template for adding further Fisk commands (see
// docs/cli-guardrails.md).
func registerDeploy(app *fisk.Application) {
	opts := &deployOptions{}

	cmd := app.Command("deploy", "Publish a release's rendered manifests for Kargo to promote (ADR-16)").
		Action(func(_ *fisk.ParseContext) error {
			return opts.run(context.Background(), os.Stdout, os.Stderr)
		})
	cmd.HelpLong(deployHelpLong)
	cmd.Cheat("deploy", deployCheat)

	cmd.Flag("app-image", "Application container image to deploy, as a tag (registry/app:tag) or digest (registry/app@sha256:...)").
		PlaceHolder("REF").Required().StringVar(&opts.appImage)
	cmd.Flag("manifests-repo", "OCI repository to push the rendered-manifests artifact to; defaults to scripts/publish's in-cluster Quay default").
		PlaceHolder("REPO").StringVar(&opts.manifestsRepo)
	cmd.Flag("repo-root", "holos-paas repository root holding scripts/publish; defaults to walking up from the working directory").
		PlaceHolder("DIR").StringVar(&opts.repoRoot)
	cmd.Flag("publish-script", "Path to the publish script to run; defaults to <repo-root>/scripts/publish").
		PlaceHolder("PATH").StringVar(&opts.publishScript)
	cmd.Flag("artifact-tag", "Override the input-addressed artifact tag; defeats idempotency, use with care").
		PlaceHolder("TAG").StringVar(&opts.artifactTag)
	cmd.Flag("force-push", "Push even when the input-addressed tag already exists, overwriting it").
		UnNegatableBoolVar(&opts.forcePush)
	cmd.Flag("keep-workdir", "Keep the temporary working directory for debugging instead of removing it").
		UnNegatableBoolVar(&opts.keepWorkdir)
	cmd.Flag("insecure", "Skip TLS verification against the registry (ORAS_INSECURE); for mkcert-signed in-cluster Quay").
		UnNegatableBoolVar(&opts.insecure)
	cmd.Flag("plain-http", "Use plain HTTP for the registry (ORAS_PLAIN_HTTP); for a localhost dev registry").
		UnNegatableBoolVar(&opts.plainHTTP)
	cmd.Flag("dry-run", "Print the resolved scripts/publish invocation without executing it").
		UnNegatableBoolVar(&opts.dryRun)
}
