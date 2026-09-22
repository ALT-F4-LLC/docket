package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `docket report executors` — the cross-run ledger at the CLI boundary
// (DKT-2453). The grouping itself is the engine's to prove; what these pin is
// the window flag, the document shape, and the rendered page.

// TestParseLedgerSince pins the two forms --since accepts and the refusal of
// anything else, each naming the flag.
func TestParseLedgerSince(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		wantRun   int
		wantMS    int64
		wantLabel string
	}{
		{"", 0, 0, ""},
		{"RUN-12", 12, 0, "RUN-12"},
		{"run-12", 12, 0, "RUN-12"},
		{"12", 12, 0, "RUN-12"},
		{"2026-09-01", 0, 1_788_220_800_000, "2026-09-01T00:00:00Z"},
		{"2026-09-01T10:30:00Z", 0, 1_788_258_600_000, "2026-09-01T10:30:00Z"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			run, ms, label, err := parseLedgerSince(tc.raw)
			testsupport.Must(t, err, "parseLedgerSince(%q): %v", tc.raw, err)
			if run != tc.wantRun || ms != tc.wantMS || label != tc.wantLabel {
				t.Errorf("parseLedgerSince(%q) = (%d, %d, %q), want (%d, %d, %q)",
					tc.raw, run, ms, label, tc.wantRun, tc.wantMS, tc.wantLabel)
			}
		})
	}
	for _, raw := range []string{"yesterday", "RUN-0", "2026-13-01", "-5"} {
		t.Run(raw, func(t *testing.T) {
			_, _, _, err := parseLedgerSince(raw)
			if err == nil {
				t.Fatalf("parseLedgerSince(%q) accepted", raw)
			}
			if !strings.Contains(err.Error(), "--since") {
				t.Errorf("refusal %q does not name the flag", err)
			}
		})
	}
}

