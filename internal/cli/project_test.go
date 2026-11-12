package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestProjectCommandsOwnTheirProject pins the project commands to the verb
// layer: the root hook leaves project resolution to internal/verb for each.
func TestProjectCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{projectListCmd, projectSetPrefixCmd, projectDeleteCmd} {
		if _, ok := cmd.Annotations["verbOwnsProject"]; !ok {
			t.Errorf("`project %s` lacks the verbOwnsProject annotation", cmd.Name())
		}
	}
}

// TestProjectSetPrefixThenListThroughTheRoot drives both commands through the
// real root hook, which no longer resolves their project: set-prefix must
// still change the caller's own project, and list must still mark it current.
func TestProjectSetPrefixThenListThroughTheRoot(t *testing.T) {
	clientConfigSandbox(t)
	t.Cleanup(func() { model.SetDisplayPrefix("DKT") })
	_, err := runRootVerb(t, initCmd, "init")
	testsupport.Must(t, err, "docket init: %v", err)

	out, err := runRootVerb(t, projectSetPrefixCmd, "project", "set-prefix", " abc ")
	testsupport.Must(t, err, "docket project set-prefix: %v", err)
	if want := "Issues here now read as ABC-N (the number stays the store-wide id)"; !strings.Contains(out, want) {
		t.Errorf("set-prefix printed %q, want it to contain %q", out, want)
	}

	out, err = runRootVerb(t, projectListCmd, "project", "list", "--json")
	testsupport.Must(t, err, "docket project list: %v", err)
	var env struct {
		Data struct {
			Projects []projectView `json:"items"`
			Total    int           `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, out)
	}
	var current []projectView
	for _, p := range env.Data.Projects {
		if p.Current {
			current = append(current, p)
		}
	}
	if len(current) != 1 || current[0].Prefix != "ABC" {
		t.Errorf("current projects = %+v, want exactly the caller's, prefixed ABC", current)
	}
	if env.Data.Total != len(env.Data.Projects) {
		t.Errorf("total = %d, want %d", env.Data.Total, len(env.Data.Projects))
	}
}

// TestProjectDeleteRefusesTheDefaultProjectThroughTheRoot keeps the refusal's
// code and text unchanged on the verb path.
func TestProjectDeleteRefusesTheDefaultProjectThroughTheRoot(t *testing.T) {
	clientConfigSandbox(t)
	_, err := runRootVerb(t, initCmd, "init")
	testsupport.Must(t, err, "docket init: %v", err)

	_, err = runRootVerb(t, projectDeleteCmd, "project", "delete", "1")
	var ce *CmdError
	if !errors.As(err, &ce) || ce.Code != output.ErrValidation {
		t.Fatalf("project delete 1: error = %v, want a %s CmdError", err, output.ErrValidation)
	}
	if !strings.Contains(err.Error(), "is the default project and cannot be deleted") {
		t.Errorf("error text %q lost the default-project refusal", err.Error())
	}
}

// TestProjectSetPrefixRecordsTheRegistrationItMakes covers the one write the
// root hook used to make for these commands: a project registered on first
// contact gets its project-registered event, attributed to the command.
func TestProjectSetPrefixRecordsTheRegistrationItMakes(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/src/first.git", "first.git", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	t.Cleanup(func() { model.SetDisplayPrefix("DKT") })

	cmd := cmdWithDB(conn)
	cmd.Use = "set-prefix"
	group := &cobra.Command{Use: "project"}
	group.AddCommand(cmd)
	(&cobra.Command{Use: "docket"}).AddCommand(group)
	cmd.SetContext(context.WithValue(cmd.Context(), cfgKey,
		&config.Config{Identity: "/src/fresh.git", Anchored: true}))

	before := registeredEventCount(t, conn)
	stdout := captureStdout(t)
	err = projectSetPrefixCmd.RunE(cmd, []string{"FRS"})
	stdout()
	testsupport.Must(t, err, "project set-prefix FRS: %v", err)

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
	if reg.Identity != "/src/fresh.git" || reg.Verb != "project set-prefix" {
		t.Errorf("event = %+v, want identity /src/fresh.git by `project set-prefix`", reg)
	}
}
