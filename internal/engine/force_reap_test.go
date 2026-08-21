package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `docket step reap` (DKT-83): a relay that established its executor is dead
// clears the claim now, instead of the row blocking for the full lease TTL —
// which cannot be sized for both healthy long writers and fast dead-agent
// cleanup at once.

func TestForceReapClearsADeadClaim(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	stepID := stepIDByInstance(t, conn, "implement@0")
	_, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "doomed", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	err = ForceReapStep(conn, stepID, "spawn died at startup; no surviving process", nowMS)
	testsupport.Must(t, err, "ForceReapStep: %v", err)

	// The step returns to the pool immediately.
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepPending {
		t.Errorf("status = %s after a forced reap, want pending", step.Status)
	}

	// The event is the expiry reap's own kind, distinguished by data.forced
	// and carrying the asserted reason.
	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var reapData struct {
		Forced bool   `json:"forced"`
		Reason string `json:"reason"`
	}
	found := false
	for _, e := range page.Events {
		if e.Kind != EventLeaseReaped {
			continue
		}
		found = true
		testsupport.Must(t, json.Unmarshal(e.Data, &reapData), "decoding data")
	}
	if !found {
		t.Fatal("no lease-reaped event for the forced reap")
	}
	if !reapData.Forced || !strings.Contains(reapData.Reason, "no surviving process") {
		t.Errorf("event data = %+v; a forced reap records who-said-so and why", reapData)
	}

	// The freed step is claimable again — the point of the verb.
	_, err = ClaimStep(conn, stepID, ClaimOptions{Owner: "successor", NowMS: nowMS})
	testsupport.Must(t, err, "the successor's claim: %v", err)
}

func TestForceReapRefusals(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	// No reason: a forced eviction must say on whose word.
	err := ForceReapStep(conn, stepID, "", nowMS)
	if err == nil {
		t.Fatal("a reasonless forced reap was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}

	// An unclaimed step holds no lease to reap.
	err = ForceReapStep(conn, stepID, "it looked stuck", nowMS)
	if err == nil {
		t.Fatal("reaping an unclaimed step was accepted")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("error code = %q, want %q", code, CodeConflict)
	}
}
