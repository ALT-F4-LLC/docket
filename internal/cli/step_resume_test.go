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
	stubDetachedChildDeath(t, false)
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

// TestDetachedSagaLauncherRefusesWhereTheChildWouldDie: inside a bubblewrap
// pid namespace the child dies with the recording command, so the launcher
// starts nothing and returns an error, which sends the engine inline.
func TestDetachedSagaLauncherRefusesWhereTheChildWouldDie(t *testing.T) {
	stubDetachedChildDeath(t, true)
	started := false
	prev := startDetachedProcess
	startDetachedProcess = func(*exec.Cmd) error {
		started = true
		return nil
	}
	t.Cleanup(func() { startDetachedProcess = prev })

	if err := launchDetachedSaga(7); err == nil {
		t.Error("launchDetachedSaga returned nil for a child that would die with the command")
	}
	if started {
		t.Error("the launcher started a child the sandbox would kill")
	}
}

// stubDetachedChildDeath fixes the sandbox probe's answer for one test.
func stubDetachedChildDeath(t *testing.T, dies bool) {
	t.Helper()
	prev := detachedChildDiesWithCommand
	detachedChildDiesWithCommand = func() bool { return dies }
	t.Cleanup(func() { detachedChildDiesWithCommand = prev })
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
