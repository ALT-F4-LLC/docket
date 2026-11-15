package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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
` + "`dispatch close`" + ` records a ` + "`chain`" + ` deferral itself behind a predecessor a wave attempted,
and filing that one returns the engine's record instead of writing a second. The engine
misses a row behind a predecessor skipped before its claim, so keep filing every
` + "`chain`" + ` deferral.
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
	in := verb.RunFactAddInput{Run: ref, ConductorToken: stdinConductorToken}
	in.Kind, _ = cmd.Flags().GetString("kind")
	in.Reason, _ = cmd.Flags().GetString("reason")
	in.Proposal, _ = cmd.Flags().GetString("proposal")
	in.Voter, _ = cmd.Flags().GetString("voter")
	in.Step, _ = cmd.Flags().GetString("step")
	in.Cause, _ = cmd.Flags().GetString("cause")
	env, err := runRulingEnv(cmd, in.Validate)
	if err != nil {
		return err
	}

	res, err := verb.RunFactAdd(cmd.Context(), env, in)
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}
	fact := res.Fact
	w.Success(fact, fmt.Sprintf("Recorded %s on %s (seq %d): %s",
		fact.Kind, fact.Run, fact.Seq, fact.Reason))
	return nil
}

// runRulingEnv is rulingEnv for a run verb whose input checks outrank an
// unreadable working directory, as they did when the command parsed its flags
// before resolving the directory. validate is the input's Validate; it runs
// only when the directory is unreadable, so a readable one leaves the verb's
// own order, the project first, intact.
func runRulingEnv(cmd *cobra.Command, validate func() error) (verb.Env, error) {
	env, err := rulingEnv(cmd)
	if err != nil {
		if inputErr := validate(); inputErr != nil {
			return verb.Env{}, verbErr(inputErr)
		}
		return verb.Env{}, err
	}
	return env, nil
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
