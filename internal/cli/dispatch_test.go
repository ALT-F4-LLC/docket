package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// dispatch verify has no CLI-level test pre-fix, so the surface where DKT-10
// was originally reported stayed unasserted; these exercise runDispatchVerify
// directly: exit code, --json rendering, and a mismatch's rendered content.

func dispatchCmdWithDB(conn *sql.DB, runRef string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("run", runRef, "")
	cmd.Flags().Int("limit", 0, "")
	cmd.Flags().Int64Slice("ack-reap", nil, "")
	return cmd
}

func activatedDispatchRunForCLI(t *testing.T, conn *sql.DB) int {
	t.Helper()
	runID, _ := seedRun(t, conn)
	if _, err := engine.Activate(conn, runID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	return runID
}

func TestDispatchVerifyCLIEqualExitsClean(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)

	_, err := engine.NewEngine().OpenDispatch(conn, runID, 0, nil, model.NowMS())
	testsupport.Must(t, err, "dispatch open: %v", err)

	w, buf := bufWriter(true)
	err = runDispatchVerify(dispatchCmdWithDB(conn, runRef), w)
	testsupport.Must(t, err, "dispatch verify: %v", err)

	var env struct {
		Data struct {
			Verified bool   `json:"verified"`
			Dispatch string `json:"dispatch"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if !env.Data.Verified {
		t.Errorf("verified = false on an untouched manifest:\n%s", buf.String())
	}
	if env.Data.Dispatch == "" {
		t.Errorf("the payload does not name the dispatch:\n%s", buf.String())
	}
}

func TestDispatchVerifyCLIConflictRendersRowsAndExitCode(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)

	manifest, err := engine.NewEngine().OpenDispatch(conn, runID, 0, nil, model.NowMS())
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(manifest.Rows) == 0 {
		t.Fatal("premise: the manifest must have rows")
	}

	stepID, err := model.ParseStepID(manifest.Rows[0].Step)
	testsupport.Must(t, err, "parsing %q: %v", manifest.Rows[0].Step, err)
	_, err = engine.ClaimStep(conn, stepID, engine.ClaimOptions{
		Owner: "worker", NowMS: model.NowMS(),
	})
	testsupport.Must(t, err, "claim: %v", err)

	w, buf := bufWriter(true)
	err = runDispatchVerify(dispatchCmdWithDB(conn, runRef), w)
	if err == nil {
		t.Fatal("a claimed manifest row verified clean")
	}
	cerr, ok := err.(*CmdError)
	if !ok {
		t.Fatalf("error is %T, want *CmdError: %v", err, err)
	}
	if cerr.Code != output.ErrConflict {
		t.Errorf("code = %q, want %q", cerr.Code, output.ErrConflict)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused verify wrote to the success channel: %s", buf.String())
	}

	got := cerr.Error()
	// A claimed (not completed) step is still NON-TERMINAL and yet no longer
	// ready, so it vanishes from the recomputed set entirely — the
	// missing-row branch, rendered as an explicit absence rather than a
	// blank line, and the case renderRowOrAbsent's doc now calls out as the
	// alarming one (a completed step never reaches here at all).
	for _, want := range []string{
		"manifest row 0", manifest.Rows[0].Instance, "(no row at this position)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal %q does not mention %q", got, want)
		}
	}
}