// TestReportExecutorsOnAnEmptyStore: the verb answers on a store with no
// runs — zero runs, empty arrays rather than nulls, the project scope by
// default — and refuses a malformed window as VALIDATION_ERROR.
func TestReportExecutorsOnAnEmptyStore(t *testing.T) {
	conn := newTestDB(t)
	cmd := cmdWithDB(conn)
	cmd.Flags().String("since", "", "")
	cmd.Flags().Bool("all-projects", false, "")
	w, buf := bufWriter(true)

	err := runReportExecutors(cmd, w)
	testsupport.Must(t, err, "report executors: %v", err)
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Runs      int                        `json:"runs"`
			Scope     string                     `json:"scope"`
			Since     *string                    `json:"since"`
			Executors []engine.ExecutorLedgerRow `json:"executors"`
			Voters    []engine.VoterLedgerRow    `json:"voters"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope), "decoding: %s", buf.String())
	if !envelope.OK || envelope.Data.Runs != 0 || envelope.Data.Scope != "project" {
		t.Errorf("document = %s, want ok, zero runs, project scope", buf.String())
	}
	if !strings.Contains(buf.String(), `"executors":[]`) || !strings.Contains(buf.String(), `"voters":[]`) {
		t.Errorf("an empty ledger must carry empty arrays, not nulls: %s", buf.String())
	}
	if envelope.Data.Since != nil {
		t.Errorf("since = %q on an unbounded window, want the key absent", *envelope.Data.Since)
	}

	cmd.Flags().Set("since", "last tuesday")
	err = runReportExecutors(cmd, w)
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrValidation {
		t.Errorf("a malformed --since returned %v, want VALIDATION_ERROR", err)
	}
}

// TestReportExecutorsFromAnUnboundDirectoryFailsWithoutAllProjects: a cwd
// whose identity resolves to db.UnregisteredProjectID (no registered docket
// project) must refuse the project-scoped read rather than answer an empty
// ledger as success — the failure scenario is an operator in the wrong
// directory reading "no executors" instead of an error.
func TestReportExecutorsFromAnUnboundDirectoryFailsWithoutAllProjects(t *testing.T) {
	conn := newTestDB(t)
	cmd := cmdWithDB(conn)
	cmd.Flags().String("since", "", "")
	cmd.Flags().Bool("all-projects", false, "")
	cmd.SetContext(context.WithValue(cmd.Context(), projectKey, db.UnregisteredProjectID))
	w, buf := bufWriter(true)

	err := runReportExecutors(cmd, w)

	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrValidation {
		t.Fatalf("runReportExecutors from an unbound directory = %v, want a VALIDATION_ERROR CmdError", err)
	}
	cwd, _ := os.Getwd()
	if !strings.Contains(cmdError.Error(), cwd) {
		t.Errorf("refusal %q does not name the working directory %q", cmdError.Error(), cwd)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused read must emit no ledger, got %s", buf.String())
	}
}

// TestReportExecutorsAllProjectsIgnoresAnUnboundInvokingDirectory: with
// --all-projects, an unbound invoking directory must not block the read —
// the scope is the whole store, so the invocation's own missing project
// binding is irrelevant, and runs across every project still count.
func TestReportExecutorsAllProjectsIgnoresAnUnboundInvokingDirectory(t *testing.T) {
	conn := newTestDB(t)
	projectID, err := db.EnsureProject(conn, "/src/here.git", "here.git", 1)
	testsupport.Must(t, err, "registering the project: %v", err)
	_, err = db.InsertRun(conn, projectID, "here", 0, 1)
	testsupport.Must(t, err, "starting a run: %v", err)

	cmd := cmdWithDB(conn)
	cmd.Flags().String("since", "", "")
	cmd.Flags().Bool("all-projects", false, "")
	testsupport.Must(t, cmd.Flags().Set("all-projects", "true"), "setting --all-projects: %v", err)
	cmd.SetContext(context.WithValue(cmd.Context(), projectKey, db.UnregisteredProjectID))
	w, buf := bufWriter(true)

	err = runReportExecutors(cmd, w)
	testsupport.Must(t, err, "report executors --all-projects: %v", err)

	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Runs  int    `json:"runs"`
			Scope string `json:"scope"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope), "decoding: %s", buf.String())
	if !envelope.OK || envelope.Data.Scope != "store" || envelope.Data.Runs != 1 {
		t.Errorf("document = %s, want ok, store scope, 1 run", buf.String())
	}
}

// TestExecutorLedgerRendersBothTables: the human page names every hint and
// voter with its columns, and an empty window says so rather than printing
// nothing.
func TestExecutorLedgerRendersBothTables(t *testing.T) {
	r := reportExecutorsResult{
		ExecutorLedger: &engine.ExecutorLedger{
			Runs: 3,
			Executors: []engine.ExecutorLedgerRow{{
				Executor: "judge-security", Runs: 3, Steps: 5, FixLoopRoutes: 2,
				OverridePasses: 1, Reaps: 1, ForcedReaps: 1,
				UniqueClusters: 4, CorroboratedClusters: 6, HeldClusters: 1,
			}},
			Voters: []engine.VoterLedgerRow{{
				Voter: "seat-security", Runs: 3, Casts: 3, Approve: 1,
				ApproveWithConcerns: 1, Reject: 1, FixLoopRoutes: 1,
			}},
		},
		Scope: "project", Since: "RUN-40",
	}
	out := renderPlainExecutorLedger(r)
	for _, needle := range []string{
		"Window", "Runs:", "3", "RUN-40", "Executors", "judge-security",
		"steps 5", "fix-loop 2", "override-pass 1", "reaps 1 (forced 1)",
		"unique 4 / corroborated 6 / held 1",
		"Voters", "seat-security", "casts 3 (approve 1, approve-with-concerns 1, reject 1)",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("the page never says %q:\n%s", needle, out)
		}
	}

	empty := renderPlainExecutorLedger(reportExecutorsResult{
		ExecutorLedger: &engine.ExecutorLedger{}, Scope: "store",
	})
	if !strings.Contains(empty, "no steps or casts in the window") {
		t.Errorf("an empty window renders as:\n%s", empty)
	}
}
