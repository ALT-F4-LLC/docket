package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
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

// withFreshAnchoredProject claims the default project for another checkout
// and gives cmd an anchored identity no project holds yet, so the command's
// project resolution registers one.
func withFreshAnchoredProject(t *testing.T, conn *sql.DB, cmd *cobra.Command) {
	t.Helper()
	_, err := db.EnsureProject(conn, "/src/first.git", "first.git", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	t.Cleanup(func() { model.SetDisplayPrefix("DKT") })
	cmd.SetContext(context.WithValue(cmd.Context(), cfgKey,
		&config.Config{Identity: "/src/fresh.git", Anchored: true}))
}

// requireOneRegistration asserts that one project-registered event was logged
// after before, naming the fresh identity and verb.
func requireOneRegistration(t *testing.T, conn *sql.DB, before int, verb string) {
	t.Helper()
	if got := registeredEventCount(t, conn); got != before+1 {
		t.Fatalf("logged %d project-registered event(s), want 1", got-before)
	}
	var data string
	err := conn.QueryRow(`SELECT data FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 1`,
		engine.EventProjectRegistered).Scan(&data)
	testsupport.Must(t, err, "reading the event: %v", err)
	var reg struct {
		Identity string `json:"identity"`
		Verb     string `json:"verb"`
	}
	if err := json.Unmarshal([]byte(data), &reg); err != nil {
		t.Fatalf("decoding the event %s: %v", data, err)
	}
	if reg.Identity != "/src/fresh.git" || reg.Verb != verb {
		t.Errorf("event = %+v, want identity /src/fresh.git by `%s`", reg, verb)
	}
}

// TestRunStartRecordsTheRegistrationItMakes covers the write the root hook
// used to make for `run start`: a project registered on first contact gets
// its project-registered event, attributed to the command.
func TestRunStartRecordsTheRegistrationItMakes(t *testing.T) {
	conn := newTestDB(t)
	cmd := runStartCmdWithDB(conn)
	withFreshAnchoredProject(t, conn, cmd)

	before := registeredEventCount(t, conn)
	w, _ := bufWriter(true)
	err := runRunStart(cmd, w)
	testsupport.Must(t, err, "run start: %v", err)
	requireOneRegistration(t, conn, before, "run start")

	run, err := db.GetRun(conn, 1)
	testsupport.Must(t, err, "reading the run: %v", err)
	if run.ProjectID == 1 {
		t.Errorf("run started in project %d, want the caller's newly registered one", run.ProjectID)
	}
}

// `run activate` and `run conduct` register the caller's project before they
// look the run up, so the event is recorded even when the run is missing.
func TestRunActivateAndConductRecordTheRegistrationTheyMake(t *testing.T) {
	for name, tc := range map[string]struct {
		cmd func(conn *sql.DB) *cobra.Command
		run func(cmd *cobra.Command, w *output.Writer) error
	}{
		"run activate": {
			cmd: func(conn *sql.DB) *cobra.Command {
				cmd := newRunActivateCmd()
				cmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
				return cmd
			},
			run: func(cmd *cobra.Command, w *output.Writer) error {
				return runRunActivate(cmd, []string{"RUN-42"}, w)
			},
		},
		"run conduct": {
			cmd: cmdWithDB,
			run: func(cmd *cobra.Command, w *output.Writer) error {
				return runRunConduct(cmd, []string{"RUN-42"}, w)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			conn := newTestDB(t)
			cmd := tc.cmd(conn)
			withFreshAnchoredProject(t, conn, cmd)

			before := registeredEventCount(t, conn)
			w, _ := bufWriter(true)
			err := tc.run(cmd, w)
			if err == nil {
				t.Fatalf("`%s RUN-42` succeeded on a missing run", name)
			}
			if got := codeOf(t, err); got != output.ErrNotFound {
				t.Errorf("code = %q, want %q", got, output.ErrNotFound)
			}
			requireOneRegistration(t, conn, before, name)
		})
	}
}

// A negative --budget is reported ahead of an unreadable request file.
func TestRunStartReportsANegativeBudgetBeforeAMissingRequestFile(t *testing.T) {
	conn := newTestDB(t)
	cmd := runStartCmdWithDB(conn)
	for flag, value := range map[string]string{
		"budget":       "-1",
		"request-file": filepath.Join(t.TempDir(), "absent.md"),
	} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatalf("setting --%s: %v", flag, err)
		}
	}
	w, _ := bufWriter(true)

	err := runRunStart(cmd, w)
	if err == nil {
		t.Fatal("run start succeeded on a negative budget")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("code = %q, want %q (%v)", got, output.ErrValidation, err)
	}
	if !strings.Contains(err.Error(), "--budget must be >= 0") {
		t.Errorf("error = %v, want the negative --budget named", err)
	}
}

// The run records the head of the checkout it was started from.
func TestRunStartRecordsTheCheckoutHead(t *testing.T) {
	conn := newTestDB(t)
	checkout := t.TempDir()
	repinGit(t, checkout, "init", "-q", "-b", "trunk")
	repinGit(t, checkout, "commit", "-q", "--allow-empty", "-m", "base")
	commit := repinGit(t, checkout, "rev-parse", "HEAD")

	cmd := runStartCmdWithDB(conn)
	cmd.SetContext(context.WithValue(cmd.Context(), cfgKey, &config.Config{ExecRoot: checkout}))
	w, _ := bufWriter(true)
	err := runRunStart(cmd, w)
	testsupport.Must(t, err, "run start: %v", err)

	run, err := db.GetRun(conn, 1)
	testsupport.Must(t, err, "reading the run: %v", err)
	if run.ExecRoot != checkout || run.Branch != "trunk" || run.CommitSHA != commit {
		t.Errorf("run context = %q %q %q, want %s on trunk at %s",
			run.ExecRoot, run.Branch, run.CommitSHA, checkout, commit)
	}
}
