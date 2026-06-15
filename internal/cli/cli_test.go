package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/choria-io/fisk"
)

// TestNewRegistersDeploy confirms the root application builds and the deploy
// workflow command is registered alongside the cheat command.
func TestNewRegistersDeploy(t *testing.T) {
	app := New()
	if app.Model().Name != "holos-paas" {
		t.Errorf("app name = %q, want holos-paas", app.Model().Name)
	}

	var foundDeploy, foundCheat bool
	for _, c := range app.Model().Commands {
		switch c.Name {
		case "deploy":
			foundDeploy = true
		case "cheat":
			foundCheat = true
		}
	}
	if !foundDeploy {
		t.Error("deploy command not registered")
	}
	if !foundCheat {
		t.Error("cheat command not registered (WithCheats)")
	}
}

// TestDeployFlagsDocumented guards the convention that every deploy flag carries
// a non-empty help string, so the CLI stays self-documenting for agents.
func TestDeployFlagsDocumented(t *testing.T) {
	deploy := findCommand(t, New(), "deploy")
	if deploy.FlagGroupModel == nil {
		t.Fatal("deploy command has no flags")
	}
	for _, f := range deploy.Flags {
		if strings.TrimSpace(f.Help) == "" {
			t.Errorf("deploy flag --%s has no help string", f.Name)
		}
	}
}

// TestRunHelpExitsZero confirms --help is handled and yields a zero exit code
// through the testable Run path.
func TestRunHelpExitsZero(t *testing.T) {
	if code := Run([]string{"--help"}); code != 0 {
		t.Errorf("Run(--help) = %d, want 0", code)
	}
}

// TestRunHelpLLM confirms the Fisk LLM help path is wired and exits zero. This
// is the AI-agent-friendly output that motivates the Fisk adoption (ADR-17).
func TestRunHelpLLM(t *testing.T) {
	if code := Run([]string{"--help-llm"}); code != 0 {
		t.Errorf("Run(--help-llm) = %d, want 0", code)
	}
}

// TestRunUnknownCommand confirms an unknown command yields a non-zero exit code.
func TestRunUnknownCommand(t *testing.T) {
	app := New()
	var buf bytes.Buffer
	app.ErrorWriter(&buf)
	app.UsageWriter(&buf)
	code := 0
	app.Terminate(func(c int) { code = c })
	app.MustParseWithUsage([]string{"definitely-not-a-command"})
	if code == 0 {
		t.Errorf("expected non-zero exit for unknown command, got %d", code)
	}
}

func findCommand(t *testing.T, app *fisk.Application, name string) *fisk.CmdModel {
	t.Helper()
	for _, c := range app.Model().Commands {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("command %q not found", name)
	return nil
}
