package cli

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// The detached pre-gate launcher (gates-trust §7.6.2 PG6) re-executes this
// binary, so what it composes is asserted on the command itself, with the
// spawn replaced: a test that let it start would run the test suite again,
// detached, which is exactly the hazard that keeps the launcher out of every
// test binary's NewEngine.

// TestDetachedPreGateLauncherComposesADetachedChild pins the three facts that
// make the child detached: its own session, every standard stream on the null
// device, and the argv of the hidden verb with the full target.
func TestDetachedPreGateLauncherComposesADetachedChild(t *testing.T) {
	var captured *exec.Cmd
	prev := startDetachedProcess
	startDetachedProcess = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	t.Cleanup(func() { startDetachedProcess = prev })

	sha := strings.Repeat("a", 40)
	if err := launchDetachedPreGates(7, sha); err != nil {
		t.Fatalf("launchDetachedPreGates: %v", err)
	}
	if captured == nil {
		t.Fatal("the launcher composed no command")
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	want := []string{self, "step", "pregate", "STEP-7", "--target", sha}
	if !reflect.DeepEqual(captured.Args, want) {
		t.Errorf("argv = %v, want %v", captured.Args, want)
	}
	if captured.SysProcAttr == nil || !captured.SysProcAttr.Setsid {
		t.Error("the child does not start its own session; the launching verb's " +
			"process group would take it down at the harness's tool timeout")
	}
	if captured.Stdin != nil || captured.Stdout != nil || captured.Stderr != nil {
		t.Error("the child inherits a standard stream; a pipe it holds open makes " +
			"the launching verb wait for the whole gate")
	}
	if captured.Dir != "" {
		t.Errorf("the child's working directory is %q; it must inherit the "+
			"parent's so config.Resolve lands on the same store", captured.Dir)
	}
}

// TestStepPreGateVerbIsHiddenAndRequiresATarget: the verb is the engine's,
// not an operator's, and a run with no target has nothing to key its rows to.
func TestStepPreGateVerbIsHiddenAndRequiresATarget(t *testing.T) {
	if !stepPreGateCmd.Hidden {
		t.Error("step pregate is listed; it is launched by the engine, not typed")
	}
	if stepPreGateCmd.Flags().Lookup("target") == nil {
		t.Fatal("step pregate declares no --target flag")
	}
	var found bool
	for _, sub := range stepCmd.Commands() {
		if sub == stepPreGateCmd {
			found = true
		}
	}
	if !found {
		t.Error("step pregate is not registered under `step`; the launcher's argv would not resolve")
	}
}
