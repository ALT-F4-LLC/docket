package engine

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// configChangeEvents reads every `config-changed` event's data, oldest first.
func configChangeEvents(t *testing.T, conn *sql.DB) []configChange {
	t.Helper()
	rows, err := conn.Query(
		`SELECT data FROM events WHERE kind = ? ORDER BY seq ASC`, EventConfigChanged)
	testsupport.Must(t, err, "reading config-changed events: %v", err)
	defer rows.Close()
	var out []configChange
	for rows.Next() {
		var raw string
		testsupport.Must(t, rows.Scan(&raw), "scanning an event: %v", err)
		var c configChange
		testsupport.Must(t, json.Unmarshal([]byte(raw), &c), "decoding %q: %v", raw, err)
		out = append(out, c)
	}
	return out
}

// TestConfigSetOfAVoteRuleKeyRecordsAnEvent is DKT-2767: a successful write to
// a `vote.rule.<name>.*` key records exactly one `config-changed` event in the
// write's own transaction, carrying the key, the project and scope, the prior
// and new values and the writer; a write outside the family records none, and
// a refused write records none.
func TestConfigSetOfAVoteRuleKeyRecordsAnEvent(t *testing.T) {
	conn := mustDB(t)
	key := db.VoteRuleThresholdKey("x")

	t.Run("a first write records the empty prior", func(t *testing.T) {
		err := SetConfig(conn, 0, key, "0.6", "erik")
		testsupport.Must(t, err, "SetConfig: %v", err)
		events := configChangeEvents(t, conn)
		if len(events) != 1 {
			t.Fatalf("%d config-changed events after one write, want 1", len(events))
		}
		want := configChange{Key: key, Project: 0, Scope: "global", Prior: "", Value: "0.6", ChangedBy: "erik"}
		if events[0] != want {
			t.Errorf("event data = %+v, want %+v", events[0], want)
		}
	})

	t.Run("an overwrite records the earlier value as prior", func(t *testing.T) {
		err := SetConfig(conn, 0, key, "0.8", "erik")
		testsupport.Must(t, err, "SetConfig: %v", err)
		events := configChangeEvents(t, conn)
		if len(events) != 2 {
			t.Fatalf("%d config-changed events after two writes, want 2", len(events))
		}
		if events[1].Prior != "0.6" || events[1].Value != "0.8" {
			t.Errorf("overwrite recorded prior %q -> %q, want 0.6 -> 0.8",
				events[1].Prior, events[1].Value)
		}
		// A project-scoped write is its own scope: its prior is the project
		// row's, which the global writes above never touched.
		projectID, err := db.EnsureProject(conn, "/repo/one", "one", 1)
		testsupport.Must(t, err, "EnsureProject: %v", err)
		err = SetConfig(conn, projectID, key, "0.7", "erik")
		testsupport.Must(t, err, "project SetConfig: %v", err)
		events = configChangeEvents(t, conn)
		last := events[len(events)-1]
		if last.Project != projectID || last.Scope != "project" || last.Prior != "" {
			t.Errorf("project write recorded %+v, want project %d, scope project, empty prior",
				last, projectID)
		}
	})

	t.Run("a key outside the vote-rule family records no event", func(t *testing.T) {
		before := len(configChangeEvents(t, conn))
		err := SetConfig(conn, 0, db.KeyEventsRetain, "720h", "erik")
		testsupport.Must(t, err, "SetConfig(events.retain): %v", err)
		if got := len(configChangeEvents(t, conn)); got != before {
			t.Errorf("events.retain recorded a config-changed event (%d -> %d)", before, got)
		}
		entry, err := db.GetConfig(conn, 0, db.KeyEventsRetain)
		testsupport.Must(t, err, "GetConfig: %v", err)
		if entry.Value != "720h" {
			t.Errorf("events.retain = %q, want the written 720h", entry.Value)
		}
	})

	t.Run("a refused write records no event and writes nothing", func(t *testing.T) {
		before := len(configChangeEvents(t, conn))
		if err := SetConfig(conn, 0, key, "1.5", "erik"); err == nil {
			t.Fatal("SetConfig accepted a threshold above 1")
		}
		if got := len(configChangeEvents(t, conn)); got != before {
			t.Errorf("a refused write recorded a config-changed event (%d -> %d)", before, got)
		}
		entry, err := db.GetConfig(conn, 0, key)
		testsupport.Must(t, err, "GetConfig: %v", err)
		if entry.Value != "0.8" {
			t.Errorf("the refused write changed the stored value to %q", entry.Value)
		}
	})
}
