package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-489 — `step show`/`step list` rendered an expired-but-unreaped claim as
// bare `ready` while `run repin`'s quiescence guard, reading the raw row,
// refused with "claimed and mid-flight". BOTH answers are per-spec: the read
// surface renders §6.2's effective status (the machine's own
// `claimed ──lease expiry──> ready` edge, and `claim` makes it true by
// reaping lazily — DKT-468 aligned `run status` to exactly this), while the
// repin guard must count the unreaped claim because an executor that claimed
// under the current pins may still be writing until the reap actually lands
// (the same principle that makes unacknowledged reaps hold class headroom).
// So the two do not converge on one status string; instead the window is
// LABELED on the step surface — `lease_expired: true` — so a caller holding
// "ready" here and CONFLICT from repin can reconcile them, and the label
// vanishes the moment the claim is genuinely gone (re-claimed or reaped).

// stepListRowOf finds one instance's row in a step-list answer.
func stepListRowOf(t *testing.T, rows []StepListEntry, instance string) StepListEntry {
	t.Helper()
	for _, row := range rows {
		if row.Instance == instance {
			return row
		}
	}
	t.Fatalf("no %s row among %d list rows", instance, len(rows))
	return StepListEntry{}
}

// TestExpiredUnreapedClaimIsLabeledOnTheStepSurfaces walks the whole window:
// live claim (no label, repin refuses), lapsed-unreaped (label on both
// surfaces, repin still refuses), re-claimed (label gone, every surface says
// `claimed` — the issue's own "after a genuine re-claim" observation).
func TestExpiredUnreapedClaimIsLabeledOnTheStepSurfaces(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	// Drift a pin so repin has a change to refuse over — with nothing drifted
	// it no-ops before its quiescence guard runs.
	pinAFile(t, conn, run.ID, root, "contracts/dkt489.md", "OLD\n")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/dkt489.md"), []byte("NEW\n"), 0o644), "rewrite")

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	// A LIVE claim: both surfaces say `claimed`, no label, and repin refuses.
	view, err := LoadStepView(conn, stepID, nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.Row.Status != db.StepClaimed || view.Row.LeaseExpired {
		t.Fatalf("live claim renders status=%q lease_expired=%v, want %q/false",
			view.Row.Status, view.Row.LeaseExpired, db.StepClaimed)
	}
	rows, err := RunStepList(conn, run.ID, nowMS)
	testsupport.Must(t, err, "RunStepList: %v", err)
	if row := stepListRowOf(t, rows, "implement@0"); row.Status != db.StepClaimed ||
		row.LeaseExpired {
		t.Fatalf("live claim lists status=%q lease_expired=%v, want %q/false",
			row.Status, row.LeaseExpired, db.StepClaimed)
	}
	if _, err := repinRunIn(conn, run.ID, "install", nowMS, []string{root}); err == nil {
		t.Fatal("repin proceeded under a live claim")
	}

	// The lease lapses, un-reaped. The rendered status flips to the reap's
	// answer (`ready`, §6.2 — the discipline DKT-468 pinned), repin still
	// refuses — and the window is now labeled on both step surfaces.
	late := claim.LeaseExpiresMS + 1
	view, err = LoadStepView(conn, stepID, late)
	testsupport.Must(t, err, "LoadStepView after expiry: %v", err)
	if view.Row.Status != db.StepReady {
		t.Fatalf("expired-unreaped renders status=%q, want %q",
			view.Row.Status, db.StepReady)
	}
	if !view.Row.LeaseExpired {
		t.Error("step show does not label the expired-but-unreaped claim; a " +
			"caller holding this `ready` and repin's CONFLICT cannot reconcile them")
	}
	rows, err = RunStepList(conn, run.ID, late)
	testsupport.Must(t, err, "RunStepList after expiry: %v", err)
	if row := stepListRowOf(t, rows, "implement@0"); row.Status != db.StepReady ||
		!row.LeaseExpired {
		t.Errorf("step list renders status=%q lease_expired=%v after expiry, want %q/true",
			row.Status, row.LeaseExpired, db.StepReady)
	}

	// The wire name is the checkable contract: present inside the window.
	raw, err := json.Marshal(view.Row)
	testsupport.Must(t, err, "marshal: %v", err)
	if !strings.Contains(string(raw), `"lease_expired":true`) {
		t.Errorf("step show JSON %s does not carry lease_expired", raw)
	}

	_, err = repinRunIn(conn, run.ID, "install", late, []string{root})
	if err == nil {
		t.Fatal("repin proceeded under an expired-but-unreaped claim; the reap, " +
			"not the expiry, is when the executor is confirmed gone")
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("error code = %v, want CONFLICT: %v", code, err)
	}
	if !strings.Contains(err.Error(), "implement@0") {
		t.Errorf("refusal %q does not name the expired claim's step", err)
	}

	// A re-claim proves `ready` was honest — `claim` reaps lazily and succeeds
	// — and ends the window: the label is gone (omitempty, so the field
	// vanishes from the wire) and every surface says `claimed` again.
	_, err = ClaimStep(conn, stepID, ClaimOptions{Owner: "worker-2", NowMS: late})
	testsupport.Must(t, err, "re-claim of an expired-unreaped step: %v", err)
	view, err = LoadStepView(conn, stepID, late)
	testsupport.Must(t, err, "LoadStepView after re-claim: %v", err)
	if view.Row.Status != db.StepClaimed || view.Row.LeaseExpired {
		t.Errorf("re-claimed step renders status=%q lease_expired=%v, want %q/false",
			view.Row.Status, view.Row.LeaseExpired, db.StepClaimed)
	}
	raw, err = json.Marshal(view.Row)
	testsupport.Must(t, err, "marshal: %v", err)
	if strings.Contains(string(raw), "lease_expired") {
		t.Errorf("a live claim's JSON %s still carries lease_expired; the label "+
			"must vanish outside the window", raw)
	}
}

// TestRepinProceedsOnceAnExpiredClaimIsReaped is the window's other exit: the
// real lazy reap (`next`'s, the path production takes) clears both the label
// and repin's refusal in the same stroke, so the step surfaces and the repin
// guard agree at every instant of the claim's lifecycle.
func TestRepinProceedsOnceAnExpiredClaimIsReaped(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	pinAFile(t, conn, run.ID, root, "contracts/dkt489b.md", "OLD\n")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/dkt489b.md"), []byte("NEW\n"), 0o644), "rewrite")

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	late := claim.LeaseExpiresMS + 1

	if _, err := repinRunIn(conn, run.ID, "install", late, []string{root}); err == nil {
		t.Fatal("repin proceeded under an expired-but-unreaped claim")
	}

	// The reap lands.
	_, err = e.NextSteps(conn, run.ID, 0, late)
	testsupport.Must(t, err, "NextSteps: %v", err)

	// The label clears with the claim actually gone...
	view, err := LoadStepView(conn, stepID, late)
	testsupport.Must(t, err, "LoadStepView after the reap: %v", err)
	if view.Row.LeaseExpired {
		t.Error("the reap landed but the row is still labeled lease_expired")
	}
	if view.Row.Status != db.StepReady {
		t.Errorf("reaped step renders %q, want %q", view.Row.Status, db.StepReady)
	}

	// ...and repin now proceeds.
	outcome, err := repinRunIn(conn, run.ID, "install", late, []string{root})
	testsupport.Must(t, err, "repin after the reap: %v", err)
	if len(outcome.Repinned) != 1 {
		t.Errorf("repinned %d pin(s) after the reap, want 1", len(outcome.Repinned))
	}
}
