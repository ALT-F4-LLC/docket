package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestRunVerbCommandsOwnTheirProject pins `run start`, `run activate` and
// `run conduct` to the verb layer: the root hook leaves project resolution to
// internal/verb for each.
func TestRunVerbCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{runStartCmd, runActivateCmd, runConductCmd} {
		if !verbOwnsProject(cmd) {
			t.Errorf("`run %s` lacks the verbOwnsProject annotation", cmd.Name())
		}
	}
}

// TestRunStartRecordsTheRegistrationItMakes covers the write the root hook
// used to make for `run start`: a project registered on first contact gets
// its project-registered event, attributed to the command.
func TestRunStartRecordsTheRegistrationItMakes(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/src/first.git", "first.git", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	t.Cleanup(func() { model.SetDisplayPrefix("DKT") })

	cmd := runStartCmdWithDB(conn)
	cmd.SetContext(context.WithValue(cmd.Context(), cfgKey,
		&config.Config{Identity: "/src/fresh.git", Anchored: true}))

	before := registeredEventCount(t, conn)
	w, _ := bufWriter(true)
	testsupport.Must(t, runRunStart(cmd, w), "run start: %v", nil)

	if got := registeredEventCount(t, conn); got != before+1 {
		t.Fatalf("logged %d project-registered event(s), want 1", got-before)
	}
	var data string
	err = conn.QueryRow(`SELECT data FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 1`,
		engine.EventProjectRegistered).Scan(&data)
	testsupport.Must(t, err, "reading the event: %v", err)
	var reg struct {
		Identity string `json:"identity"`
		Verb     string `json:"verb"`
	}
	if err := json.Unmarshal([]byte(data), &reg); err != nil {
		t.Fatalf("decoding the event %s: %v", data, err)
	}
	if reg.Identity != "/src/fresh.git" || reg.Verb != "run start" {
		t.Errorf("event = %+v, want identity /src/fresh.git by `run start`", reg)
	}

	run, err := db.GetRun(conn, 1)
	testsupport.Must(t, err, "reading the run: %v", err)
	if run.ProjectID == 1 {
		t.Errorf("run started in project %d, want the caller's newly registered one", run.ProjectID)
	}
}
