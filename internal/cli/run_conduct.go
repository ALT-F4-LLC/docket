package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket run conduct` — the conductor capability's re-mint (DKT-2465).
//
// The capability itself is minted by a run's first activation and returned in
// that response; this verb exists for the session that does not hold it — a
// fresh session driving a run another one activated, or resuming one whose
// conductor died with the token — and for a legacy run activated before the
// capability existed.

var runConductCmd = &cobra.Command{
	Use:   "conduct RUN-N",
	Short: "Take a run's conductor seat, minting its capability token",
	Long: `Take (or re-take) a run's conductor seat.

The seven operator verbs — step approve, step reject, step resolve, step reap,
run pause, run resume, run abandon — require the run's CONDUCTOR CAPABILITY: a
token the run's first activation returned once, supplied on each of them via
DOCKET_TOKEN or stdin (never argv). This verb mints a fresh one and prints it
ONCE. Any standing token is retired by the mint, so the session that held it
is refused at its next ruling and must take the seat again.

It is deliberately open to any caller with repository access — a run whose
conductor session died must not be un-pausable forever — which makes the seat
TAMPER-EVIDENT rather than tamper-proof: the ` + "`conductor-seated`" + ` event
records who took it and from where, and the refusal the displaced conductor
sees is the signal. A harness that keys its callers keeps executors off THIS
verb, and the engine keeps them off the other seven.

A run activated before the capability existed asks for no token until it is
conducted; conducting it binds it. A terminal run is refused: there is
nothing left to conduct.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunConduct(cmd, args, getWriter(cmd))
	},
}

// conductResponse is the verb's wire shape: the run, the token, and whether a
// prior capability was retired.
type conductResponse struct {
	Run     string `json:"run"`
	Token   string `json:"token"`
	Rotated bool   `json:"rotated"`
}

func runRunConduct(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(args[0])
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	by, err := rulingBy()
	if err != nil {
		return err
	}

	result, err := engine.ConductRun(conn, runID, engine.ConductOptions{
		By: by, NowMS: model.NowMS(),
	})
	if err != nil {
		return runErr(err)
	}

	message := fmt.Sprintf("Took the conductor seat on %s", result.Run.Ref())
	if result.Rotated {
		message += "; the previous capability is retired"
	}
	if w.JSONMode {
		w.Success(conductResponse{
			Run: result.Run.Ref(), Token: result.Token, Rotated: result.Rotated,
		}, "")
		return nil
	}
	// The token goes on its own line, never inside the message — the
	// discipline `step claim` and `issue claim` established: a token echoed
	// into a status line is copied along with it into transcripts and logs.
	w.Success(nil, message)
	fmt.Fprintln(w.Stdout, result.Token)
	return nil
}

func init() {
	runCmd.AddCommand(runConductCmd)
}
