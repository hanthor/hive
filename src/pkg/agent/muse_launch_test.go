package agent

import (
	"strings"
	"testing"
)

// muse launched BARE before this contract existed: it had no case in
// backendLaunchCmd and fell through to `default`, so the pane stopped on
//
//	Do you trust this workspace?  Workspace: /data/agents/<agent>
//
// which nothing is attached to answer. The watchdog then killed and restarted
// the pane indefinitely while /api/status still reported state=running and
// busy=working. These tests pin the three properties that keep that from
// recurring.

func museLaunch(model string) string {
	return backendLaunchCmd("muse", model, "muse", false)
}

// The trust prompt is the specific thing that hung the pane, and
// --trust-workspace does not persist, so it has to be on EVERY launch.
func TestMuseLaunchAlwaysAnswersTheWorkspaceTrustPrompt(t *testing.T) {
	for _, model := range []string{"", "muse-spark-1.2-contributor"} {
		cmd := museLaunch(model)
		if !strings.Contains(cmd, "--trust-workspace") {
			t.Fatalf("muse launch (model=%q) omits --trust-workspace, so the pane blocks on the trust prompt: %q", model, cmd)
		}
		if !strings.Contains(cmd, "--approval-mode never") {
			t.Errorf("muse launch (model=%q) omits the unattended approval policy: %q", model, cmd)
		}
		if !strings.Contains(cmd, "--user-input-auto-resolve") {
			t.Errorf("muse launch (model=%q) omits --user-input-auto-resolve: %q", model, cmd)
		}
	}
}

// --yolo would also clear the prompt, and is exactly what must NOT be used:
// muse documents it as disabling approval AND the sandbox, whereas the whole
// reason muse is an acceptable rung is that it keeps its own OS sandbox.
func TestMuseLaunchNeverDisablesItsOwnSandbox(t *testing.T) {
	for _, model := range []string{"", "muse-spark-1.2-contributor"} {
		cmd := museLaunch(model)
		for _, banned := range []string{"--yolo", "--disable-sandbox", "--disable-approval"} {
			if strings.Contains(cmd, banned) {
				t.Fatalf("muse launch (model=%q) uses %s, which gives up muse's OS sandbox: %q", model, banned, cmd)
			}
		}
	}
}

// A configured model must actually reach the CLI — the failure agy's case
// already documents, where the model is silently ignored and the agent runs on
// something other than the rung rotation placed it on.
func TestMuseLaunchPassesTheConfiguredModel(t *testing.T) {
	cmd := museLaunch("muse-spark-1.2-contributor")
	if !strings.Contains(cmd, "--model muse-spark-1.2-contributor") {
		t.Fatalf("configured model did not reach the muse launch: %q", cmd)
	}
	// An empty model must not produce a bare, valueless --model flag.
	if bare := museLaunch(""); strings.Contains(bare, "--model") {
		t.Fatalf("no model configured, but --model was still emitted: %q", bare)
	}
}
