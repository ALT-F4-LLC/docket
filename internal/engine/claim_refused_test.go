package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// claimRefusedFixtureSrc has two write-class steps under max 1 (headroom), a
// step waiting on another (not ready), and a human step (unclaimable).
const claimRefusedFixtureSrc = `
[pipeline]
name = "claim-refused-fixture"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 1 }

[[step]]
name = "one"
executor = "w"
class = "write"
emits = "change-summary"
after = []

[[step]]
name = "two"
executor = "w"
class = "write"
emits = "change-summary"
after = []

[[step]]
name = "later"
executor = "w"
emits = "change-summary"
after = ["one"]

[[step]]
name = "ask"
type = "human"
on_fail = "skip"
after = []
`

// claimRefusedEvents decodes a run's claim-refused events.
func claimRefusedEvents(t *testing.T, conn *sql.DB, runID int) []map[string]any {
	t.Helper()
	page, err := ListEvents(conn, EventQuery{RunID: runID, Kind: EventClaimRefused})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var out []map[string]any
	for _, e := range page.Events {
		var data map[string]any
		testsupport.Must(t, json.Unmarshal(e.Data, &data), "decoding: %v", nil)
		if e.Step == "" {
			t.Errorf("claim-refused event %d names no step instance", e.Seq)
		}
		out = append(out, data)
	}
	return out
}

func claimRefusedRun(t *testing.T, conn *sql.DB) int {
	t.Helper()
	registerSource(t, conn, []byte(claimRefusedFixtureSrc), "claim-refused.toml")
	issue := createIssue(t, conn, "claim refused", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run.ID
}

// TestClaimRefusalsWriteAnEvent is DKT-2776: every CONFLICT-coded refusal of
// `step claim` writes exactly one claim-refused event carrying the run, the
// step, the requesting owner, and the refusal reason.
func TestClaimRefusalsWriteAnEvent(t *testing.T) {
	type attempt struct {
		runID, stepID int
		claim         func() error
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, conn *sql.DB) attempt
	}{
		{"unclaimable kind", func(t *testing.T, conn *sql.DB) attempt {
			runID := claimRefusedRun(t, conn)
			id := stepIDByInstance(t, conn, "ask@0")
			return attempt{runID, id, func() error {
				_, err := ClaimStep(conn, id, ClaimOptions{Owner: "worker", NowMS: nowMS})
				return err
			}}
		}},
		{"not ready", func(t *testing.T, conn *sql.DB) attempt {
			runID := claimRefusedRun(t, conn)
			id := stepIDByInstance(t, conn, "later@0")
			return attempt{runID, id, func() error {
				_, err := ClaimStep(conn, id, ClaimOptions{Owner: "worker", NowMS: nowMS})
				return err
			}}
		}},
		{"no headroom", func(t *testing.T, conn *sql.DB) attempt {
			runID := claimRefusedRun(t, conn)
			claimInstance(t, conn, "one@0", nowMS)
			id := stepIDByInstance(t, conn, "two@0")
			return attempt{runID, id, func() error {
				_, err := ClaimStep(conn, id, ClaimOptions{Owner: "worker", NowMS: nowMS})
				return err
			}}
		}},
		{"lost CAS", func(t *testing.T, conn *sql.DB) attempt {
			runID := claimRefusedRun(t, conn)
			id := stepIDByInstance(t, conn, "one@0")
			claimHookBeforeCAS = func(tx *sql.Tx, stepID int) {
				_, err := tx.Exec(`UPDATE steps SET owner = 'rival', token_hash = 'rival',
					expires_ms = ?, status = 'claimed' WHERE id = ?`, nowMS+3_600_000, stepID)
				testsupport.Must(t, err, "the rival's claim: %v", err)
			}
			t.Cleanup(func() { claimHookBeforeCAS = nil })
			return attempt{runID, id, func() error {
				_, err := ClaimStep(conn, id, ClaimOptions{Owner: "worker", NowMS: nowMS})
				return err
			}}
		}},
		{"lost while pre-gates ran", func(t *testing.T, conn *sql.DB) attempt {
			run, _ := activatedRun(t, conn)
			e := testEngine()
			id := advanceToVerify(t, conn, e)
			claimHookBeforeRefresh = func(tx *sql.Tx, stepID int) {
				_, err := tx.Exec(`UPDATE steps SET token_hash = 'rival' WHERE id = ?`, stepID)
				testsupport.Must(t, err, "the rival's claim: %v", err)
			}
			t.Cleanup(func() { claimHookBeforeRefresh = nil })
			return attempt{run.ID, id, func() error {
				_, err := e.ClaimStepWithGates(conn, id, ClaimOptions{Owner: "worker", NowMS: nowMS})
				return err
			}}
		}},
		{"budget", func(t *testing.T, conn *sql.DB) attempt {
			runID, _ := budgetRun(t, conn, 0)
			cost := expectedCostOf(t, conn, "implement@0")
			execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cost/2, runID)
			id := stepIDByInstance(t, conn, "implement@0")
			return attempt{runID, id, func() error {
				_, err := ClaimStep(conn, id, ClaimOptions{Owner: "worker", NowMS: nowMS})
				return err
			}}
		}},
		{"escalated budget", func(t *testing.T, conn *sql.DB) attempt {
			runID, _ := budgetRun(t, conn, 0)
			cost := expectedCostOf(t, conn, "implement@0")
			execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, 3*cost, runID)
			id := stepIDByInstance(t, conn, "implement@0")
			return attempt{runID, id, func() error {
				_, err := ClaimStep(conn, id, ClaimOptions{Owner: "worker", CostMultiplier: 4, NowMS: nowMS})
				return err
			}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn := mustDB(t)
			a := c.setup(t, conn)
			before := len(claimRefusedEvents(t, conn, a.runID))

			err := a.claim()
			code, isEngine := CodeOf(err)
			if !(isEngine && code == CodeConflict) && !errors.Is(err, db.ErrLeaseHeld) {
				t.Fatalf("claim err = %v, want a CONFLICT refusal", err)
			}

			events := claimRefusedEvents(t, conn, a.runID)
			if len(events)-before != 1 {
				t.Fatalf("%d claim-refused events written, want exactly 1", len(events)-before)
			}
			got := events[len(events)-1]
			want := map[string]any{
				"step": model.FormatStepID(a.stepID), "owner": "worker", "reason": err.Error(),
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("event %s = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}
