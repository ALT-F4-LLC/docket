package engine

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4264: a claim refusal names the step holding what the claim needed, so a
// conductor's refill releases a held lane when its holder reaches a state that
// frees the scope, instead of relaunching against a holder that is still gated.

// eventHolders decodes the "holders" array of a claim-refused event's data.
func eventHolders(t *testing.T, data map[string]any) []Holder {
	t.Helper()
	raw, ok := data["holders"].([]any)
	if !ok {
		t.Fatalf("claim-refused data carries no holders array: %v", data)
	}
	var out []Holder
	for _, r := range raw {
		m := r.(map[string]any)
		out = append(out, Holder{Step: m["step"].(string), Status: m["status"].(string)})
	}
	return out
}

func refusalError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("claim error %v is not an engine error", err)
	}
	return e
}

// TestClaimRefusalNamesGatedClassHolder is the acceptance check: a gated writer
// occupies the write class, a second writer's claim is refused, and both the
// refusal and the claim-refused event name the gated writer's step with status
// gated.
func TestClaimRefusalNamesGatedClassHolder(t *testing.T) {
	conn := mustDB(t)
	runID := claimRefusedRun(t, conn)

	claimInstance(t, conn, "one@0", nowMS)
	holderID := stepIDByInstance(t, conn, "one@0")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepGated, holderID)

	_, err := ClaimStep(conn, stepIDByInstance(t, conn, "two@0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	if err == nil {
		t.Fatal("the second writer claimed past a gated holder at max 1")
	}
	want := []Holder{{Step: model.FormatStepID(holderID), Status: db.StepGated}}

	refusal := refusalError(t, err)
	if !reflect.DeepEqual(refusal.Holders, want) {
		t.Errorf("refusal holders = %+v; want %+v", refusal.Holders, want)
	}
	if !strings.Contains(err.Error(), "held by "+model.FormatStepID(holderID)+" (gated)") {
		t.Errorf("refusal text does not name the gated holder: %v", err)
	}

	events := claimRefusedEvents(t, conn, runID)
	if len(events) != 1 {
		t.Fatalf("claim-refused events = %d; want 1", len(events))
	}
	if got := eventHolders(t, events[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("event holders = %+v; want %+v", got, want)
	}
}

// TestClaimRefusalNamesScopeHolder: a scope refusal names the claimed step
// whose issue scope intersects the refused step's.
func TestClaimRefusalNamesScopeHolder(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue a", "body", "task", nil)
	b := createIssue(t, conn, "issue b", "body", "task", nil)
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, a, `["internal/db/**"]`), "scope a")
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, b, `["internal/db/leases.go"]`), "scope b")
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// "implement@0" resolves to the lowest id: issue a's.
	claimInstance(t, conn, "implement@0", nowMS)
	var holderID, refusedID int
	testsupport.Must(t, conn.QueryRow(
		`SELECT id FROM steps WHERE issue_id = ? AND step_name = 'implement'`, a).Scan(&holderID), "holder id")
	testsupport.Must(t, conn.QueryRow(
		`SELECT id FROM steps WHERE issue_id = ? AND step_name = 'implement'`, b).Scan(&refusedID), "refused id")

	_, err = ClaimStep(conn, refusedID, ClaimOptions{Owner: "other", NowMS: nowMS})
	if err == nil {
		t.Fatal("a step claimed past an intersecting claimed scope")
	}
	want := []Holder{{Step: model.FormatStepID(holderID), Status: db.StepClaimed}}
	if got := refusalError(t, err).Holders; !reflect.DeepEqual(got, want) {
		t.Errorf("refusal holders = %+v; want %+v", got, want)
	}
	if !strings.Contains(err.Error(), "held by "+model.FormatStepID(holderID)+" (claimed)") {
		t.Errorf("refusal text does not name the scope holder: %v", err)
	}

	events := claimRefusedEvents(t, conn, run.ID)
	if len(events) != 1 {
		t.Fatalf("claim-refused events = %d; want 1", len(events))
	}
	if got := eventHolders(t, events[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("event holders = %+v; want %+v", got, want)
	}
}

// TestClaimRefusalWithoutHolderCarriesNone: a refusal that is not a scope or
// headroom one keeps the event shape it had before.
func TestClaimRefusalWithoutHolderCarriesNone(t *testing.T) {
	conn := mustDB(t)
	runID := claimRefusedRun(t, conn)

	_, err := ClaimStep(conn, stepIDByInstance(t, conn, "later@0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	if err == nil {
		t.Fatal("a step claimed before its predecessor was done")
	}
	events := claimRefusedEvents(t, conn, runID)
	if len(events) != 1 {
		t.Fatalf("claim-refused events = %d; want 1", len(events))
	}
	if _, has := events[0]["holders"]; has {
		t.Errorf("a predecessor refusal carries holders: %v", events[0])
	}
}

// A lapsed writer's unacknowledged reap holds class headroom, so a refusal it
// causes names the reaped step even when nothing live holds the class.
func TestClaimRefusalNamesAnUnacknowledgedReap(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, _ := reapOneWriter(t, conn, runID)
	reapedID := stepIDByInstance(t, conn, "one@0")
	execSQL(t, conn,
		`UPDATE steps SET status = ?, usage_recorded = 1 WHERE run_id = ? AND instance = ?`,
		db.StepDone, runID, "one@0")

	_, err := ClaimStep(conn, stepIDByInstance(t, conn, "two@0"),
		ClaimOptions{Owner: "worker", NowMS: at})
	if err == nil {
		t.Fatal("the successor claimed past an unacknowledged reap")
	}
	want := []Holder{{Step: model.FormatStepID(reapedID), Status: HolderUnacknowledgedReap}}
	if got := refusalError(t, err).Holders; !reflect.DeepEqual(got, want) {
		t.Errorf("refusal holders = %+v; want %+v", got, want)
	}
	if !strings.Contains(err.Error(), "held by "+model.FormatStepID(reapedID)+" ("+HolderUnacknowledgedReap+")") {
		t.Errorf("refusal text does not name the reaped holder: %v", err)
	}
}
