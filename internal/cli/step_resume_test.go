package cli

import (
	"os"
	"os/exec"
	"reflect"
	"testing"
)

// TestDetachedSagaLauncherComposesADetachedChild pins the argv of the hidden
// verb and the detachment `step pregate`'s launcher has: its own session, null
// streams, inherited working directory. The argv carries the step id and
// nothing else: no token, no shell, no caller input.
func TestDetachedSagaLauncherComposesADetachedChild(t *testing.T) {
	var captured *exec.Cmd
	prev := startDetachedProcess
	startDetachedProcess = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	t.Cleanup(func() { startDetachedProcess = prev })

	if err := launchDetachedSaga(7); err != nil {
		t.Fatalf("launchDetachedSaga: %v", err)
	}
	if captured == nil {
		t.Fatal("the launcher composed no command")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if want := []string{self, "step", "resume", "STEP-7"}; !reflect.DeepEqual(captured.Args, want) {
		t.Errorf("argv = %v, want %v", captured.Args, want)
	}
	if captured.SysProcAttr == nil || !captured.SysProcAttr.Setsid {
		t.Error("the child does not start its own session")
	}
	if captured.Stdin != nil || captured.Stdout != nil || captured.Stderr != nil {
		t.Error("the child inherits a standard stream")
	}
	if captured.Dir != "" {
		t.Errorf("the child's working directory is %q; it must inherit the parent's", captured.Dir)
	}
}

// TestStepResumeVerbIsHiddenAndRegistered: the verb is the engine's argv, not
// an operator's, and must resolve under `step`.
func TestStepResumeVerbIsHiddenAndRegistered(t *testing.T) {
	if !stepResumeCmd.Hidden {
		t.Error("step resume is listed; it is launched by the engine, not typed")
	}
	var found bool
	for _, sub := range stepCmd.Commands() {
		if sub == stepResumeCmd {
			found = true
		}
	}
	if !found {
		t.Error("step resume is not registered under `step`")
	}
}
