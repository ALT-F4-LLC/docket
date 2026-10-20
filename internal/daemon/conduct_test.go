package daemon

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

var conductBy = engine.ConductOptions{
	By:    engine.Attribution{Actor: "Ada Relay", Cwd: "/work/session"},
	NowMS: 1000,
}

// unboundRun inserts a planning run that holds no conductor capability, the
// state a run activated before the capability existed is in.
func unboundRun(t *testing.T, conn *sql.DB) int {
	t.Helper()
	run, err := db.InsertRun(conn, db.DefaultProjectID, "legacy run", 0, 1)
	testsupport.Must(t, err, "InsertRun: %v", err)
	hash, err := db.RunConductorHash(conn, run.ID)
	testsupport.Must(t, err, "RunConductorHash: %v", err)
	if hash != "" {
		t.Fatalf("a fresh run already holds a conductor capability")
	}
	return run.ID
}

func assertSeated(t *testing.T, conn *sql.DB, runID int, result *engine.ConductResult) {
	t.Helper()
	hash, err := db.RunConductorHash(conn, runID)
	testsupport.Must(t, err, "RunConductorHash: %v", err)
	if !model.TokenMatches(result.Token, hash) {
		t.Fatal("the run is not bound to the token the conduct returned")
	}
}

// TestConductUnboundRunRequiresHTTP: the first conductor of an unbound run is
// seated only over HTTP; over the Unix socket it is refused with a message
// naming HTTP and the run stays unbound. Once bound, the socket may re-seat.
func TestConductUnboundRunRequiresHTTP(t *testing.T) {
	conn := newEventStore(t)

	t.Run("socket refused", func(t *testing.T) {
		runID := unboundRun(t, conn)
		_, err := ConductRun(conn, runID, TransportSocket, conductBy)
		if err == nil {
			t.Fatal("the socket seated the first conductor of an unbound run")
		}
		if !strings.Contains(err.Error(), "HTTP") {
			t.Errorf("refusal %q does not name HTTP", err)
		}
		if code, _ := engine.CodeOf(err); code != engine.CodeAuth {
			t.Errorf("refusal code = %q, want %s", code, engine.CodeAuth)
		}
		hash, err := db.RunConductorHash(conn, runID)
		testsupport.Must(t, err, "RunConductorHash: %v", err)
		if hash != "" {
			t.Error("the refused conduct bound the run")
		}
	})

	t.Run("HTTP seats", func(t *testing.T) {
		runID := unboundRun(t, conn)
		result, err := ConductRun(conn, runID, TransportHTTP, conductBy)
		testsupport.Must(t, err, "ConductRun over HTTP: %v", err)
		assertSeated(t, conn, runID, result)
	})

	t.Run("socket re-seats a bound run", func(t *testing.T) {
		runID := unboundRun(t, conn)
		_, err := ConductRun(conn, runID, TransportHTTP, conductBy)
		testsupport.Must(t, err, "binding over HTTP: %v", err)
		result, err := ConductRun(conn, runID, TransportSocket, conductBy)
		testsupport.Must(t, err, "ConductRun over the socket on a bound run: %v", err)
		if !result.Rotated {
			t.Error("rotated = false on a run that already held a capability")
		}
		assertSeated(t, conn, runID, result)
	})
}
