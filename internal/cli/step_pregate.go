package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// `step pregate` — the detached pre-gate run (gates-trust §7.6.2 PG6).
//
// The engine launches it; an operator has no ordinary reason to. It is hidden
// rather than absent because the engine's launch IS a re-execution of this
// binary, and a verb that exists only as an argv the engine composes still has
// to parse, open the store, and resolve the project exactly as every other
// verb does — PersistentPreRunE is the one place that happens.

var stepPreGateCmd = &cobra.Command{
	Use:    "pregate STEP-N --target SHA",
	Hidden: true,
	Short:  "Run a step's pre-gates against a target commit, outside any claim",
	Long: `Run a step's pre-gates against one target commit, ahead of the claim
that will consume them.

The engine launches this verb detached — its own session, no terminal, no
pipes — the moment a step's pre-gate target is first recorded: a writer's
completion, a re-pin, or an integration annotation. The target is rebuilt
from the object database into a throwaway checkout, every pre-gate runs under
its trust entry's own timeout rather than the claim's 60s budget, and the
rows are recorded keyed to the target. A later ` + "`step claim`" + ` that
resolves the same target serves those rows instead of running the gate
inside its budget; one that resolves a different target, or arrives while
this run is still going, runs the gate itself exactly as before.

One run per step and target at a time: a second invocation finding the first
still running exits without measuring. A run whose target the step no longer
resolves, or whose step is already over, measures nothing and says so.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := stepArg(args[0])
		if err != nil {
			return err
		}
		target, _ := cmd.Flags().GetString("target")
		if target == "" {
			return cmdErr(
				fmt.Errorf("--target is required: the full commit id the pre-gates measure"),
				output.ErrValidation)
		}

		run, err := engine.NewEngine().RunDetachedPreGates(conn, id, target, model.NowMS())
		if err != nil {
			return stepErr(err, stepLabel(id))
		}
		w.Success(run, fmt.Sprintf("%s pre-gates against %.12s: %s",
			model.FormatStepID(id), target, run.Outcome))
		return nil
	},
}

func init() {
	stepPreGateCmd.Flags().String("target", "",
		"Full commit id the pre-gates measure (required)")
}

// startDetachedProcess starts a prepared child and releases the handle, so
// the parent never waits on it. A variable so a test can inspect the command
// the launcher composed without spawning this binary.
var startDetachedProcess = func(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// launchDetachedPreGates is the launcher Execute installs on
// engine.LaunchDetachedPreGates: it re-executes this binary as
// `step pregate STEP-N --target SHA`, detached.
//
// DETACHED MEANS THREE THINGS, and each closes a specific failure:
//
//   - Its own session (Setsid). The harness that runs the launching verb kills
//     that verb's process group at its tool timeout, and a child in the same
//     group would die with it — mid-measurement, leaving the next claim on
//     its budgeted path again.
//   - All three standard streams on the null device. A child holding the
//     parent's stdout pipe keeps that pipe open until the gate finishes, so
//     the harness waits on `docket step complete` for the whole test suite —
//     the pipe-hang internal/exec guards against, reproduced one layer up.
//   - The handle released. The parent exits as soon as its own verb is done
//     and never waits on the child.
//
// The child inherits the parent's working directory and environment on
// purpose: that is what makes its config.Resolve land on the same store and
// the same project the parent's did.
func launchDetachedPreGates(stepID int, targetSHA string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "step", "pregate", model.FormatStepID(stepID),
		"--target", targetSHA)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Stdin, Stdout, and Stderr stay nil: os/exec connects each to the null
	// device, which is the detachment the second bullet above requires.
	return startDetachedProcess(cmd)
}
