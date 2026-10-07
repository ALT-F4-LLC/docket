package cli

import (
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket run fact add` — DKT-2759. The conductor's channel for coordination
// facts only it observes, recorded as events a store-only retro can count.

var runFactCmd = &cobra.Command{
	Use:   "fact",
	Short: "Record coordination facts only the conductor observes",
	Long: `Record coordination facts that only the conductor driving a run observes,
so a retro reading the store can count them.

Two kinds exist. ` + "`vote-reseated`" + ` records a vote seat the conductor re-spawned;
` + "`step-deferred`" + ` records a step it held back, with the cause ` + "`budget`" + ` or ` + "`chain`" + `.
A hand-resolved cherry-pick has no kind here: its record is the ` + "`step-annotated`" + `
event that ` + "`step annotate --integrated-sha`" + ` writes.`,
}

var runFactAddCmd = &cobra.Command{
	Use:   "add RUN-N --kind (vote-reseated --proposal PROPOSAL-N --voter NAME | step-deferred --step STEP-N --cause budget|chain) --reason TEXT",
	Short: "Record one conductor-observed fact as an event",
	Long: `Record one conductor-observed fact against a run, as one event of the named
kind carrying the reason, the actor, and the working directory.

  --kind vote-reseated --proposal PROPOSAL-N --voter NAME
      A seat of the proposal was re-spawned. The proposal must exist and be
      linked to an issue with a step on the run.
  --kind step-deferred --step STEP-N --cause budget|chain
      The step was held back, on budget or behind a chain. The step must
      belong to the run.

On a run bound to a conductor capability this requires that capability, via
DOCKET_TOKEN or an owner-only file on stdin, never argv. A fact is an
observation rather than a ruling, so it takes no --authority.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunFactAdd(cmd, args[0], getWriter(cmd))
	},
}

func runRunFactAdd(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	kind, _ := cmd.Flags().GetString("kind")
	reason, _ := cmd.Flags().GetString("reason")
	voter, _ := cmd.Flags().GetString("voter")
	cause, _ := cmd.Flags().GetString("cause")

	opts := engine.RunFactOptions{
		RunID: runID, Kind: kind, Reason: reason, Voter: voter, Cause: cause,
		NowMS: model.NowMS(),
	}
	if p, _ := cmd.Flags().GetString("proposal"); p != "" {
		if opts.ProposalID, err = model.ParseProposalID(p); err != nil {
			return cmdErr(fmt.Errorf("invalid --proposal %q: %w", p, err), output.ErrValidation)
		}
	}
	if s, _ := cmd.Flags().GetString("step"); s != "" {
		if opts.StepID, err = model.ParseStepID(s); err != nil {
			return cmdErr(fmt.Errorf("invalid --step %q: %w", s, err), output.ErrValidation)
		}
	}
	if opts.By, err = rulingBy(); err != nil {
		return err
	}
	opts.Token = conductorToken(conn, runID, os.Stdin)

	fact, err := engine.RecordRunFact(conn, opts)
	if err != nil {
		return runErr(err)
	}
	w.Success(fact, fmt.Sprintf("Recorded %s on %s (seq %d): %s",
		fact.Kind, fact.Run, fact.Seq, fact.Reason))
	return nil
}

func init() {
	runFactAddCmd.Flags().String("kind", "",
		"The fact: vote-reseated or step-deferred")
	runFactAddCmd.Flags().String("reason", "", "Why it happened (required)")
	runFactAddCmd.Flags().String("proposal", "",
		"The proposal whose seat was re-seated (vote-reseated)")
	runFactAddCmd.Flags().String("voter", "",
		"The re-seated seat's voter name (vote-reseated)")
	runFactAddCmd.Flags().String("step", "", "The deferred step, STEP-N (step-deferred)")
	runFactAddCmd.Flags().String("cause", "", "Why the step was deferred: budget or chain (step-deferred)")

	runFactCmd.AddCommand(runFactAddCmd)
	runCmd.AddCommand(runFactCmd)
}
