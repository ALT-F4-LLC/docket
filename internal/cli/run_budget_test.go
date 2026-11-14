package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// TestRunBudgetOwnsItsProject pins `run budget` to the verb layer: the root
// hook leaves project resolution to internal/verb.
func TestRunBudgetOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(runBudgetCmd) {
		t.Error("`run budget` lacks the verbOwnsProject annotation")
	}
}

// runBudgetFlagsCmd is a command carrying `run budget`'s flags over conn, set
// from flags, for a caller with configuration cfg (nil for the default
// project).
func runBudgetFlagsCmd(t *testing.T, conn *sql.DB, cfg *config.Config, flags map[string]string) *cobra.Command {
	t.Helper()
	cmd := cmdWithDB(conn)
	if cfg != nil {
		cmd.SetContext(context.WithValue(cmd.Context(), cfgKey, cfg))
	}
	cmd.Flags().Float64("set", 0, "")
	cmd.Flags().String("reason", "", "")
	addIfVersionFlag(cmd)
	for flag, value := range flags {
		testsupport.Must(t, cmd.Flags().Set(flag, value), "setting --%s", flag)
	}
	return cmd
}

func activeRunIn(t *testing.T, conn *sql.DB) *model.Run {
	t.Helper()
	run, err := db.InsertRun(conn, 1, "a run", 0, 1000)
	testsupport.Must(t, err, "InsertRun: %v", err)
	testsupport.Must(t, db.SetRunStatus(conn, run.ID, model.RunActive, "", 1000), "SetRunStatus")
	fresh, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	return fresh
}

func requireCmdError(t *testing.T, err error, code output.ErrorCode, prefix string) {
	t.Helper()
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != code || !strings.HasPrefix(cmdError.Error(), prefix) {
		t.Errorf("error = %v, want %s starting %q", err, code, prefix)
	}
}

// --set, --reason and --if-version reach the cap change: a stale version is
// CONFLICT and moves nothing, and a current one sets the cap, records the
// reason on the event, and says so.
func TestRunBudgetSetChangesTheCapUnderItsPrecondition(t *testing.T) {
	conn := newTestDB(t)
	run := activeRunIn(t, conn)

	stale := runBudgetFlagsCmd(t, conn, nil, map[string]string{
		"set": "5", "if-version": strconv.Itoa(run.RowVersion + 1),
	})
	w, _ := bufWriter(false)
	requireCmdError(t, runRunBudget(stale, run.Ref(), w), output.ErrConflict, "")

	set := runBudgetFlagsCmd(t, conn, nil, map[string]string{
		"set": "5", "reason": "more room", "if-version": strconv.Itoa(run.RowVersion),
	})
	w, buf := bufWriter(false)
	testsupport.Must(t, runRunBudget(set, run.Ref(), w), "run budget --set")
	if want := run.Ref() + " budget set to 5: more room"; !strings.Contains(buf.String(), want) {
		t.Errorf("output %q does not state %q", buf.String(), want)
	}

	stored, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if stored.Budget != 5 {
		t.Errorf("stored budget = %g, want 5", stored.Budget)
	}
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	events, err := db.RunEventsTx(tx, run.ID, engine.EventRunBudgetSet)
	testsupport.Must(t, err, "RunEventsTx: %v", err)
	var data struct {
		To     float64 `json:"to"`
		Reason string  `json:"reason"`
	}
	if len(events) != 1 {
		t.Fatalf("run-budget-set events = %+v, want one: the stale set moves nothing", events)
	}
	testsupport.Must(t, json.Unmarshal([]byte(events[0].Data), &data), "decoding %q", events[0].Data)
	if data.To != 5 || data.Reason != "more room" {
		t.Errorf("event data = %+v, want a cap of 5 and the reason", data)
	}
}

// An invalid --if-version is reported after the caller's project and the run
// reference, the order `run budget --set` reported them in before the verb
// layer, and alone it is the VALIDATION fault.
func TestRunBudgetReportsTheIfVersionAfterTheProjectAndTheRun(t *testing.T) {
	conn := newTestDB(t)
	run := activeRunIn(t, conn)
	badVersion := map[string]string{"set": "5", "if-version": "0"}

	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		ref    string
		prefix string
	}{
		{"unanchored caller", &config.Config{Identity: "/work/scratch"}, run.Ref(),
			"cannot tell which project this belongs to"},
		{"malformed run ID", nil, "RUN-x", `invalid run ID "RUN-x"`},
		{"bad version alone", nil, run.Ref(), "--if-version must be >= 1, got 0 (versions start at 1)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := bufWriter(false)
			err := runRunBudget(runBudgetFlagsCmd(t, conn, tc.cfg, badVersion), tc.ref, w)
			requireCmdError(t, err, output.ErrValidation, tc.prefix)
		})
	}
	stored, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if stored.Budget != 0 {
		t.Errorf("stored budget = %g after refused sets, want 0", stored.Budget)
	}
}
