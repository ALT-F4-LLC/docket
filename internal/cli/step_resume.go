package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// `step resume` — the detached completion saga (DKT-3977).
//
// `step record` launches it the moment the artifact records, so the gates and
// routing run outside the recording turn. Hidden for the reason `step pregate`
// is: the engine's launch is a re-execution of this binary, and the verb has to
// parse, open the store, and resolve the project like any other.

var stepResumeCmd = &cobra.Command{
	Use:    "resume STEP-N",
	Hidden: true,
	Short:  "Run a recorded step's gates and routing, then drive the run's lifecycles",
	Long: `Resume the completion saga of a step whose artifact already recorded.

The engine launches this verb detached — its own session, no terminal, no
pipes — when ` + "`step record`" + ` commits the artifact. It holds a per-step lock,
runs the remaining gates and routing, then readies any vote or action step the
completion unblocked. A second invocation finding the lock held exits without
running. It takes no token: the step is engine-owned from the recording.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := stepArg(args[0])
		if err != nil {
			return err
		}
		run, err := engine.NewEngine().RunDetachedSaga(conn, id, model.NowMS())
		if err != nil {
			return stepErr(err, stepLabel(id))
		}
		w.Success(run, fmt.Sprintf("%s saga: %s", model.FormatStepID(id), run.Outcome))
		return nil
	},
}

// launchDetachedSaga is the launcher Execute installs on
// engine.LaunchDetachedSaga: it re-executes this binary as
// `step resume STEP-N`, detached the way launchDetachedPreGates documents
// (own session, null streams, handle released, inherited cwd and environment).
// The argv is the binary and the step id: no shell, no token, no passthrough.
func launchDetachedSaga(stepID int) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "step", "resume", model.FormatStepID(stepID))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return startDetachedProcess(cmd)
}
