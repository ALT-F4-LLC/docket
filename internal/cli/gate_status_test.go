package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// `docket gate status` — DKT-1286.

// TestGateStatusRefusesAnUnknownStep drives the real command wiring end to
// end: cobra arg parsing, stepArg, and stepErr's NOT_FOUND mapping, without
// needing a full vote-step fixture.
func TestGateStatusRefusesAnUnknownStep(t *testing.T) {
	conn := newTestDB(t)
	w, _ := bufWriter(false)

	err := runGateStatus(cmdWithDB(conn), []string{"STEP-1"}, w)
	if err == nil {
		t.Fatal("gate status answered for a step that does not exist")
	}
	var ce *CmdError
	if !asCmdErr(err, &ce) || ce.Code != output.ErrNotFound {
		t.Errorf("error = %v, want a NOT_FOUND CmdError", err)
	}
}

// TestGateStatusEnvelopeStaysUnder1KB is AC2: a 5-seat proposal's envelope
// fits under 1KB, so a relay agent copies it verbatim without truncating.
func TestGateStatusEnvelopeStaysUnder1KB(t *testing.T) {
	score := 0.83
	result := gateStatusResult{
		StepStatus: "gated",
		Proposal:   "DKT-V4242",
		Outcome:    "open",
		Tally:      &gateTallyJSON{WeightedScore: &score, Threshold: 0.6},
		Target: &gateTargetJSON{
			SHA:      "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
			Worktree: "/home/operator/work/docket/run-142/issue-8901/worktree",
		},
	}
	voters := []string{"seat-alpha", "seat-bravo", "seat-charlie", "seat-delta", "seat-echo"}
	for i, voter := range voters {
		seat := gateSeatJSON{Voter: voter}
		if i < 3 {
			seat.Cast = true
			seat.Verdict = string(model.VerdictApprove)
		} else {
			result.MissingSeats = append(result.MissingSeats, voter)
		}
		result.Seats = append(result.Seats, seat)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshaling the envelope: %v", err)
	}
	if len(raw) >= 1024 {
		t.Errorf("envelope is %d bytes, want under 1KB: %s", len(raw), raw)
	}

	// AC3: every field the contract names is present on the wire.
	for _, want := range []string{
		`"step_status"`, `"proposal"`, `"outcome"`, `"tally"`, `"weighted_score"`,
		`"threshold"`, `"seats"`, `"voter"`, `"cast"`, `"verdict"`,
		`"missing_seats"`, `"target"`, `"sha"`, `"worktree"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("envelope %s is missing %s", raw, want)
		}
	}
}
