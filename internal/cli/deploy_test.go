package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDeployInvocation(t *testing.T) {
	tests := []struct {
		name     string
		opts     deployOptions
		wantArgs []string
		wantEnv  []string
		wantErr  bool
	}{
		{
			name:    "app image required",
			opts:    deployOptions{repoRoot: "/repo"},
			wantErr: true,
		},
		{
			name:     "minimal",
			opts:     deployOptions{repoRoot: "/repo", appImage: "quay.example/app:v1"},
			wantArgs: []string{"quay.example/app:v1"},
		},
		{
			name: "manifests repo becomes second positional arg",
			opts: deployOptions{
				repoRoot:      "/repo",
				appImage:      "quay.example/app:v1",
				manifestsRepo: "quay.example/app-manifests",
			},
			wantArgs: []string{"quay.example/app:v1", "quay.example/app-manifests"},
		},
		{
			name: "boolean and value flags map to env overrides",
			opts: deployOptions{
				repoRoot:    "/repo",
				appImage:    "quay.example/app:v1",
				artifactTag: "render-abc",
				forcePush:   true,
				keepWorkdir: true,
				insecure:    true,
				plainHTTP:   true,
			},
			wantArgs: []string{"quay.example/app:v1"},
			wantEnv: []string{
				"FORCE_PUSH=1",
				"KEEP_WORKDIR=1",
				"ORAS_INSECURE=1",
				"ORAS_PLAIN_HTTP=1",
				"ARTIFACT_TAG=render-abc",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := tt.opts.invocation()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			wantScript := filepath.Join("/repo", "scripts", "publish")
			if inv.Script != wantScript {
				t.Errorf("script = %q, want %q", inv.Script, wantScript)
			}
			if !equalStrings(inv.Args, tt.wantArgs) {
				t.Errorf("args = %v, want %v", inv.Args, tt.wantArgs)
			}
			if !equalStrings(inv.Env, tt.wantEnv) {
				t.Errorf("env = %v, want %v", inv.Env, tt.wantEnv)
			}
		})
	}
}

func TestDeployInvocationPublishScriptOverride(t *testing.T) {
	opts := deployOptions{
		repoRoot:      "/repo",
		publishScript: "/custom/publish",
		appImage:      "app:v1",
	}
	inv, err := opts.invocation()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.Script != "/custom/publish" {
		t.Errorf("script = %q, want /custom/publish (explicit override wins over repo-root)", inv.Script)
	}
}

func TestPublishInvocationString(t *testing.T) {
	inv := publishInvocation{
		Script: "/repo/scripts/publish",
		Args:   []string{"app:v1", "repo"},
		Env:    []string{"FORCE_PUSH=1"},
	}
	want := "FORCE_PUSH=1 /repo/scripts/publish app:v1 repo"
	if got := inv.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestPublishInvocationStringQuotesSpecialChars(t *testing.T) {
	inv := publishInvocation{
		Script: "/repo dir/scripts/publish",
		Args:   []string{"app:v1"},
		Env:    []string{"ARTIFACT_TAG=has space"},
	}
	want := `'ARTIFACT_TAG=has space' '/repo dir/scripts/publish' app:v1`
	if got := inv.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "''"},
		{"safe-value", "safe-value"},
		{"KEY=value", "KEY=value"},
		{"registry/app@sha256:abc", "registry/app@sha256:abc"},
		{"has space", "'has space'"},
		{"it's", `'it'\''s'`},
		{"a;rm -rf", "'a;rm -rf'"},
	}
	for _, tt := range tests {
		if got := shellQuote(tt.in); got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFindRepoRoot(t *testing.T) {
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "publish"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findRepoRoot(nested)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// macOS resolves t.TempDir() through /private; compare resolved paths.
	gotResolved, _ := filepath.EvalSymlinks(got)
	rootResolved, _ := filepath.EvalSymlinks(root)
	if gotResolved != rootResolved {
		t.Errorf("findRepoRoot = %q, want %q", gotResolved, rootResolved)
	}
}

func TestFindRepoRootNotFound(t *testing.T) {
	// A fresh temp dir with no scripts/publish anywhere up to the filesystem root.
	dir := t.TempDir()
	if _, err := findRepoRoot(dir); err == nil {
		t.Fatal("expected an error when scripts/publish is absent")
	}
}

func TestDeployRunDryRun(t *testing.T) {
	opts := &deployOptions{
		repoRoot: "/repo",
		appImage: "quay.example/app:v1",
		dryRun:   true,
	}
	var out, errOut bytes.Buffer
	if err := opts.run(context.Background(), &out, &errOut); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.TrimSpace(out.String())
	want := filepath.Join("/repo", "scripts", "publish") + " quay.example/app:v1"
	if got != want {
		t.Errorf("dry-run output = %q, want %q", got, want)
	}
	if errOut.Len() != 0 {
		t.Errorf("expected empty stderr, got %q", errOut.String())
	}
}

func TestDeployRunExecutesPublishScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell publish stub is POSIX-only")
	}
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	// A stub publish script that echoes its first arg and the FORCE_PUSH env var
	// so the test can confirm args and env reach the child process.
	stub := "#!/bin/sh\necho \"app=$1 force=$FORCE_PUSH\"\n"
	if err := os.WriteFile(filepath.Join(scripts, "publish"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := &deployOptions{
		repoRoot:  root,
		appImage:  "app:v1",
		forcePush: true,
	}
	var out, errOut bytes.Buffer
	if err := opts.run(context.Background(), &out, &errOut); err != nil {
		t.Fatalf("unexpected error: %v\nstderr: %s", err, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "app=app:v1 force=1" {
		t.Errorf("publish stub output = %q, want %q", got, "app=app:v1 force=1")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
