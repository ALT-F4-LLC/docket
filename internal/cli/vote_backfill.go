package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var voteBackfillUsageCmd = &cobra.Command{
	Use:   "backfill-usage <id>",
	Short: "Record seat spend a relay measured after the casts landed",
	Long: `Record vote-seat usage a relay measured but the seats could not report
themselves (DKT-115).

` + "`vote cast --usage`" + ` is the seat's OWN report at cast time. A relay that
measures panel cost from its transcripts afterwards had no ledger path at all —
tribunal seats carry a proposal id, never a step id, so ` + "`dispatch\nbackfill-usage`" + ` could not receive them and governance spend stayed invisible.

Two forms, one per invocation:

  --voter NAME --unit U --quantity Q      repeatable, one triple per row
  --from-json PATH                        a JSON array; "-" reads stdin

    [{"voter": "tribunal-security", "unit": "output_tokens", "quantity": 48211}, ...]

THE WHOLE BATCH IS ONE TRANSACTION. Rows attach to each seat's CAST: a seat
that never cast is refused by name, because usage on a vote nobody cast is
spend on a decision that did not happen.

` + "`--source`" + ` defaults to "backfilled" and is free text — recorded on every
row, so a relay's reconstruction stays distinguishable from the seats' own
cast-time reports forever.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runVoteBackfillUsage(cmd, args, getWriter(cmd))
	},
}

// voteBackfillOutcome is what the verb reports: the proposal, the rows
// written, the seats they cover, and the source they carry.
type voteBackfillOutcome struct {
	Proposal string `json:"proposal"`
	Rows     int    `json:"rows"`
	Seats    int    `json:"seats"`
	Source   string `json:"source"`
}

func runVoteBackfillUsage(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	proposalID, err := model.ParseProposalID(args[0])
	if err != nil {
		return cmdErr(fmt.Errorf("invalid proposal ID: %w", err), output.ErrValidation)
	}

	rows, err := voteBackfillRows(cmd)
	if err != nil {
		return err
	}
	source, _ := cmd.Flags().GetString("source")

	if err := engine.NewEngine().BackfillVoteUsage(
		conn, proposalID, rows, source, model.NowMS()); err != nil {
		return runErr(err)
	}

	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		seen[r.Voter] = true
	}
	if source == "" {
		source = engine.UsageSourceBackfilled
	}
	outcome := &voteBackfillOutcome{
		Proposal: model.FormatProposalID(proposalID), Rows: len(rows),
		Seats: len(seen), Source: source,
	}

	var message string
	if !w.JSONMode {
		message = fmt.Sprintf("Back-filled %d usage row(s) across %d seat(s) "+
			"of %s as %q", outcome.Rows, outcome.Seats, outcome.Proposal,
			outcome.Source)
	}
	w.Success(outcome, message)
	return nil
}

// voteBackfillRows assembles the batch from whichever form the caller used —
// the same mutually-exclusive split the step back-fill makes, and for the same
// reason: two sources of truth for one batch have no way to say which wins.
func voteBackfillRows(cmd *cobra.Command) ([]engine.VoteBackfillRow, error) {
	fromJSON, _ := cmd.Flags().GetString("from-json")
	voters, _ := cmd.Flags().GetStringSlice("voter")

	if fromJSON != "" && len(voters) > 0 {
		return nil, cmdErr(fmt.Errorf(
			"--from-json and --voter are two forms of the same batch; pass one"),
			output.ErrValidation)
	}
	if fromJSON != "" {
		return voteBackfillRowsFromJSON(cmd, fromJSON)
	}
	return voteBackfillRowsFromFlags(cmd, voters)
}

// voteBackfillRowsFromFlags pairs the repeatable triples positionally: the Nth
// --voter takes the Nth --unit and the Nth --quantity. A mismatched count is
// refused rather than zip-truncated.
func voteBackfillRowsFromFlags(cmd *cobra.Command, voters []string) ([]engine.VoteBackfillRow, error) {
	units, _ := cmd.Flags().GetStringSlice("unit")
	quantities, _ := cmd.Flags().GetFloat64Slice("quantity")

	if len(voters) == 0 {
		return nil, cmdErr(fmt.Errorf(
			"pass --voter with its --unit and --quantity, or --from-json"),
			output.ErrValidation)
	}
	if len(units) != len(voters) || len(quantities) != len(voters) {
		return nil, cmdErr(fmt.Errorf(
			"got %d --voter, %d --unit, and %d --quantity; each row needs all "+
				"three", len(voters), len(units), len(quantities)),
			output.ErrValidation)
	}

	out := make([]engine.VoteBackfillRow, 0, len(voters))
	for i, voter := range voters {
		out = append(out, engine.VoteBackfillRow{
			Voter: voter, Unit: units[i], Quantity: quantities[i],
		})
	}
	return out, nil
}

// voteBackfillRowsFromJSON reads the batch form.
func voteBackfillRowsFromJSON(cmd *cobra.Command, path string) ([]engine.VoteBackfillRow, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, cmdErr(
				fmt.Errorf("reading the back-fill batch from stdin: %w", err),
				output.ErrGeneral)
		}
	} else {
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, cmdErr(
				fmt.Errorf("reading the back-fill batch from %s: %w", path, err),
				output.ErrNotFound)
		}
	}

	var wire []struct {
		Voter    string  `json:"voter"`
		Unit     string  `json:"unit"`
		Quantity float64 `json:"quantity"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, cmdErr(fmt.Errorf(
			"reading the back-fill batch: %w — it is a JSON array of "+
				`{"voter","unit","quantity"}`, err), output.ErrValidation)
	}

	out := make([]engine.VoteBackfillRow, 0, len(wire))
	for _, r := range wire {
		out = append(out, engine.VoteBackfillRow{
			Voter: r.Voter, Unit: r.Unit, Quantity: r.Quantity,
		})
	}
	return out, nil
}

func init() {
	voteBackfillUsageCmd.Flags().StringSlice("voter", nil,
		"Seat whose usage is being recorded (repeatable)")
	voteBackfillUsageCmd.Flags().StringSlice("unit", nil,
		"Unit for the matching --voter (repeatable); core has no default unit")
	voteBackfillUsageCmd.Flags().Float64Slice("quantity", nil,
		"Quantity for the matching --voter and --unit (repeatable)")
	voteBackfillUsageCmd.Flags().String("from-json", "",
		`JSON array of {"voter","unit","quantity"}; "-" reads stdin`)
	voteBackfillUsageCmd.Flags().String("source", "",
		`Who measured it (default "backfilled"); recorded on every row`)
	voteCmd.AddCommand(voteBackfillUsageCmd)
}
