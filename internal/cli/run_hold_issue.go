package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var runHoldIssueCmd = &cobra.Command{
	Use:   "hold-issue RUN-N DKT-N --reason R",
	Short: "Park every pending step of one issue for the operator",
	Long: `Park every PENDING step of one issue in a run waiting-human (park class
` + "`held`" + `), ready and staged alike, so no dispatch offers any of its rows until
the operator releases them.

An operator's ruling to hold an issue otherwise lives only in the conductor's
launch filter, and a staged row cannot be filtered by what is ready. The held
steps are listed under the manifest's ` + "`held`" + ` with their reason, and each releases
like any park: ` + "`step resolve STEP-N --as retry`" + ` returns it to the pool.

The issue's steps must not be in flight: a claimed step is force-reaped first
(` + "`step reap`" + `). --reason is required and becomes each step's park reason; put the
operator's note id in it. One step-held event is recorded per step.

Like step hold, it requires the RUN'S CONDUCTOR CAPABILITY via DOCKET_TOKEN or
stdin; a run activated before the capability existed asks for none.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunHoldIssue(cmd, args, getWriter(cmd))
	},
}

func runRunHoldIssue(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)
	runID, err := model.ParseRunID(args[0])
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	issueID, err := model.ParseID(args[1])
	if err != nil {
		return cmdErr(fmt.Errorf("invalid issue ID %q: %w", args[1], err), output.ErrValidation)
	}
	if _, err := db.GetRun(conn, runID); errors.Is(err, db.ErrRunNotFound) {
		return cmdErr(fmt.Errorf("run %s not found", model.FormatRunID(runID)), output.ErrNotFound)
	} else if err != nil {
		return cmdErr(err, output.ErrGeneral)
	}
	reason, _ := cmd.Flags().GetString("reason")
	by, err := rulingBy()
	if err != nil {
		return err
	}
	held, err := engine.HoldIssue(conn, runID, issueID, engine.HoldOptions{
		Reason: reason, By: by, Token: conductorToken(conn, runID, os.Stdin),
		NowMS: model.NowMS(),
	})
	if err != nil {
		return runErr(err)
	}
	steps := make([]string, 0, len(held))
	for _, id := range held {
		steps = append(steps, model.FormatStepID(id))
	}
	w.Success(map[string]any{
		"run": model.FormatRunID(runID), "issue": model.FormatID(issueID),
		"held": steps, "reason": reason,
	}, fmt.Sprintf("Held %d step(s) of %s in %s: %v",
		len(steps), model.FormatID(issueID), model.FormatRunID(runID), steps))
	return nil
}

func init() {
	runHoldIssueCmd.Flags().String("reason", "",
		"Why the issue is held for the operator; name the operator's note (required)")
	runCmd.AddCommand(runHoldIssueCmd)
}
