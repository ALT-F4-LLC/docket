package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// usageJoinEvents returns the carve_out of each of a run's spawn-admitted
// events, in order.
func usageJoinEvents(t *testing.T, conn *sql.DB, runID int) []string {
	t.Helper()
	page, err := ListEvents(conn, EventQuery{RunID: runID, Kind: EventSpawnAdmitted})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var out []string
	for _, e := range page.Events {
		var data struct {
			CarveOut string `json:"carve_out"`
		}
		testsupport.Must(t, json.Unmarshal(e.Data, &data), "decoding spawn-admitted: %v", nil)
		out = append(out, data.CarveOut)
	}
	return out
}

// TestGuardSpawnUsageJoin is DKT-3289 on the --run path: under an
// unacknowledged reap, a launch declared a usage join with no rows is admitted
// with exactly one spawn-admitted event whose carve_out is usage-join; the
// same spawn without the option still denies naming --ack-reap; and a usage
// join that proposes rows is denied and writes no event.
func TestGuardSpawnUsageJoin(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, _ := reapOneWriter(t, conn, runID)

	plain, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{NowMS: at})
	testsupport.Must(t, err, "GuardSpawn: %v", err)
	if plain.Allowed || !strings.Contains(plain.Reason, "--ack-reap") {
		t.Fatalf("a plain spawn under the hold: allowed=%v reason=%q; want a denial naming --ack-reap",
			plain.Allowed, plain.Reason)
	}
	if got := usageJoinEvents(t, conn, runID); len(got) != 0 {
		t.Fatalf("a denied plain spawn wrote spawn-admitted events: %v", got)
	}

	// Rows that byte-match the open dispatch pass the row half, so only the
	// usage-join branch's own rows clause can deny this launch.
	manifest := openDispatch(t, conn, runID, 0, at)
	rows, err := json.Marshal(manifest.Rows)
	testsupport.Must(t, err, "marshaling the manifest rows: %v", err)
	if len(manifest.Rows) == 0 {
		t.Fatal("premise: the open dispatch must offer at least one row")
	}
	withRows, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
		UsageJoin: true, Rows: rows, NowMS: at,
	})
	testsupport.Must(t, err, "GuardSpawn --usage-join --rows: %v", err)
	if withRows.Allowed {
		t.Fatalf("a usage join proposing rows was admitted under the hold: %q", withRows.Reason)
	}
	if got := usageJoinEvents(t, conn, runID); len(got) != 0 {
		t.Fatalf("a denied usage join with rows wrote spawn-admitted events: %v", got)
	}

	joined, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{UsageJoin: true, NowMS: at})
	testsupport.Must(t, err, "GuardSpawn --usage-join: %v", err)
	if !joined.Allowed {
		t.Fatalf("a usage join claiming no step was denied by the reap hold: %q", joined.Reason)
	}
	if got := usageJoinEvents(t, conn, runID); len(got) != 1 || got[0] != "usage-join" {
		t.Errorf("spawn-admitted carve-outs = %v, want exactly [usage-join]", got)
	}
	if len(openReapsOf(t, conn, runID)) == 0 {
		t.Error("the usage join acknowledged the reap; it only admits a launch past it")
	}

	if _, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
		UsageJoin: true, DecidingVote: 1, NowMS: at,
	}); !hasCode(err, CodeValidation) {
		t.Errorf("--usage-join with --deciding-vote: err = %v, want a validation error", err)
	}
}

// TestGuardSpawnActiveUsageJoin is DKT-3289 on the --active path: the same
// hold denies a plain --active spawn naming --ack-reap, and admits a usage
// join with exactly one spawn-admitted usage-join event.
func TestGuardSpawnActiveUsageJoin(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")
	held, _ := activeRunOver(t, conn, "held")
	at := reapWriterOf(t, conn, held)

	plain, err := GuardSpawnActive(conn, 0, 0, false, at)
	testsupport.Must(t, err, "GuardSpawnActive: %v", err)
	if plain.Allowed || !strings.Contains(plain.Reason, "--ack-reap") {
		t.Fatalf("a plain --active spawn under the hold: allowed=%v reason=%q; want a denial naming --ack-reap",
			plain.Allowed, plain.Reason)
	}
	if got := usageJoinEvents(t, conn, held); len(got) != 0 {
		t.Fatalf("a denied plain spawn wrote spawn-admitted events: %v", got)
	}

	joined, err := GuardSpawnActive(conn, 0, 0, true, at)
	testsupport.Must(t, err, "GuardSpawnActive --usage-join: %v", err)
	if !joined.Allowed {
		t.Fatalf("an --active usage join was denied by the reap hold: %q", joined.Reason)
	}
	if got := usageJoinEvents(t, conn, held); len(got) != 1 || got[0] != "usage-join" {
		t.Errorf("spawn-admitted carve-outs = %v, want exactly [usage-join]", got)
	}

	if _, err := GuardSpawnActive(conn, 0, 1, true, at); !hasCode(err, CodeValidation) {
		t.Errorf("--active --usage-join --deciding-vote: err = %v, want a validation error", err)
	}
}
