package engine

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The step-artifact read surface.
//
// The defect these pin is an ABSENCE: an action step's verdict and an
// aggregate's held-cluster payload were in the database with no verb that
// reached them, so RUN-1's conductor read them with raw sqlite. The tests
// therefore drive the same fixture the held-cluster tests use and assert that
// what a step produced is now readable through the engine's own API.

// TestListStepArtifactsReportsWhatAStepProduced covers the listing half: what
// is here, how big, and no bodies.
func TestListStepArtifactsReportsWhatAStepProduced(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	stepID := stepIDByInstance(t, conn, "implement@0")
	artifacts, err := ListStepArtifacts(conn, stepID)
	testsupport.Must(t, err, "ListStepArtifacts: %v", err)

	if len(artifacts) == 0 {
		t.Fatal("implement@0 produced no artifacts; the fixture records at least one")
	}

	// Matched by KIND, not by size. The fixture's stubbed diff artifact can
	// coincidentally have the same byte count as the recorded body, so a
	// size match silently selects the wrong row.
	var summary *StepArtifact
	for i := range artifacts {
		if artifacts[i].Kind == "change-summary" {
			summary = &artifacts[i]
		}
	}
	if summary == nil {
		t.Fatalf("no artifact matched the recorded body; got %+v", artifacts)
	}

	// THE BODY IS NOT IN THE LISTING. An artifact runs to 1MiB, so a listing
	// that inlined bodies would be unreadable exactly when it matters.
	if summary.Body != "" {
		t.Errorf("listing carried a body (%d bytes); it must report sizes only",
			len(summary.Body))
	}
	// The size and identity ARE, or the listing cannot be used to choose.
	if summary.Bytes != len("the change summary") {
		t.Errorf("Bytes = %d, want %d", summary.Bytes, len("the change summary"))
	}
	if !strings.HasPrefix(summary.Artifact, "ARTIFACT-") {
		t.Errorf("Artifact = %q, want an ARTIFACT-N reference", summary.Artifact)
	}
	if summary.SHA256 == "" {
		t.Error("SHA256 is empty; the listing must carry the hash")
	}
	if summary.Producer != "implement@0" {
		t.Errorf("Producer = %q, want implement@0", summary.Producer)
	}
}

// TestListStepArtifactsDistinguishesEmptyFromMissing is the distinction that
// makes the verb trustworthy.
//
// A step that produced nothing and a step that does not exist must not give
// the same answer: reporting "no artifacts" for a typo'd reference states a
// fact about a step that is not there.
func TestListStepArtifactsDistinguishesEmptyFromMissing(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	t.Run("a real step that has produced nothing yet lists nothing", func(t *testing.T) {
		// implement@0 exists but has not completed, so it has no artifacts.
		stepID := stepIDByInstance(t, conn, "implement@0")

		artifacts, err := ListStepArtifacts(conn, stepID)
		testsupport.Must(t, err, "ListStepArtifacts on a step with no output: %v", err)
		if len(artifacts) != 0 {
			t.Errorf("got %d artifacts, want none", len(artifacts))
		}
	})

	t.Run("a step that does not exist is NOT_FOUND", func(t *testing.T) {
		_, err := ListStepArtifacts(conn, 999999)
		if err == nil {
			t.Fatal("ListStepArtifacts on a missing step returned nil error")
		}
		code, ok := CodeOf(err)
		if !ok || code != CodeNotFound {
			t.Errorf("code = %v (recognized=%v), want CodeNotFound", code, ok)
		}
	})
}

// TestReadArtifactReturnsBodyAndPayload covers the fetch half — the one that
// answers "what does this verdict actually say".
func TestReadArtifactReturnsBodyAndPayload(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	const (
		body    = "the findings body"
		payload = `[{"id":"C-1","severity":"low"}]`
	)
	claimAndComplete(t, conn, e, "implement@0", body, payload)

	stepID := stepIDByInstance(t, conn, "implement@0")
	listed, err := ListStepArtifacts(conn, stepID)
	testsupport.Must(t, err, "ListStepArtifacts: %v", err)

	// Fetch by the reference the LISTING printed, which is the workflow an
	// operator actually follows. Selected by kind for the same reason as
	// above: the fixture's diff artifact can share the body's byte count.
	var target string
	for _, a := range listed {
		if a.Kind == "change-summary" {
			target = a.Artifact
		}
	}
	if target == "" {
		t.Fatalf("the listing did not name the recorded artifact: %+v", listed)
	}

	got, err := ReadArtifact(conn, artifactIDFromRef(t, target))
	testsupport.Must(t, err, "ReadArtifact(%s): %v", target, err)

	if got.Body != body {
		t.Errorf("Body = %q, want %q", got.Body, body)
	}
	if got.Payload != payload {
		t.Errorf("Payload = %q, want %q", got.Payload, payload)
	}
	if got.Producer != "implement@0" {
		t.Errorf("Producer = %q, want implement@0", got.Producer)
	}
	if got.Artifact != target {
		t.Errorf("Artifact = %q, want the reference asked for (%q)", got.Artifact, target)
	}
}

// TestReadArtifactMissingIsNotFound keeps a bad reference from reading as an
// empty artifact.
func TestReadArtifactMissingIsNotFound(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	_, err := ReadArtifact(conn, 999999)
	if err == nil {
		t.Fatal("ReadArtifact on a missing id returned nil error")
	}
	code, ok := CodeOf(err)
	if !ok || code != CodeNotFound {
		t.Errorf("code = %v (recognized=%v), want CodeNotFound", code, ok)
	}
	if !strings.Contains(err.Error(), "ARTIFACT-999999") {
		t.Errorf("error %q does not name the reference asked for", err)
	}
}

// TestActionStepVerdictIsReadable is the surface's own case, end to end.
//
// The issue is specifically about an ACTION step's result and an aggregate's
// held-cluster payload — the two things the conductor could not read. This
// drives the fixture to `reconcile@0`, an action step the engine runs itself,
// and asserts its output is reachable without touching sqlite.
func TestActionStepVerdictIsReadable(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, clusteredPayload)

	stepID := stepIDByInstance(t, conn, "reconcile@0")
	artifacts, err := ListStepArtifacts(conn, stepID)
	testsupport.Must(t, err, "ListStepArtifacts on the action step: %v", err)
	if len(artifacts) == 0 {
		t.Fatal("the action step's artifacts are unreadable; DKT-17 is not fixed")
	}

	// The verdict must be READABLE, not merely listed. A listing that names an
	// artifact whose body cannot then be fetched is the same dead end.
	got, err := ReadArtifact(conn, artifactIDFromRef(t, artifacts[0].Artifact))
	testsupport.Must(t, err, "ReadArtifact on the action step's output: %v", err)
	if got.Body == "" && got.Payload == "" {
		t.Error("the action step's artifact has neither body nor payload")
	}
	if got.Producer != "reconcile@0" {
		t.Errorf("Producer = %q, want reconcile@0", got.Producer)
	}
}

// artifactIDFromRef parses the N out of an ARTIFACT-N reference, so a test can
// fetch by exactly the string the listing printed.
func artifactIDFromRef(t *testing.T, reference string) int {
	t.Helper()
	id, err := strconv.Atoi(strings.TrimPrefix(reference, "ARTIFACT-"))
	testsupport.Must(t, err, "parsing artifact reference %q: %v", reference, err)
	return id
}
