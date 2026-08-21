package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Per-cluster hold resolution.
//
// A hold carrying several clusters used to materialize ONE `<step>-held@k`
// step, and approve/reject over it was binary across the whole set. RUN-1's
// round-2 hold carried four clusters in one step; the operator wanted two
// escalated and two accepted, could not express it, and recorded the protest
// in the approve note instead.
//
// The fix materializes ONE HUMAN STEP PER HELD CLUSTER, so each is answered on
// its own. These tests pin that, and pin that the aggregate half still treats
// the routing step as gated until every cluster has an answer.

// multiClusterPayload holds TWO clusters, each with spread >= 2. The fixture's
// `hold_spread = 2` over `findings@1`'s severity order is what makes both
// held; that is the same mechanism the single-cluster tests use, with a second
// cluster added so "all-or-nothing" has something to be wrong about.
const multiClusterPayload = `[
  {"id":"C-1","severity":["low","blocker"]},
  {"id":"C-2","severity":["low","high"]}
]`

// TestEachHeldClusterGetsItsOwnStep is the core assertion.
//
// Two held clusters must produce two human steps. With one step for both, an
// operator has exactly one approve/reject to spend on two independent
// questions.
func TestEachHeldClusterGetsItsOwnStep(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)

	held := heldInstances(t, conn)
	if len(held) != 2 {
		t.Fatalf("got %d held step(s) for 2 held clusters: %v\n"+
			"each held cluster needs its own human step, or approve/reject "+
			"is binary over the set (DKT-15)", len(held), held)
	}
}

// TestHeldClustersResolveIndependently is the property the operator actually
// wanted: accept one cluster, reject another, in the same hold.
func TestHeldClustersResolveIndependently(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)

	held := heldInstances(t, conn)
	if len(held) != 2 {
		t.Fatalf("expected 2 held steps, got %v", held)
	}

	// Approve the first, reject the second — the mixed disposition that was
	// inexpressible before.
	approveHeld(t, conn, e, held[0])
	rejectHeld(t, conn, e, held[1])

	// Both decisions must be recorded, and they must DIFFER. A fix that
	// resolved them together would show the same routing on both.
	first := heldStep(t, conn, held[0]).Routing
	second := heldStep(t, conn, held[1]).Routing
	if first == second {
		t.Errorf("both clusters routed %q; approve and reject on separate "+
			"clusters must not collapse to one disposition", first)
	}
}

// TestApprovingOneClusterDoesNotResolveTheOthers is the narrower half of the
// same property, and the one a partial implementation gets wrong: resolving
// the first cluster must leave the second still asking.
func TestApprovingOneClusterDoesNotResolveTheOthers(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)

	held := heldInstances(t, conn)
	if len(held) != 2 {
		t.Fatalf("expected 2 held steps, got %v", held)
	}

	approveHeld(t, conn, e, held[0])

	// The SECOND is still open.
	if status := stepStatus(t, conn, held[1]); db.StepTerminal(status) {
		t.Errorf("%s is %q after approving a DIFFERENT cluster; each hold "+
			"must be answered on its own", held[1], status)
	}

	// And the routing step is still gated, because one question is unanswered.
	// Releasing on a partial answer would route over a cluster nobody decided.
	if ready := readyInstances(t, conn); contains(ready, "verify@0") {
		t.Error("a downstream step became ready while a held cluster was " +
			"still unanswered")
	}
}

// heldInstances lists the materialized held steps for the fixture's
// `reconcile`, in a stable order.
func heldInstances(t *testing.T, conn *sql.DB) []string {
	t.Helper()

	rows, err := conn.Query(
		`SELECT instance FROM steps
		  WHERE materialized = 1 AND step_name LIKE 'reconcile-held%'
		  ORDER BY instance`)
	testsupport.Must(t, err, "listing held steps: %v", err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var instance string
		err := rows.Scan(&instance)
		testsupport.Must(t, err, "reading a held step: %v", err)
		out = append(out, instance)
	}
	err = rows.Err()
	testsupport.Must(t, err, "listing held steps: %v", err)
	return out
}

func approveHeld(t *testing.T, conn *sql.DB, e *Engine, instance string) {
	t.Helper()
	err := e.DecideStep(conn, stepIDByInstance(t, conn, instance),
		true, "accepted", nowMS)
	testsupport.Must(t, err, "approving %s: %v", instance, err)
}

func rejectHeld(t *testing.T, conn *sql.DB, e *Engine, instance string) {
	t.Helper()
	err := e.DecideStep(conn, stepIDByInstance(t, conn, instance),
		false, "escalated", nowMS)
	testsupport.Must(t, err, "rejecting %s: %v", instance, err)
}

// TestHeldStepPacketCarriesItsCluster pins DKT-105: a materialized held
// step's bundle inlines the ONE cluster it decides. Renders used to carry
// only the step header and issue body, so every panel fetched the routing
// step's findings artifact out-of-band and correlated `#k` to a payload
// position by hand — an array-order join with transposition risk.
func TestHeldStepPacketCarriesItsCluster(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)

	clusters := []string{"C-1", "C-2"}
	for i, wantID := range clusters {
		instance := fmt.Sprintf("reconcile-held@0#%d", i)
		bundle, err := ReadContext(conn, stepIDByInstance(t, conn, instance), nowMS)
		testsupport.Must(t, err, "ReadContext(%s): %v", instance, err)

		var cluster *ContextInput
		for j := range bundle.Inputs {
			if bundle.Inputs[j].Kind == "held-cluster" {
				cluster = &bundle.Inputs[j]
			}
		}
		if cluster == nil {
			t.Fatalf("%s has no held-cluster input; the packet poses a "+
				"question without its subject", instance)
		}
		if !strings.Contains(cluster.Payload, wantID) {
			t.Errorf("%s carries %q, want the cluster holding %q",
				instance, cluster.Payload, wantID)
		}
		// The ONE cluster, not the whole payload: inlining every cluster
		// would recreate the join this input exists to remove.
		other := clusters[1-i]
		if strings.Contains(cluster.Payload, other) {
			t.Errorf("%s's input also carries %q — it must inline its own "+
				"cluster alone", instance, other)
		}
		if cluster.ProducerStep != "reconcile@0" {
			t.Errorf("producer = %q, want reconcile@0", cluster.ProducerStep)
		}
	}
}
