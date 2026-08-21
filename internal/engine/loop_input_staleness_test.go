package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// roundPayload is one round's synthesize output, carrying a cluster id unique
// to the round so the artifact a later packet binds names the round it came
// from. One value, so the cluster's spread is 0 and no held step materializes;
// `high` so the fixture's `any(severity >= high)` routes `fix-loop`.
func roundPayload(round int) string {
	return fmt.Sprintf(`[{"id":"C-%d01","severity":"high"}]`, round+1)
}

// driveRoundToReconcile completes one loop ordinal up to and including its
// `reconcile` action step, with a payload that routes `fix-loop`.
func driveRoundToReconcile(t *testing.T, conn *sql.DB, e *Engine, ordinal int) {
	t.Helper()
	driveFixtureRound(t, ordinal)
	if ordinal == 0 {
		claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	} else {
		claimAndComplete(t, conn, e, fmt.Sprintf("fix@%d", ordinal), "the fix summary", "")
	}
	completeReviewFanout(t, conn, e, ordinal)
	claimAndComplete(t, conn, e, fmt.Sprintf("synthesize@%d", ordinal),
		"the synthesis", roundPayload(ordinal))
	driveAction(t, conn, e, fmt.Sprintf("reconcile@%d", ordinal))
}

// TestFixRoundBindsTheRoutingInstancesFindings is DKT-375.
//
// A `reconcile` that exhausts `max_fix_loops` parks; the operator authorizes
// one more round with `resolve --as fix-round`, which SUPERSEDES the parked
// instance (human.go's ResolveFixRound) and mints `fix@k+1`. The parked
// instance is the one whose findings the authorization was granted on the
// strength of — and it had already emitted them.
//
// Before the fix, `matchingArtifacts` admitted `done` producers only, so the
// routing instance's artifacts were invisible and `ordinalScoped` fell back a
// round: harness RUN-32's `fix@3` was rendered `reconcile@1`'s 19 round-1
// clusters and not one of `reconcile@2`'s 15 round-2 ones. The executor
// re-verified an already-closed finding set, correctly reported "no code
// change required", and the authorized round was spent reading round 1.
func TestFixRoundBindsTheRoutingInstancesFindings(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	// Three rounds: `max_fix_loops = 2` admits the first two, and the third
	// reconcile parks on the exhausted bound.
	for k := range 3 {
		driveRoundToReconcile(t, conn, e, k)
	}
	if got := stepStatus(t, conn, "reconcile@2"); got != db.StepWaitingHuman {
		t.Fatalf("premise: reconcile@2 = %q, want the exhausted-loop park", got)
	}

	// The operator authorizes one more round on the strength of round 3's
	// findings — the ones reconcile@2 emitted.
	reconcileID := stepIDByInstance(t, conn, "reconcile@2")
	testsupport.Must(t, e.ResolveStep(conn, reconcileID, ResolveFixRound,
		"two blockers stand", nowMS), "resolve --as fix-round")
	if got := stepStatus(t, conn, "reconcile@2"); got != db.StepSuperseded {
		t.Fatalf("premise: reconcile@2 = %q after the re-entry, want %q",
			got, db.StepSuperseded)
	}

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "fix@3"), nowMS)
	testsupport.Must(t, err, "assembling fix@3's bundle: %v", err)

	var bound []string
	for _, in := range bundle.Inputs {
		if in.Kind != "findings" {
			continue
		}
		bound = append(bound, in.ProducerStep)
		if strings.Contains(in.Payload, "C-101") || strings.Contains(in.Payload, "C-201") {
			t.Errorf("fix@3 was fed a STALE round's findings from %s: %s",
				in.ProducerStep, in.Payload)
		}
		if !strings.Contains(in.Payload, "C-301") {
			t.Errorf("%s's payload names no round-3 cluster: %s",
				in.ProducerStep, in.Payload)
		}
	}
	if len(bound) != 1 || bound[0] != "reconcile@2" {
		t.Errorf("`reconcile.findings` bound %v, want [reconcile@2] — the "+
			"instance whose park routed this round", bound)
	}
}
