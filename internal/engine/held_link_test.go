package engine

import (
	"fmt"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestHeldStepViewNamesItsClusterAndArtifact is DKT-239.
//
// A held row named none of its provenance. `step artifacts` on a
// `reconcile-held@0#N` row answered "produced no artifacts" — true, and a dead
// end: the payload sits on the SYNTHESIZE step's artifact, not the hold's.
// `step show --json` named no cluster and no source artifact. And `#N` is the
// element's POSITION in the payload, which reads as a cluster id and is not
// one. A conductor spent 8 calls, 3 tracebacks, and a reading of the
// aggregate's step-recorded event to find and disambiguate one payload.
func TestHeldStepViewNamesItsClusterAndArtifact(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)

	for i := range 2 {
		instance := fmt.Sprintf("reconcile-held@0#%d", i)
		id := stepIDByInstance(t, conn, instance)

		view, err := LoadStepView(conn, id, nowMS)
		testsupport.Must(t, err, "LoadStepView(%s): %v", instance, err)

		if view.HeldCluster == nil {
			t.Fatalf("%s reports no held-cluster link; the row names neither "+
				"the cluster it decides nor where that cluster lives", instance)
		}
		link := view.HeldCluster

		if link.Cluster != i {
			t.Errorf("%s reports cluster index %d, want %d — the index must "+
				"agree with the instance's own #N suffix or it disambiguates "+
				"nothing", instance, link.Cluster, i)
		}
		// The COUNT is what makes the index legible: "#1" alone says nothing
		// about whether that is the last cluster.
		if link.Clusters != 2 {
			t.Errorf("%s reports %d clusters, want 2", instance, link.Clusters)
		}
		if link.Artifact == "" {
			t.Errorf("%s names no source artifact — the one thing `step "+
				"artifacts` on this row cannot tell you", instance)
		}
		if link.ProducerStep != "reconcile@0" {
			t.Errorf("%s reports producer %q, want reconcile@0",
				instance, link.ProducerStep)
		}
	}

	// Both holds point at the SAME artifact — one payload, two clusters —
	// which is exactly the ambiguity the index resolves.
	first, err := LoadStepView(conn, stepIDByInstance(t, conn, "reconcile-held@0#0"), nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	second, err := LoadStepView(conn, stepIDByInstance(t, conn, "reconcile-held@0#1"), nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if first.HeldCluster.Artifact != second.HeldCluster.Artifact {
		t.Errorf("the two holds name different artifacts (%s vs %s); they "+
			"decide two clusters of ONE payload",
			first.HeldCluster.Artifact, second.HeldCluster.Artifact)
	}
}

// TestOrdinaryStepViewCarriesNoHeldLink keeps the field dormant: a step that
// is not a materialized hold must report nothing, so no other step's view
// changes shape.
func TestOrdinaryStepViewCarriesNoHeldLink(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	view, err := LoadStepView(conn, stepIDByInstance(t, conn, "implement@0"), nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.HeldCluster != nil {
		t.Errorf("an ordinary step reports a held-cluster link: %+v", view.HeldCluster)
	}
}

// TestHeldStepStillProducesNoArtifacts pins the fact the link exists to
// explain rather than to contradict: the hold really does produce nothing, and
// the answer is a pointer, not a fabricated artifact on the held row.
func TestHeldStepStillProducesNoArtifacts(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)

	id := stepIDByInstance(t, conn, "reconcile-held@0#0")
	artifacts, err := ListStepArtifacts(conn, id)
	testsupport.Must(t, err, "ListStepArtifacts: %v", err)
	if len(artifacts) != 0 {
		t.Errorf("the held row now claims %d artifact(s); the fix is to point "+
			"at the routing step's artifact, never to attribute one here",
			len(artifacts))
	}
}
