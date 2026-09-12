package cli

import (
	"encoding/json"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket gate status` — DKT-1286.

var gateCmd = &cobra.Command{
	Use:   "gate",
	Short: "Read a gate step's decision state",
}

// gateStatusResult is GateStatus's wire format: the JSON tags are the
// envelope's contract, so they are pinned here rather than left to a struct
// tag on the engine value.
type gateStatusResult struct {
	StepStatus   string          `json:"step_status"`
	Proposal     string          `json:"proposal,omitempty"`
	Outcome      string          `json:"outcome"`
	Tally        *gateTallyJSON  `json:"tally,omitempty"`
	Seats        []gateSeatJSON  `json:"seats,omitempty"`
	MissingSeats []string        `json:"missing_seats,omitempty"`
	Target       *gateTargetJSON `json:"target,omitempty"`
}

type gateTallyJSON struct {
	WeightedScore *float64 `json:"weighted_score"`
	Threshold     float64  `json:"threshold"`
}

type gateSeatJSON struct {
	Voter   string `json:"voter"`
	Cast    bool   `json:"cast"`
	Verdict string `json:"verdict,omitempty"`
}

type gateTargetJSON struct {
	SHA      string `json:"sha"`
	Worktree string `json:"worktree"`
}

func (r gateStatusResult) MarshalJSON() ([]byte, error) {
	type alias gateStatusResult
	a := alias(r)
	if a.MissingSeats == nil {
		a.MissingSeats = []string{}
	}
	return json.Marshal(a)
}

var gateStatusCmd = &cobra.Command{
	Use:   "status STEP-N",
	Short: "Outcome, tally and missing seats for one gate, in one call",
	Long: `Answer one gate step's whole decision state in a single small
envelope: whether it is decided, which way, and — on a vote gate — which
declared seats have not cast yet.

READ-ONLY, and it WRITES NOTHING — the same LoadStepView every other read verb
uses, plus the proposal reads a vote gate's seat roster needs.

  step_status    the step's effective status (` + "`step show`" + `'s own field)
  proposal       the vote this gate opened, absent on a human gate or a vote
                 gate whose proposal has not opened yet
  outcome        "approved" | "rejected" | "open" — a proposal retired
                 without a tally (` + "`closed`" + `) reads as "open": no vote decided it
  tally          {weighted_score, threshold}, absent without a proposal
  seats          every DECLARED voter, whether they have cast, and their
                 verdict once they have — absent on a human gate
  missing_seats  the voters in ` + "`seats`" + ` who have not cast — always present as
                 (possibly empty) once a proposal exists
  target         {sha, worktree} the gate judges, absent when the step's
                 packet names none

This replaces separately probing ` + "`step show`" + `, ` + "`vote show`" + `, and re-deriving an
outcome from their fields — the exact scatter wave.js's gate:show, gate:record,
gate:tally and gate:outcome probes performed, each a relayed round trip.

STEP-N must be a ` + "`type=\"human\"`" + ` or ` + "`type=\"vote\"`" + ` step; any other kind has no
decision to report status for and is a VALIDATION_ERROR.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runGateStatus(cmd, args, getWriter(cmd))
	},
}

func runGateStatus(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := stepArg(args[0])
	if err != nil {
		return err
	}

	status, err := engine.GateStatus(conn, id, model.NowMS())
	if err != nil {
		return stepErr(err, stepLabel(id))
	}

	result := gateStatusResult{
		StepStatus:   status.StepStatus,
		Proposal:     status.Proposal,
		Outcome:      string(status.Outcome),
		MissingSeats: status.MissingSeats,
	}
	if status.Tally != nil {
		result.Tally = &gateTallyJSON{
			WeightedScore: status.Tally.WeightedScore, Threshold: status.Tally.Threshold,
		}
	}
	if status.Seats != nil {
		result.Seats = make([]gateSeatJSON, 0, len(status.Seats))
		for _, s := range status.Seats {
			result.Seats = append(result.Seats, gateSeatJSON{
				Voter: s.Voter, Cast: s.Cast, Verdict: s.Verdict,
			})
		}
	}
	if status.Target != nil {
		result.Target = &gateTargetJSON{SHA: status.Target.SHA, Worktree: status.Target.Worktree}
	}

	var message string
	if !w.JSONMode {
		message = renderGateStatus(result)
	}
	w.Success(result, message)
	return nil
}

func renderGateStatus(r gateStatusResult) string {
	line := "step: " + r.StepStatus + "; outcome: " + r.Outcome
	if r.Proposal != "" {
		line += "; proposal: " + r.Proposal
	}
	if len(r.MissingSeats) > 0 {
		line += "; missing seats: " + strings.Join(r.MissingSeats, ", ")
	}
	return line
}

func init() {
	gateCmd.AddCommand(gateStatusCmd)
	rootCmd.AddCommand(gateCmd)
}
