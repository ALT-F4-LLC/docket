package cli

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// guardCfg is an anchored project identity no project row holds yet.
func guardCfg(t *testing.T) *config.Config {
	return &config.Config{Identity: filepath.Join(t.TempDir(), "repo"), Anchored: true}
}

func requireCmdErrCode(t *testing.T, err error, code output.ErrorCode, want string) {
	t.Helper()
	var ce *CmdError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *CmdError", err, err)
	}
	if ce.Code != code {
		t.Errorf("code = %s, want %s (err: %v)", ce.Code, code, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to contain %q", err, want)
	}
}

func TestGuardCommandsResolveTheirOwnProject(t *testing.T) {
	for _, cmd := range guardCmd.Commands() {
		if !verbOwnsProject(cmd) {
			t.Errorf("guard %s is not annotated verbOwnsProject", cmd.Name())
		}
	}
}

func TestGuardAllowRendersAllowedTrue(t *testing.T) {
	conn := newTestDB(t)
	out, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardStopCmd)
	testsupport.Must(t, err, "guard stop: %v", err)
	if !strings.Contains(out, `"data":{"allowed":true}`) {
		t.Errorf("guard stop output = %s, want data {\"allowed\":true}", out)
	}
}

// A denial rides the error channel with NOT_FOUND's code, which exits 2.
func TestGuardDenialIsANotFoundError(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, guardGateCmd.Flags().Set("step", "approve"), "set --step")
	_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardGateCmd)
	requireCmdErrCode(t, err, output.ErrNotFound, "approve")
}

// --active refuses --rows before reading it, so a missing file is a
// validation error rather than NOT_FOUND, and stdin is never read.
func TestGuardSpawnActiveRefusesRowsWithoutReadingThem(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, guardSpawnCmd.Flags().Set("active", "true"), "set --active")
	testsupport.Must(t, guardSpawnCmd.Flags().Set("rows", filepath.Join(t.TempDir(), "absent.json")), "set --rows")
	_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardSpawnCmd)
	requireCmdErrCode(t, err, output.ErrValidation, "--active does not take --rows")
}

// setGuardSpawnFlags sets flags on guard spawn for one test. The --ack-reap
// slice is emptied afterwards, because pflag cannot reset a slice from its
// default's string form.
func setGuardSpawnFlags(t *testing.T, flags map[string]string) {
	t.Helper()
	t.Cleanup(func() {
		_ = guardSpawnCmd.Flags().Lookup("ack-reap").Value.(pflag.SliceValue).Replace(nil)
	})
	for name, value := range flags {
		testsupport.Must(t, guardSpawnCmd.Flags().Set(name, value), "set --%s", name)
	}
}

// The --run path refuses a missing or malformed --run, and a non-seq
// --ack-reap, before it reads --rows: an unreadable file does not turn the
// refusal into NOT_FOUND, and stdin is left unread.
func TestGuardSpawnRefusesTheRunBeforeReadingRows(t *testing.T) {
	cases := []struct {
		name  string
		flags map[string]string
		want  string
	}{
		{"no run", map[string]string{}, "--run is required"},
		{"a malformed run", map[string]string{"run": "nope"}, "invalid run ID"},
		{"a non-seq acknowledgment", map[string]string{"run": "RUN-1", "ack-reap": "0"},
			"--ack-reap 0 is not an event seq"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" with an absent rows file", func(t *testing.T) {
			conn := newTestDB(t)
			setGuardSpawnFlags(t, tc.flags)
			setGuardSpawnFlags(t, map[string]string{"rows": filepath.Join(t.TempDir(), "absent.json")})
			_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardSpawnCmd)
			requireCmdErrCode(t, err, output.ErrValidation, tc.want)
		})
		t.Run(tc.name+" with rows on stdin", func(t *testing.T) {
			conn := newTestDB(t)
			const batch = "[]"
			stdin := strings.NewReader(batch)
			guardSpawnCmd.SetIn(stdin)
			t.Cleanup(func() { guardSpawnCmd.SetIn(nil) })
			setGuardSpawnFlags(t, tc.flags)
			setGuardSpawnFlags(t, map[string]string{"rows": "-"})
			_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardSpawnCmd)
			requireCmdErrCode(t, err, output.ErrValidation, tc.want)
			if stdin.Len() != len(batch) {
				t.Errorf("stdin has %d of %d bytes left, want it unread", stdin.Len(), len(batch))
			}
		})
	}
}

