package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Post-completion annotation (DKT-35). A step's record freezes when the saga
// routes it, but integration mints a new commit id AFTER that — and a record
// citing the writer's own sha is gc-eligible prose once the worktree is
// swept. `step annotate` is the durable channel for such late facts.

func TestAnnotateMergesOntoAFinishedStep(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	stepID := stepIDByInstance(t, conn, "implement@0")

	updated, err := AnnotateStep(conn, stepID,
		`{"integrated": "durable-and-reachable"}`, nowMS)
	testsupport.Must(t, err, "AnnotateStep: %v", err)

	var bag map[string]any
	testsupport.Must(t, json.Unmarshal([]byte(updated.Metadata), &bag),
		"decoding metadata: %v", err)
	if bag["integrated"] != "durable-and-reachable" {
		t.Errorf("metadata = %v, want the annotation merged", bag)
	}

	// The mutation is in the ledger, annotation verbatim.
	page, err := ListEvents(conn, EventQuery{RunID: updated.RunID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	event, ok := findEvent(t, page, EventStepAnnotated)
	if !ok {
		t.Fatalf("no %s event; a record that changed with no event is evidence "+
			"that rewrote itself", EventStepAnnotated)
	}
	if !strings.Contains(string(event.Data), "durable-and-reachable") {
		t.Errorf("event data = %s, want the annotation verbatim", event.Data)
	}

	// A second annotation MERGES rather than replaces the bag.
	updated, err = AnnotateStep(conn, stepID, `{"second": "fact"}`, nowMS)
	testsupport.Must(t, err, "second AnnotateStep: %v", err)
	bag = nil
	testsupport.Must(t, json.Unmarshal([]byte(updated.Metadata), &bag), "decoding")
	if bag["integrated"] != "durable-and-reachable" || bag["second"] != "fact" {
		t.Errorf("metadata = %v, want both annotations", bag)
	}
}

func TestAnnotateRefusals(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	stepID := stepIDByInstance(t, conn, "implement@0")

	// A live step refuses: its metadata lands with its record, under its
	// holder's token.
	if _, err := AnnotateStep(conn, stepID, `{"k": "v"}`, nowMS); err == nil {
		t.Fatal("annotating a pending step succeeded")
	} else if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("error code = %q, want %q", code, CodeConflict)
	}

	// Malformed and empty bags refuse without writing.
	e := testEngine()
	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	if _, err := AnnotateStep(conn, stepID, `not json`, nowMS); err == nil {
		t.Fatal("a malformed annotation was accepted")
	}
	if _, err := AnnotateStep(conn, stepID, "", nowMS); err == nil {
		t.Fatal("an empty annotation was accepted")
	}

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if strings.Contains(step.Metadata, "not json") {
		t.Errorf("a refused annotation landed: %q", step.Metadata)
	}
}