// Once the run is well formed the rows are read before the engine looks the
// run up and before --deciding-vote is parsed, so an unreadable file is the
// error those inputs report.
func TestGuardSpawnReadsRowsBeforeTheRunLookupAndTheDecidingVote(t *testing.T) {
	cases := map[string]map[string]string{
		"a run nobody started":      {"run": "RUN-999"},
		"a malformed deciding vote": {"run": "RUN-1", "deciding-vote": "nope"},
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			conn := newTestDB(t)
			setGuardSpawnFlags(t, flags)
			setGuardSpawnFlags(t, map[string]string{"rows": filepath.Join(t.TempDir(), "absent.json")})
			_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardSpawnCmd)
			requireCmdErrCode(t, err, output.ErrNotFound, "reading proposed rows")
		})
	}
}

// guard stop answers for the caller's project, and --all-projects widens it to
// the store: a dispatched run in the default project does not block a caller
// whose identity no project holds until the flag asks about every project.
func TestGuardStopAllProjectsWidensToTheStore(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	_, err := engine.NewEngine().OpenDispatch(conn, runID, 0, nil, model.NowMS())
	testsupport.Must(t, err, "dispatch open: %v", err)
	cfg := guardCfg(t)

	out, err := runLeaseCommandOutput(t, conn, cfg, guardStopCmd)
	testsupport.Must(t, err, "guard stop: %v", err)
	if !strings.Contains(out, `"data":{"allowed":true}`) {
		t.Errorf("guard stop output = %s, want another project's run to leave it allowed", out)
	}

	testsupport.Must(t, guardStopCmd.Flags().Set("all-projects", "true"), "set --all-projects")
	_, err = runLeaseCommandOutput(t, conn, cfg, guardStopCmd)
	requireCmdErrCode(t, err, output.ErrNotFound, "first@0")
}

// guardReapWorkflow bounds its write class to one step, so a reaped claim on
// "first" opens a reap hold.
const guardReapWorkflow = `
[pipeline]
name = "unit-guard-reap"
version = 1
[match]
kind = ["task"]
[limits]
write = { max = 1 }
[[step]]
name = "first"
after = []
executor = "someone"
class = "write"
emits = "result"
`

// reapHeldRunForCLI activates a guardReapWorkflow run, claims "first" with a
// lease that has already lapsed, and lets `next` reap it. It returns the run
// reference.
func reapHeldRunForCLI(t *testing.T, conn *sql.DB) string {
	t.Helper()
	registerForRun(t, conn, guardReapWorkflow)
	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "held", Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)
	run, err := db.InsertRun(conn, store.DefaultProjectID, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "adding issue to run")
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "activate: %v", err)

	steps, err := store.StoreOn(conn).ListRunSteps(run.ID)
	testsupport.Must(t, err, "listing steps: %v", err)
	now := model.NowMS()
	for _, s := range steps {
		if s.Instance != "first@0" {
			continue
		}
		_, err = engine.ClaimStep(conn, s.ID, engine.ClaimOptions{
			Owner: "crashed-writer", NowMS: now - time.Hour.Milliseconds(),
		})
		testsupport.Must(t, err, "claim: %v", err)
		_, err = engine.NewEngine().NextSteps(conn, run.ID, 0, now)
		testsupport.Must(t, err, "next past the lease: %v", err)
		return model.FormatRunID(run.ID)
	}
	t.Fatalf("no first@0 step among %d steps", len(steps))
	return ""
}

// --usage-join reaches the verb: under a reap hold the plain spawn denies and
// the usage join is admitted.
func TestGuardSpawnUsageJoinAdmitsPastAReapHold(t *testing.T) {
	conn := newTestDB(t)
	run := reapHeldRunForCLI(t, conn)
	cfg := guardCfg(t)

	setGuardSpawnFlags(t, map[string]string{"run": run})
	_, err := runLeaseCommandOutput(t, conn, cfg, guardSpawnCmd)
	requireCmdErrCode(t, err, output.ErrNotFound, "--ack-reap")

	setGuardSpawnFlags(t, map[string]string{"run": run, "usage-join": "true"})
	out, err := runLeaseCommandOutput(t, conn, cfg, guardSpawnCmd)
	testsupport.Must(t, err, "guard spawn --usage-join: %v", err)
	if !strings.Contains(out, `"data":{"allowed":true}`) {
		t.Errorf("guard spawn --usage-join output = %s, want allowed", out)
	}
}

// breakProjectLookup leaves the store open but makes its project lookup fail,
// as a query against a store that opened and then cannot be read does.
func breakProjectLookup(t *testing.T, conn *sql.DB) {
	t.Helper()
	_, err := conn.Exec(`ALTER TABLE projects RENAME TO projects_unreadable`)
	testsupport.Must(t, err, "renaming projects: %v", err)
}

// guard spawn resolves the caller's project before it checks --run and
// --ack-reap or reads --rows, as the root hook did before every flag: a store
// it cannot consult is a guard UNAVAILABLE denial however malformed the
// request, and stdin is left unread.
func TestGuardSpawnResolvesTheProjectBeforeTheRequest(t *testing.T) {
	requests := map[string]map[string]string{
		"no run":                   {},
		"a malformed run":          {"run": "nope"},
		"a non-seq acknowledgment": {"run": "RUN-1", "ack-reap": "0"},
		"a well-formed run":        {"run": "RUN-1"},
	}
	for name, flags := range requests {
		t.Run(name+" with an absent rows file", func(t *testing.T) {
			conn := newTestDB(t)
			breakProjectLookup(t, conn)
			setGuardSpawnFlags(t, flags)
			setGuardSpawnFlags(t, map[string]string{"rows": filepath.Join(t.TempDir(), "absent.json")})
			_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardSpawnCmd)
			if guardUnavailable(err) == nil || !strings.Contains(err.Error(), "resolving the project") {
				t.Errorf("err = %v, want the project lookup's failure as a guard UNAVAILABLE denial", err)
			}
		})
		t.Run(name+" with rows on stdin", func(t *testing.T) {
			conn := newTestDB(t)
			breakProjectLookup(t, conn)
			const batch = "[]"
			stdin := strings.NewReader(batch)
			guardSpawnCmd.SetIn(stdin)
			t.Cleanup(func() { guardSpawnCmd.SetIn(nil) })
			setGuardSpawnFlags(t, flags)
			setGuardSpawnFlags(t, map[string]string{"rows": "-"})
			_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardSpawnCmd)
			if guardUnavailable(err) == nil || !strings.Contains(err.Error(), "resolving the project") {
				t.Errorf("err = %v, want the project lookup's failure as a guard UNAVAILABLE denial", err)
			}
			if stdin.Len() != len(batch) {
				t.Errorf("stdin has %d of %d bytes left, want it unread", stdin.Len(), len(batch))
			}
		})
	}
}

// --all-projects reaches the verb on guard gate, guard record and guard spawn
// --active: a caller whose identity no project holds gets its own project's
// answer over the default project's run until the flag asks about the store.
func TestGuardAllProjectsWidensGateRecordAndActiveSpawn(t *testing.T) {
	dispatched := func(t *testing.T, conn *sql.DB) {
		runID := activatedDispatchRunForCLI(t, conn)
		_, err := engine.NewEngine().OpenDispatch(conn, runID, 0, nil, model.NowMS())
		testsupport.Must(t, err, "dispatch open: %v", err)
	}
	reapHeld := func(t *testing.T, conn *sql.DB) { reapHeldRunForCLI(t, conn) }
	cases := []struct {
		name  string
		cmd   *cobra.Command
		flags map[string]string
		seed  func(*testing.T, *sql.DB)
		own   string // the caller's own-project denial, or "" for an allow
		store string // the store-wide denial
	}{
		{"gate", guardGateCmd, map[string]string{"step": "second"}, dispatched,
			"no `type=\"human\"`", "not approved"},
		{"record", guardRecordCmd, nil, dispatched, "", "dispatch"},
		{"spawn --active", guardSpawnCmd, map[string]string{"active": "true"}, reapHeld,
			"", "--ack-reap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := newTestDB(t)
			tc.seed(t, conn)
			cfg := guardCfg(t)
			for name, value := range tc.flags {
				testsupport.Must(t, tc.cmd.Flags().Set(name, value), "set --%s", name)
			}

			out, err := runLeaseCommandOutput(t, conn, cfg, tc.cmd)
			if tc.own == "" {
				testsupport.Must(t, err, "guard %s: %v", tc.name, err)
				if !strings.Contains(out, `"data":{"allowed":true}`) {
					t.Errorf("guard %s output = %s, want another project's run to leave it allowed", tc.name, out)
				}
			} else {
				requireCmdErrCode(t, err, output.ErrNotFound, tc.own)
			}

			testsupport.Must(t, tc.cmd.Flags().Set("all-projects", "true"), "set --all-projects")
			_, err = runLeaseCommandOutput(t, conn, cfg, tc.cmd)
			requireCmdErrCode(t, err, output.ErrNotFound, tc.store)
		})
	}
}
