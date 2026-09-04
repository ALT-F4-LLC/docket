package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

// The `run` verbs' exit codes and machine-readable codes, per the §5.5
// taxonomy. The engine's own behavior is tested in internal/engine; what these
// assert is the CLI boundary — that each engine failure reaches the operator as
// the RIGHT code, since an error taxonomy that degrades to "general error" at
// the boundary is one no script can branch on.

// runWorkflow is a two-step definition that binds `task` issues, small enough
// that a test's intent is not buried in grammar.
const runWorkflow = `
[pipeline]
name = "unit-run"
version = 1
[match]
kind = ["task"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
[[step]]
name = "second"
after = ["first"]
type = "human"
on_fail = "skip"
`

func registerForRun(t *testing.T, conn *sql.DB, src string) *model.Workflow {
	t.Helper()

	def, err := workflow.Load([]byte(src))
	testsupport.Must(t, err, "loading definition: %v", err)
	parsed, err := workflow.Canonical(def)
	testsupport.Must(t, err, "serializing definition: %v", err)
	stored, _, err := db.InsertWorkflow(conn, &model.Workflow{
		Name: def.Pipeline.Name, Version: def.Pipeline.Version,
		SourceSHA256: workflow.SHA256([]byte(src)),
		Body:         src, Parsed: string(parsed),
	}, model.NowMS())
	testsupport.Must(t, err, "registering definition: %v", err)
	return stored
}

// runStartCmdWithDB builds a `run start` command with its flags.
func runStartCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("request-file", "", "")
	cmd.Flags().Float64("budget", 0, "")
	cmd.Flags().StringSlice("issue", nil, "")
	addIdempotencyKeyFlag(cmd)
	return cmd
}

// runActivateWithWriter drives runRunActivate against a FACTORY-built
// command — newRunActivateCmd's real flag registration, with the args parsed
// by cobra's own parser — and the caller's writer injected, for tests that
// read the envelope or stderr. runActivateViaCLI is the pure-CLI twin; this
// one exists because getWriter targets process stdout and these assertions
// need a buffer. It replaces the retired runActivateCmdWithDB stand-in,
// whose hand-built flag set drifted from the real registration (DKT-69: a
// dead --reason copy outlived the factory extraction, and --dry-run was
// being re-registered ad hoc at call sites).
func runActivateWithWriter(t *testing.T, conn *sql.DB, w *output.Writer, args ...string) error {
	t.Helper()
	cmd := newRunActivateCmd()
	cmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return runRunActivate(cmd, cmd.Flags().Args(), w)
}

func runStatusCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().Bool("active", false, "")
	cmd.Flags().Int("limit", 50, "")
	return cmd
}

func runMoveCmdWithDB(conn *sql.DB, name string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Use = name
	cmd.Flags().String("reason", "", "")
	return cmd
}

// seedRun registers the workflow, creates a task issue, and starts a run over
// it — the state every activation test begins from.
func seedRun(t *testing.T, conn *sql.DB) (runID, issueID int) {
	t.Helper()

	registerForRun(t, conn, runWorkflow)

	id, err := db.CreateIssue(conn, &model.Issue{
		Title: "activate me", Description: "a body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)

	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	err = db.AddRunIssue(conn, run.ID, id)
	testsupport.Must(t, err, "adding issue to run: %v", err)
	return run.ID, id
}

// TestRunStartStoresBudget covers the "accepted and stored, enforces nothing"
// rule. Accepting the flag now means a later release adds enforcement rather
// than a flag — and a flag appearing later would break the invocation a
// harness scripted today.
// TestRunStartRefusesCrossProjectIssue pins DKT-21 at the start edge: the run
// would be created in the invoking project, and an --issue homed elsewhere is
// refused BEFORE the run is created, so no empty run is left behind.
func TestRunStartRefusesCrossProjectIssue(t *testing.T) {
	conn := newTestDB(t)
	other := otherProject(t, conn)
	foreign := createIssueInProject(t, conn, other, "foreign")

	cmd := runStartCmdWithDB(conn)
	err := cmd.Flags().Set("issue", model.FormatID(foreign))
	testsupport.Must(t, err, "setting --issue: %v", err)
	w, _ := bufWriter(true)

	err = runRunStart(cmd, w)
	if err == nil {
		t.Fatal("run start accepted a cross-project issue")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("code = %s, want %s", got, output.ErrValidation)
	}
	if _, err := db.GetRun(conn, 1); err == nil {
		t.Error("a run row was created despite the refusal")
	}
}

func TestRunStartStoresBudget(t *testing.T) {
	conn := newTestDB(t)
	cmd := runStartCmdWithDB(conn)
	err := cmd.Flags().Set("budget", "12.5")
	testsupport.Must(t, err, "setting --budget: %v", err)
	w, _ := bufWriter(true)

	err = runRunStart(cmd, w)
	testsupport.Must(t, err, "run start: %v", err)

	run, err := db.GetRun(conn, 1)
	testsupport.Must(t, err, "reading run: %v", err)
	if run.Budget != 12.5 {
		t.Errorf("budget = %g, want 12.5", run.Budget)
	}
	if run.Status != model.RunPlanning {
		t.Errorf("status = %q, want %q", run.Status, model.RunPlanning)
	}
}

// TestRunStartIdempotencyKeyReplaysOriginal covers DKT-416's acceptance
// criterion: repeating `run start` with the same --idempotency-key returns
// the original run rather than creating a duplicate — the same
// replay-protection `issue create` and `doc create` already give the other
// create verbs. A --budget that differs on the replay must not overwrite the
// original either; a hit returns the entity UNCHANGED, matching
// CreateIssueIdempotent/CreateDocIdempotent (they never compare params, they
// just return the original on any repeat of the key).
func TestRunStartIdempotencyKeyReplaysOriginal(t *testing.T) {
	conn := newTestDB(t)

	cmd := runStartCmdWithDB(conn)
	err := cmd.Flags().Set("idempotency-key", "retry-1")
	testsupport.Must(t, err, "setting --idempotency-key: %v", err)
	err = cmd.Flags().Set("budget", "12.5")
	testsupport.Must(t, err, "setting --budget: %v", err)
	w, _ := bufWriter(true)
	err = runRunStart(cmd, w)
	testsupport.Must(t, err, "first run start: %v", err)

	// The retry carries a DIFFERENT budget, to prove the replay returns the
	// original row rather than re-applying the new call's parameters.
	cmd2 := runStartCmdWithDB(conn)
	err = cmd2.Flags().Set("idempotency-key", "retry-1")
	testsupport.Must(t, err, "setting --idempotency-key: %v", err)
	err = cmd2.Flags().Set("budget", "99")
	testsupport.Must(t, err, "setting --budget: %v", err)
	w2, _ := bufWriter(true)
	err = runRunStart(cmd2, w2)
	testsupport.Must(t, err, "replayed run start: %v", err)

	var n int
	err = conn.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
	testsupport.Must(t, err, "counting runs: %v", err)
	if n != 1 {
		t.Errorf("run count = %d, want 1 — the replay created a duplicate", n)
	}

	run, err := db.GetRun(conn, 1)
	testsupport.Must(t, err, "reading run: %v", err)
	if run.Budget != 12.5 {
		t.Errorf("budget = %g, want 12.5 (the replay's --budget=99 must not overwrite the original)", run.Budget)
	}
}

// TestRunStartDistinctIdempotencyKeysCreateSeparateRuns confirms distinct
// keys do not collide with each other.
func TestRunStartDistinctIdempotencyKeysCreateSeparateRuns(t *testing.T) {
	conn := newTestDB(t)

	cmd := runStartCmdWithDB(conn)
	err := cmd.Flags().Set("idempotency-key", "key-a")
	testsupport.Must(t, err, "setting --idempotency-key: %v", err)
	w, _ := bufWriter(true)
	err = runRunStart(cmd, w)
	testsupport.Must(t, err, "first run start: %v", err)

	cmd2 := runStartCmdWithDB(conn)
	err = cmd2.Flags().Set("idempotency-key", "key-b")
	testsupport.Must(t, err, "setting --idempotency-key: %v", err)
	w2, _ := bufWriter(true)
	err = runRunStart(cmd2, w2)
	testsupport.Must(t, err, "second run start: %v", err)

	var n int
	err = conn.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
	testsupport.Must(t, err, "counting runs: %v", err)
	if n != 2 {
		t.Errorf("run count = %d, want 2 — distinct keys must not collide", n)
	}
}

// TestRunStartRefusesNegativeBudget and TestRunStartRefusesMissingIssue: both
// refuse BEFORE the run is created, so a typo leaves no empty run behind for
// someone to wonder about later.
func TestRunStartRefusesBadInput(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		want output.ErrorCode
	}{
		{"negative budget", map[string]string{"budget": "-1"}, output.ErrValidation},
		{"malformed issue id", map[string]string{"issue": "not-an-id"}, output.ErrValidation},
		{"absent issue", map[string]string{"issue": "DKT-99"}, output.ErrNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := newTestDB(t)
			cmd := runStartCmdWithDB(conn)
			for flag, value := range tc.set {
				if err := cmd.Flags().Set(flag, value); err != nil {
					t.Fatalf("setting --%s: %v", flag, err)
				}
			}
			w, _ := bufWriter(true)

			err := runRunStart(cmd, w)
			if err == nil {
				t.Fatal("run start succeeded on bad input")
			}
			if got := codeOf(t, err); got != tc.want {
				t.Errorf("code = %q, want %q", got, tc.want)
			}

			// Nothing was created.
			var n int
			err = conn.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
			testsupport.Must(t, err, "counting runs: %v", err)
			if n != 0 {
				t.Errorf("%d runs exist after a refused `run start`, want 0", n)
			}
		})
	}
}

// TestRunActivateSucceeds is the verb's happy path through the CLI boundary,
// including the shape the JSON envelope carries.
func TestRunActivateSucceeds(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := seedRun(t, conn)

	w, buf := bufWriter(true)

	err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
	testsupport.Must(t, err, "run activate: %v", err)

	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Run               json.RawMessage `json:"run"`
			IssuesBound       int             `json:"issues_bound"`
			IssuesExpanded    int             `json:"issues_expanded"`
			StepsCreated      int             `json:"steps_created"`
			ExpectedCostAdded float64         `json:"expected_cost_added"`
			ExpectedCostTotal float64         `json:"expected_cost_total"`
			PinsRecorded      int             `json:"pins_recorded"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding envelope %s: %v", buf.String(), err)
	}
	if !envelope.OK {
		t.Errorf("envelope reports failure: %s", buf.String())
	}
	if envelope.Data.IssuesBound != 1 || envelope.Data.IssuesExpanded != 1 {
		t.Errorf("bound %d / expanded %d, want 1 / 1",
			envelope.Data.IssuesBound, envelope.Data.IssuesExpanded)
	}
	if envelope.Data.StepsCreated != 2 {
		t.Errorf("created %d steps, want 2", envelope.Data.StepsCreated)
	}
	if envelope.Data.PinsRecorded != 1 {
		t.Errorf("recorded %d pins, want 1 (the bound workflow)", envelope.Data.PinsRecorded)
	}
	// DKT-517: `expected_cost_added` and `expected_cost_total` both ride the
	// envelope, named distinctly. On a first activation they agree — the
	// increment IS the total — but both keys must be present so a consumer
	// never has to infer one from the other.
	if envelope.Data.ExpectedCostAdded != envelope.Data.ExpectedCostTotal {
		t.Errorf("first activation: expected_cost_added = %v, expected_cost_total = %v, want equal",
			envelope.Data.ExpectedCostAdded, envelope.Data.ExpectedCostTotal)
	}

	// The issue was promoted out of backlog by the transaction's stage 7.
	issue, err := db.GetIssue(conn, issueID)
	testsupport.Must(t, err, "reading issue: %v", err)
	if issue.Status != model.StatusTodo {
		t.Errorf("issue status = %q after activation, want %q", issue.Status, model.StatusTodo)
	}
}

// TestRunActivateHumanLinePrintsAddedBeforeTotal is DKT-517's human-mode
// acceptance criterion: the summary line must print the cost increment
// BEFORE the run-wide total, so a reader scanning left to right sees "what
// this activation cost" first rather than the whole roster.
func TestRunActivateHumanLinePrintsAddedBeforeTotal(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)

	w, buf := bufWriter(false)

	err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
	testsupport.Must(t, err, "run activate: %v", err)

	text := buf.String()
	addedIdx := strings.Index(text, "expected cost added")
	totalIdx := strings.Index(text, "expected cost total")
	if addedIdx == -1 || totalIdx == -1 {
		t.Fatalf("summary line %q missing 'expected cost added' or 'expected cost total'", text)
	}
	if addedIdx >= totalIdx {
		t.Errorf("summary line %q prints the total before the increment, want added first", text)
	}
}

// TestRunActivateHelpDocumentsCostFields is DKT-517's --help acceptance
// criterion: an operator reading `docket run activate --help` must be told
// both JSON field names exist and what distinguishes them, not just find
// them undocumented in the envelope.
func TestRunActivateHelpDocumentsCostFields(t *testing.T) {
	cmd := newRunActivateCmd()
	long := cmd.Long
	for _, field := range []string{"expected_cost_total", "expected_cost_added"} {
		if !strings.Contains(long, field) {
			t.Errorf("--help text does not document `%s`", field)
		}
	}
}

// TestRunActivateNamesBlockedIssues is DKT-1180's boundary: an activation
// that leaves an issue unexpanded says so on the summary line, naming the
// predecessor holding it, and carries the same roster as `blocked_issues` in
// the JSON envelope — so `issues_expanded: 0` is never the whole report. The
// --help text names the field, as DKT-517's cost fields set the precedent.
func TestRunActivateNamesBlockedIssues(t *testing.T) {
	conn := newTestDB(t)
	runID, rootID := seedRun(t, conn)
	nextID, err := db.CreateIssue(conn, &model.Issue{
		Title: "waits on the root", Description: "a body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating the successor: %v", err)
	_, err = conn.Exec(
		`INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, 'depends_on', '2026-08-02T00:00:00Z')`, nextID, rootID)
	testsupport.Must(t, err, "seeding relation: %v", err)
	err = db.AddRunIssue(conn, runID, nextID)
	testsupport.Must(t, err, "adding the successor to the run: %v", err)

	// Human mode on a dry run, so the JSON pass below sees the same first
	// activation rather than a re-activation.
	w, buf := bufWriter(false)
	err = runActivateWithWriter(t, conn, w, model.FormatRunID(runID), "--dry-run")
	testsupport.Must(t, err, "run activate --dry-run: %v", err)
	want := "1 issue(s) still blocked (" + model.FormatID(nextID) +
		" waits on " + model.FormatID(rootID) + " todo)"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("summary line %q does not carry %q", buf.String(), want)
	}

	w, buf = bufWriter(true)
	err = runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
	testsupport.Must(t, err, "run activate: %v", err)
	var envelope struct {
		Data struct {
			IssuesExpanded int                   `json:"issues_expanded"`
			BlockedIssues  []engine.BlockedIssue `json:"blocked_issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding envelope %s: %v", buf.String(), err)
	}
	if envelope.Data.IssuesExpanded != 1 {
		t.Errorf("expanded %d, want 1 (the root alone)", envelope.Data.IssuesExpanded)
	}
	wantRoster := []engine.BlockedIssue{{
		IssueID: model.FormatID(nextID),
		BlockedBy: []engine.BlockingIssue{{
			IssueID: model.FormatID(rootID), Status: string(model.StatusTodo),
		}},
	}}
	if got := envelope.Data.BlockedIssues; !reflect.DeepEqual(got, wantRoster) {
		t.Errorf("blocked_issues = %+v, want %+v", got, wantRoster)
	}

	if long := newRunActivateCmd().Long; !strings.Contains(long, "blocked_issues") {
		t.Error("--help text does not document `blocked_issues`")
	}
}

// runActivateViaCLI drives a FRESH `run activate` command, built through the
// SAME newRunActivateCmd factory the package's registered runActivateCmd
// uses, through cobra's own arg parsing — the way a shell invocation would:
// SetArgs + ExecuteContext, per runImportCmd's pattern (import_test.go). A
// fresh, parentless instance (rather than the package's runActivateCmd
// singleton, wired into rootCmd) means this drives the real flag
// registration without also triggering rootCmd's PersistentPreRunE, which
// would open the ON-DISK database and ignore the in-memory one this test
// seeds. A case that stopped registering a flag fails HERE with "unknown
// flag: --whatever" instead of runActivateCmdWithDB's hand-built stand-in
// flag set silently testing nothing (DKT-56-CL1).
func runActivateViaCLI(t *testing.T, conn *sql.DB, args ...string) error {
	t.Helper()
	cmd := newRunActivateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	ctx := context.WithValue(context.Background(), dbKey, conn)
	return cmd.ExecuteContext(ctx)
}

// TestRunActivateAcceptsReasonAndRecordsIt is DKT-53/DKT-56: `--reason` must
// not be refused as an unknown flag, matching `run budget --set --reason`,
// and the supplied value must round-trip onto the run-activated event.
func TestRunActivateAcceptsReasonAndRecordsIt(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)

	err := runActivateViaCLI(t, conn, model.FormatRunID(runID), "--reason", "operator kickoff")
	testsupport.Must(t, err, "run activate --reason: %v", err)

	page, err := engine.ListEvents(conn, engine.EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var found bool
	for _, e := range page.Events {
		if e.Kind != engine.EventRunActivated {
			continue
		}
		found = true
		if !strings.Contains(string(e.Data), `"reason":"operator kickoff"`) {
			t.Errorf("run-activated data %s does not carry the reason", e.Data)
		}
	}
	if !found {
		t.Fatal("no run-activated event recorded")
	}
}

// TestRunActivateDryRunProjectsRatherThanCommits is the CLI-boundary half of
// DKT-96/DKT-100: the JSON envelope's `run.status`/`run.activated_at_ms` must
// render the STILL-PLANNING, uncommitted state, and the mutation belongs on
// `projected_status`/`projected_activated_at_ms` instead — so a script that
// only reads `run.status` can never mistake a dry run for a commit.
func TestRunActivateDryRunProjectsRatherThanCommits(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)

	w, buf := bufWriter(true)

	err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID), "--dry-run")
	testsupport.Must(t, err, "run activate --dry-run: %v", err)

	var envelope struct {
		Data struct {
			Run struct {
				Status        string `json:"status"`
				ActivatedAtMS *int64 `json:"activated_at_ms"`
			} `json:"run"`
			DryRun                 bool   `json:"dry_run"`
			ProjectedStatus        string `json:"projected_status"`
			ProjectedActivatedAtMS *int64 `json:"projected_activated_at_ms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding envelope %s: %v", buf.String(), err)
	}

	if !envelope.Data.DryRun {
		t.Fatal("envelope does not report dry_run")
	}
	if envelope.Data.Run.Status != string(model.RunPlanning) {
		t.Errorf("run.status = %q, want %q (the committed state)",
			envelope.Data.Run.Status, model.RunPlanning)
	}
	if envelope.Data.Run.ActivatedAtMS != nil {
		t.Errorf("run.activated_at_ms = %v, want absent (never committed)",
			*envelope.Data.Run.ActivatedAtMS)
	}
	if envelope.Data.ProjectedStatus != string(model.RunActive) {
		t.Errorf("projected_status = %q, want %q",
			envelope.Data.ProjectedStatus, model.RunActive)
	}
	if envelope.Data.ProjectedActivatedAtMS == nil {
		t.Error("projected_activated_at_ms is absent, want the projected timestamp")
	}

	// And the run is still `planning` on disk — the dry run committed nothing.
	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "reading run: %v", err)
	if run.Status != model.RunPlanning {
		t.Errorf("committed run status = %q, want %q", run.Status, model.RunPlanning)
	}
}

// TestRunActivateCarriesScopeWarnings is the unscoped-holder lint at the CLI
// boundary. The engine decides WHAT to warn about; what these assert is that the
// verb carries it on both channels, and that a run with nothing to say carries
// no key at all.
//
// `runWorkflow` declares no `holds_tree`, so both its steps hold the tree by
// §11.1's default, and `seedRun`'s issue declares no scope — which is the RUN-5
// shape exactly, arrived at without anyone choosing it.
func TestRunActivateCarriesScopeWarnings(t *testing.T) {
	t.Run("the JSON payload carries the array", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := seedRun(t, conn)

		w, buf := bufWriter(true)

		err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
		testsupport.Must(t, err, "run activate: %v", err)

		var envelope struct {
			Data struct {
				ScopeWarnings []struct {
					Issue    string `json:"issue"`
					Workflow string `json:"workflow"`
					Reason   string `json:"reason"`
				} `json:"scope_warnings"`
			} `json:"data"`
		}
		if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
			t.Fatalf("decoding envelope %s: %v", buf.String(), err)
		}
		if len(envelope.Data.ScopeWarnings) != 1 {
			t.Fatalf("scope_warnings holds %d entries, want 1: %s",
				len(envelope.Data.ScopeWarnings), buf.String())
		}
		got := envelope.Data.ScopeWarnings[0]
		if got.Issue != model.FormatID(issueID) {
			t.Errorf("scope_warnings[0].issue = %q, want %q", got.Issue, model.FormatID(issueID))
		}
		if got.Workflow == "" || got.Reason == "" {
			t.Errorf("scope_warnings[0] = %+v, want a workflow and a reason", got)
		}
	})

	t.Run("human mode writes it to stderr", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := seedRun(t, conn)

		// A writer whose stderr is readable — bufWriter discards it, and stderr
		// is the whole point here: the warning is a report ABOUT the activation,
		// so a caller consuming the result must not have to filter it out.
		stderr := &bytes.Buffer{}
		w := &output.Writer{Stdout: &bytes.Buffer{}, Stderr: stderr}

		err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
		testsupport.Must(t, err, "run activate: %v", err)

		if !strings.Contains(stderr.String(), model.FormatID(issueID)) {
			t.Errorf("stderr does not name the unscoped issue: %q", stderr.String())
		}
	})

	t.Run("a declared scope carries no key", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := seedRun(t, conn)
		err := db.SetIssueScopeGlobs(conn, issueID, `["internal/cli/**"]`)
		testsupport.Must(t, err, "setting scope: %v", err)

		w, buf := bufWriter(true)

		err = runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
		testsupport.Must(t, err, "run activate: %v", err)

		if strings.Contains(buf.String(), "scope_warnings") {
			t.Errorf("payload carries scope_warnings for a run whose issue "+
				"declared its scope: %s", buf.String())
		}
	})
}

// baselineWorkflow and domainWorkflow are DKT-1182's routing pair, in the shape
// the corpus actually has: a baseline selected by NO positive label that
// declares no domain, and a pipeline selected by one label that declares the
// paths its domain occupies. An issue labelled `ui` binds the second and only
// the second; anything else falls into the first.
const baselineWorkflow = `
[pipeline]
name = "baseline-run"
version = 1
[match]
kind = ["task"]
unless_labels = ["ui"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
`

const domainWorkflow = `
[pipeline]
name = "ui-run"
version = 1
[match]
kind = ["task"]
labels_any = ["ui"]
domain_paths = ["internal/tui/**"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
`

// TestRunActivateCarriesBindingWarnings is DKT-1182 at the CLI boundary: the
// exactly-one-WRONG-match signal must reach BOTH channels — the JSON envelope a
// conductor parses and the stderr line an operator reads — and a run whose
// labels and scope agree must carry no key at all.
//
// The dry run is the case the issue was filed about: it is where a conductor
// decides whether to commit a run, and by the time a real activation could
// report a mis-binding the binding is already made.
func TestRunActivateCarriesBindingWarnings(t *testing.T) {
	// seedMisbound builds the HRN-1118 shape: an issue scoped entirely inside
	// `ui-run`'s declared domain, labelled for neither pipeline, so it binds the
	// label-less `unit-run` baseline exactly once.
	seedMisbound := func(t *testing.T, conn *sql.DB, labels []string) (runID, issueID int) {
		t.Helper()
		registerForRun(t, conn, baselineWorkflow)
		registerForRun(t, conn, domainWorkflow)

		id, err := db.CreateIssue(conn, &model.Issue{
			Title: "flaky conversation screen test", Description: "a body",
			Status: model.StatusBacklog, Priority: model.PriorityNone,
			Kind: model.IssueKindTask,
		}, labels, nil)
		testsupport.Must(t, err, "creating issue: %v", err)
		err = db.SetIssueScopeGlobs(conn, id,
			`["internal/tui/screens/conversation_test.go"]`)
		testsupport.Must(t, err, "setting scope: %v", err)

		run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
		testsupport.Must(t, err, "starting run: %v", err)
		err = db.AddRunIssue(conn, run.ID, id)
		testsupport.Must(t, err, "adding issue to run: %v", err)
		return run.ID, id
	}

	t.Run("the dry run's JSON payload carries the array", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := seedMisbound(t, conn, []string{"qa"})

		w, buf := bufWriter(true)

		err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID), "--dry-run")
		testsupport.Must(t, err, "run activate --dry-run: %v", err)

		var envelope struct {
			Data struct {
				DryRun          bool `json:"dry_run"`
				BindingWarnings []struct {
					Issue          string   `json:"issue"`
					BoundWorkflow  string   `json:"bound_workflow"`
					DomainWorkflow string   `json:"domain_workflow"`
					Scope          []string `json:"scope"`
					MissingLabels  []string `json:"missing_labels"`
					Reason         string   `json:"reason"`
				} `json:"binding_warnings"`
			} `json:"data"`
		}
		if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
			t.Fatalf("decoding envelope %s: %v", buf.String(), err)
		}
		if !envelope.Data.DryRun {
			t.Fatal("envelope does not report dry_run")
		}
		if len(envelope.Data.BindingWarnings) != 1 {
			t.Fatalf("binding_warnings holds %d entries, want 1: %s",
				len(envelope.Data.BindingWarnings), buf.String())
		}
		got := envelope.Data.BindingWarnings[0]
		if got.Issue != model.FormatID(issueID) {
			t.Errorf("binding_warnings[0].issue = %q, want %q",
				got.Issue, model.FormatID(issueID))
		}
		if got.BoundWorkflow != "baseline-run@1" {
			t.Errorf("bound_workflow = %q, want baseline-run@1", got.BoundWorkflow)
		}
		if got.DomainWorkflow != "ui-run@1" {
			t.Errorf("domain_workflow = %q, want ui-run@1", got.DomainWorkflow)
		}
		if len(got.MissingLabels) != 1 || got.MissingLabels[0] != "ui" {
			t.Errorf("missing_labels = %v, want [ui]", got.MissingLabels)
		}
		if len(got.Scope) != 1 || got.Reason == "" {
			t.Errorf("binding_warnings[0] = %+v, want the scope and a reason", got)
		}
	})

	t.Run("human mode writes it to stderr", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := seedMisbound(t, conn, []string{"qa"})

		stderr := &bytes.Buffer{}
		w := &output.Writer{Stdout: &bytes.Buffer{}, Stderr: stderr}

		err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID), "--dry-run")
		testsupport.Must(t, err, "run activate --dry-run: %v", err)

		out := stderr.String()
		// The issue, and BOTH workflows: what will run, and what the paths say
		// should. A line naming only one of them leaves the reader to guess.
		for _, want := range []string{model.FormatID(issueID), "baseline-run@1", "ui-run@1"} {
			if !strings.Contains(out, want) {
				t.Errorf("stderr does not name %q: %q", want, out)
			}
		}
	})

	t.Run("a correctly labelled issue carries no key", func(t *testing.T) {
		conn := newTestDB(t)
		runID, _ := seedMisbound(t, conn, []string{"ui"})

		w, buf := bufWriter(true)

		err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID), "--dry-run")
		testsupport.Must(t, err, "run activate --dry-run: %v", err)

		if strings.Contains(buf.String(), "binding_warnings") {
			t.Errorf("payload carries binding_warnings for an issue labelled for "+
				"the workflow whose domain it sits in: %s", buf.String())
		}
	})
}

// TestRunActivateCarriesContextWarnings pins `context_warnings`' JSON shape,
// the sibling of TestRunActivateCarriesScopeWarnings' — same struct family
// (DKT-99), so `issue` must carry the display id here too, not the internal
// numeric PK.
func TestRunActivateCarriesContextWarnings(t *testing.T) {
	conn := newTestDB(t)
	registerForRun(t, conn, runWorkflow)

	err := db.SetConfig(conn, 0, db.KeyContextWarnBytes, "128")
	testsupport.Must(t, err, "setting the warn cap: %v", err)
	err = db.SetConfig(conn, 0, db.KeyContextErrorBytes, "1000000")
	testsupport.Must(t, err, "setting the error cap: %v", err)

	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "warn me", Description: strings.Repeat("x", 4096),
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)

	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	err = db.AddRunIssue(conn, run.ID, issueID)
	testsupport.Must(t, err, "adding issue to run: %v", err)

	w, buf := bufWriter(true)

	err = runActivateWithWriter(t, conn, w, model.FormatRunID(run.ID))
	testsupport.Must(t, err, "run activate: %v", err)

	var envelope struct {
		Data struct {
			ContextWarnings []struct {
				Instance string `json:"instance"`
				Issue    string `json:"issue"`
			} `json:"context_warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding envelope %s: %v", buf.String(), err)
	}
	if len(envelope.Data.ContextWarnings) == 0 {
		t.Fatalf("context_warnings holds no entries: %s", buf.String())
	}
	want := model.FormatID(issueID)
	for _, w := range envelope.Data.ContextWarnings {
		if w.Issue != want {
			t.Errorf("context_warnings[].issue = %q, want %q", w.Issue, want)
		}
	}
}

// TestRunActivateHumanModeRendersContextWarnings is DKT-108: the human-mode
// `context_warnings` render (the `for _, warning := range result.ContextWarnings
// { w.Warn(...) }` block in runRunActivate) had ZERO test coverage — the only
// context-warning test ran in JSON mode, where `Warn` returns before
// formatting anything. A mutant that deleted the operator-visible issue id
// from the warning text survived the whole suite. This exercises the actual
// human-mode stderr text, not just the JSON sibling's shape.
func TestRunActivateHumanModeRendersContextWarnings(t *testing.T) {
	conn := newTestDB(t)
	registerForRun(t, conn, runWorkflow)

	testsupport.Must(t, db.SetConfig(conn, 0, db.KeyContextWarnBytes, "128"),
		"setting the warn cap")
	testsupport.Must(t, db.SetConfig(conn, 0, db.KeyContextErrorBytes, "1000000"),
		"setting the error cap")

	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "warn me", Description: strings.Repeat("x", 4096),
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)

	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "adding issue to run")

	stderr := &bytes.Buffer{}
	w := &output.Writer{JSONMode: false, Stdout: &bytes.Buffer{}, Stderr: stderr}

	err = runActivateWithWriter(t, conn, w, model.FormatRunID(run.ID))
	testsupport.Must(t, err, "run activate: %v", err)

	text := stderr.String()
	if !strings.Contains(text, "Warning:") {
		t.Fatalf("stderr carries no warning at all: %q", text)
	}

	// Isolate the CONTEXT-WARNING line specifically, identified by the config
	// key only that render names — an unscoped issue over this fixture also
	// trips the scope-warning lint, whose line ALSO carries the issue id, so
	// asserting against the whole buffer would pass even with the context-
	// warning render's id deleted (the exact mutant this test exists to kill).
	var contextLine string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "context.warn_bytes") {
			contextLine = line
			break
		}
	}
	if contextLine == "" {
		t.Fatalf("no context-warning line in stderr: %q", text)
	}

	wantIssue := model.FormatID(issueID)
	if !strings.Contains(contextLine, wantIssue) {
		t.Errorf("context-warning line = %q, does not name the issue %q — the "+
			"id-deleting mutant DKT-99-T1 found would survive this", contextLine, wantIssue)
	}
	// The byte count and cap are the other operator-visible numbers the
	// message names — asserted so a mutant swapping `warning.Bytes` for
	// `warning.Cap`, or either for a literal, cannot survive silently either.
	if !strings.Contains(contextLine, "128") {
		t.Errorf("context-warning line = %q, does not name the configured cap 128", contextLine)
	}
}

// TestRunActivateErrorCodes is the §5.5 taxonomy at the CLI boundary. Each
// engine failure must arrive as its own code — an error that degraded to
// "general error" here is one no script can branch on.
func TestRunActivateErrorCodes(t *testing.T) {
	t.Run("missing run is NOT_FOUND", func(t *testing.T) {
		conn := newTestDB(t)
		registerForRun(t, conn, runWorkflow)

		w, _ := bufWriter(true)

		err := runActivateWithWriter(t, conn, w, "RUN-99")
		if err == nil {
			t.Fatal("activate succeeded on a run that does not exist")
		}
		if got := codeOf(t, err); got != output.ErrNotFound {
			t.Errorf("code = %q, want %q", got, output.ErrNotFound)
		}
	})

	t.Run("missing pin is NOT_FOUND", func(t *testing.T) {
		conn := newTestDB(t)
		runID, _ := seedRun(t, conn)

		w, _ := bufWriter(true)

		err := runActivateWithWriter(t, conn, w,
			model.FormatRunID(runID), "--pin", filepath.Join(t.TempDir(), "absent"))
		if err == nil {
			t.Fatal("activate succeeded with a --pin path that does not exist")
		}
		if got := codeOf(t, err); got != output.ErrNotFound {
			t.Errorf("code = %q, want %q", got, output.ErrNotFound)
		}
	})

	t.Run("unbindable issue is VALIDATION_ERROR", func(t *testing.T) {
		conn := newTestDB(t)
		registerForRun(t, conn, runWorkflow)

		// The definition binds `task`; an `epic` matches nothing.
		id, err := db.CreateIssue(conn, &model.Issue{
			Title: "an epic", Status: model.StatusBacklog,
			Priority: model.PriorityNone, Kind: model.IssueKindEpic,
		}, nil, nil)
		testsupport.Must(t, err, "creating issue: %v", err)
		run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
		testsupport.Must(t, err, "starting run: %v", err)
		err = db.AddRunIssue(conn, run.ID, id)
		testsupport.Must(t, err, "adding issue: %v", err)

		w, _ := bufWriter(true)

		err = runActivateWithWriter(t, conn, w, run.Ref())
		if err == nil {
			t.Fatal("activate succeeded on an issue no workflow binds")
		}
		if got := codeOf(t, err); got != output.ErrValidation {
			t.Errorf("code = %q, want %q", got, output.ErrValidation)
		}
	})

	t.Run("terminal run is CONFLICT", func(t *testing.T) {
		conn := newTestDB(t)
		runID, _ := seedRun(t, conn)
		err := db.SetRunStatus(conn, runID, model.RunDone, "", model.NowMS())
		testsupport.Must(t, err, "setting run done: %v", err)

		w, _ := bufWriter(true)

		err = runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
		if err == nil {
			t.Fatal("activate succeeded on a done run")
		}
		if got := codeOf(t, err); got != output.ErrConflict {
			t.Errorf("code = %q, want %q", got, output.ErrConflict)
		}
	})

	t.Run("malformed run id is VALIDATION_ERROR", func(t *testing.T) {
		conn := newTestDB(t)
		w, _ := bufWriter(true)

		err := runActivateWithWriter(t, conn, w, "not-a-run")
		if err == nil {
			t.Fatal("activate succeeded on a malformed run id")
		}
		if got := codeOf(t, err); got != output.ErrValidation {
			t.Errorf("code = %q, want %q", got, output.ErrValidation)
		}
	})
}

// TestRunStatusCountsIssuesAddedAfterActivation is DKT-103: an issue attached
// to an ACTIVE run via `run issue add` and bound/expanded/promoted by a
// re-activation (RA3) must be counted by `run status` — the roster surface
// must agree with the event log and the step table, or `docket run status`
// undercounts a run with live steps against an issue nobody can see.
func TestRunStatusCountsIssuesAddedAfterActivation(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)

	w, _ := bufWriter(true)
	err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
	testsupport.Must(t, err, "first activate: %v", err)

	// A second issue joins the ACTIVE run — RUN-8's exact shape.
	second, err := db.CreateIssue(conn, &model.Issue{
		Title: "late arrival", Description: "late body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating second issue: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, runID, second), "adding issue to active run: %v", err)

	err = runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
	testsupport.Must(t, err, "re-activate: %v", err)

	statusCmd := runStatusCmdWithDB(conn)
	w2, buf := bufWriter(true)
	err = runRunStatus(statusCmd, []string{model.FormatRunID(runID)}, w2)
	testsupport.Must(t, err, "run status: %v", err)

	var envelope struct {
		Data struct {
			Issues int `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding envelope %s: %v", buf.String(), err)
	}
	if envelope.Data.Issues != 2 {
		t.Errorf("run status issues = %d, want 2 (the original plus the "+
			"late arrival re-activation bound and promoted)", envelope.Data.Issues)
	}
}

// TestRunStatusWritesNothing is the read-verb discipline v6 established for
// leases, applied to runs: a read computes and renders, and never writes.
// "I only looked at it" has to be true.
func TestRunStatusWritesNothing(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)

	w, _ := bufWriter(true)
	err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
	testsupport.Must(t, err, "activate: %v", err)

	before := snapshotRunState(t, conn)

	statusCmd := runStatusCmdWithDB(conn)
	statusW, buf := bufWriter(true)
	err = runRunStatus(statusCmd, []string{model.FormatRunID(runID)}, statusW)
	testsupport.Must(t, err, "run status: %v", err)
	// And again as a list, which takes the other branch.
	listCmd := runStatusCmdWithDB(conn)
	listW, _ := bufWriter(true)
	err = runRunStatus(listCmd, nil, listW)
	testsupport.Must(t, err, "run status (list): %v", err)

	if after := snapshotRunState(t, conn); after != before {
		t.Errorf("run state changed across two READS:\n before %s\n after  %s", before, after)
	}

	// The rollup is present, so the test is not passing because it rendered
	// nothing.
	if !strings.Contains(buf.String(), "steps") {
		t.Errorf("run status emitted no step rollup: %s", buf.String())
	}
}

// snapshotRunState renders every mutable field a read could plausibly touch.
func snapshotRunState(t *testing.T, conn *sql.DB) string {
	t.Helper()
	var state string
	err := conn.QueryRow(`
		SELECT (SELECT group_concat(id || status || row_version || updated_at_ms) FROM runs)
		    || '|' || (SELECT COALESCE(group_concat(instance || status || row_version), '') FROM steps)
		    || '|' || (SELECT COALESCE(group_concat(expanded_at_ms), '') FROM run_issues)
	`).Scan(&state)
	testsupport.Must(t, err, "snapshotting run state: %v", err)
	return state
}

// TestRunStatusActiveExcludesTerminalRuns covers `--active`. `planning` counts
// as active: a run that exists but has not been activated is still live work
// an operator is mid-way through.
func TestRunStatusActiveExcludesTerminalRuns(t *testing.T) {
	conn := newTestDB(t)

	planning, err := db.InsertRun(conn, 1, "planning", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	done, err := db.InsertRun(conn, 1, "done", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	err = db.SetRunStatus(conn, done.ID, model.RunDone, "", model.NowMS())
	testsupport.Must(t, err, "setting run done: %v", err)

	cmd := runStatusCmdWithDB(conn)
	err = cmd.Flags().Set("active", "true")
	testsupport.Must(t, err, "setting --active: %v", err)
	w, buf := bufWriter(true)
	err = runRunStatus(cmd, nil, w)
	testsupport.Must(t, err, "run status --active: %v", err)

	out := buf.String()
	if !strings.Contains(out, planning.Ref()) {
		t.Errorf("--active omitted the planning run %s: %s", planning.Ref(), out)
	}
	if strings.Contains(out, done.Ref()) {
		t.Errorf("--active included the done run %s: %s", done.Ref(), out)
	}
}

// TestRunLifecycleTransitions walks the §1.1 machine and asserts that an
// ILLEGAL transition is REFUSED rather than silently applied. A no-op success
// would let a harness believe it had quiesced a run that never was active.
func TestRunLifecycleTransitions(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)

	w, _ := bufWriter(true)
	err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
	testsupport.Must(t, err, "activate: %v", err)

	pause := runMoveCmdWithDB(conn, "pause")
	err = pause.Flags().Set("reason", "operator review")
	testsupport.Must(t, err, "setting --reason: %v", err)
	pauseW, _ := bufWriter(true)
	if err := moveRun(pause, model.FormatRunID(runID), runMove{
		to: model.RunWaitingHuman, from: []model.RunStatus{model.RunActive}, verb: "Paused",
	}, pauseW); err != nil {
		t.Fatalf("pause: %v", err)
	}

	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "reading run: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Errorf("status after pause = %q, want %q", run.Status, model.RunWaitingHuman)
	}
	if run.Reason != "operator review" {
		t.Errorf("reason = %q, want %q", run.Reason, "operator review")
	}

	// Pausing an already-paused run is a CONFLICT, not a no-op success.
	again := runMoveCmdWithDB(conn, "pause")
	againW, _ := bufWriter(true)
	err = moveRun(again, model.FormatRunID(runID), runMove{
		to: model.RunWaitingHuman, from: []model.RunStatus{model.RunActive}, verb: "Paused",
	}, againW)
	if err == nil {
		t.Fatal("pausing an already-paused run succeeded")
	}
	if got := codeOf(t, err); got != output.ErrConflict {
		t.Errorf("code = %q, want %q", got, output.ErrConflict)
	}

	// Resume, then abandon — and abandon without --reason is refused, because
	// "abandoned" alone does not answer the question somebody asks later.
	resume := runMoveCmdWithDB(conn, "resume")
	resumeW, _ := bufWriter(true)
	if err := moveRun(resume, model.FormatRunID(runID), runMove{
		to: model.RunActive, from: []model.RunStatus{model.RunWaitingHuman}, verb: "Resumed",
	}, resumeW); err != nil {
		t.Fatalf("resume: %v", err)
	}

	noReason := runMoveCmdWithDB(conn, "abandon")
	noReasonW, _ := bufWriter(true)
	err = moveRun(noReason, model.FormatRunID(runID), runMove{
		to:   model.RunAbandoned,
		from: []model.RunStatus{model.RunPlanning, model.RunActive, model.RunWaitingHuman},
		verb: "Abandoned", requiresReason: true,
	}, noReasonW)
	if err == nil {
		t.Fatal("abandon succeeded without --reason")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("code = %q, want %q", got, output.ErrValidation)
	}
}

// ---------------------------------------------------------------------------
// `--scope` on issue create|edit
// ---------------------------------------------------------------------------

// TestScopeGlobsJSON covers the flag's encoding, including the two cases that
// are easy to get wrong: an unset flag stores NULL (not `[]`), and an
// explicitly-emptied flag clears the declaration back to NULL.
func TestScopeGlobsJSON(t *testing.T) {
	t.Run("unset flag stores nothing", func(t *testing.T) {
		cmd := &cobra.Command{}
		addScopeFlag(cmd)

		got, err := scopeGlobsJSON(cmd)
		testsupport.Must(t, err, "scopeGlobsJSON: %v", err)
		if got != "" {
			t.Errorf("scopeGlobsJSON = %q with --scope unset, want \"\" (which stores NULL); "+
				"\"no scope declared\" and \"declared to touch nothing\" are different facts",
				got)
		}
	})

	t.Run("globs keep declared order", func(t *testing.T) {
		cmd := &cobra.Command{}
		addScopeFlag(cmd)
		// Deliberately not alphabetical: the order is the author's, it is
		// echoed back in the issue snapshot, and sorting it would make the
		// snapshot differ from what the operator wrote.
		for _, glob := range []string{"z/**", "a/**", "m/**"} {
			err := cmd.Flags().Set(scopeFlag, glob)
			testsupport.Must(t, err, "setting --scope: %v", err)
		}

		got, err := scopeGlobsJSON(cmd)
		testsupport.Must(t, err, "scopeGlobsJSON: %v", err)
		if want := `["z/**","a/**","m/**"]`; got != want {
			t.Errorf("scopeGlobsJSON = %s, want %s", got, want)
		}
	})

	t.Run("empty glob is refused", func(t *testing.T) {
		cmd := &cobra.Command{}
		addScopeFlag(cmd)
		err := cmd.Flags().Set(scopeFlag, " ")
		testsupport.Must(t, err, "setting --scope: %v", err)

		_, err = scopeGlobsJSON(cmd)
		if err == nil {
			t.Fatal("an empty --scope glob was accepted")
		}
		if got := codeOf(t, err); got != output.ErrValidation {
			t.Errorf("code = %q, want %q", got, output.ErrValidation)
		}
	})
}

// TestApplyScopeLeavesUnsetColumnAlone is the dormancy obligation at the verb
// level: an `issue edit --title X` must not clear a scope the operator declared
// earlier, and an issue created without `--scope` must read NULL.
func TestApplyScopeLeavesUnsetColumnAlone(t *testing.T) {
	conn := newTestDB(t)

	id, err := db.CreateIssue(conn, &model.Issue{
		Title: "scoped", Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)

	// Created without --scope: NULL.
	if got := scopeColumn(t, conn, id); got.Valid {
		t.Errorf("scope_globs = %q on an issue created without --scope, want NULL", got.String)
	}

	// Declared.
	set := &cobra.Command{}
	addScopeFlag(set)
	err = set.Flags().Set(scopeFlag, "internal/db/**")
	testsupport.Must(t, err, "setting --scope: %v", err)
	err = applyScope(set, conn, id)
	testsupport.Must(t, err, "applyScope: %v", err)
	if got := scopeColumn(t, conn, id); !got.Valid || got.String != `["internal/db/**"]` {
		t.Errorf("scope_globs = %v after --scope, want [\"internal/db/**\"]", got)
	}

	// An edit that does NOT mention --scope leaves it alone.
	untouched := &cobra.Command{}
	addScopeFlag(untouched)
	err = applyScope(untouched, conn, id)
	testsupport.Must(t, err, "applyScope with the flag unset: %v", err)
	if got := scopeColumn(t, conn, id); !got.Valid || got.String != `["internal/db/**"]` {
		t.Errorf("scope_globs = %v after an edit that never mentioned --scope, "+
			"want the declaration preserved", got)
	}
}

func scopeColumn(t *testing.T, conn *sql.DB, issueID int) sql.NullString {
	t.Helper()
	var globs sql.NullString
	if err := conn.QueryRow(
		`SELECT scope_globs FROM issues WHERE id = ?`, issueID,
	).Scan(&globs); err != nil {
		t.Fatalf("reading scope_globs: %v", err)
	}
	return globs
}

// TestRunStartReadsRequestFile covers the one file the verb reads, and its
// NOT_FOUND refusal.
func TestRunStartReadsRequestFile(t *testing.T) {
	conn := newTestDB(t)

	path := filepath.Join(t.TempDir(), "request.md")
	err := os.WriteFile(path, []byte("do the thing\n"), 0o644)
	testsupport.Must(t, err, "writing request file: %v", err)

	cmd := runStartCmdWithDB(conn)
	err = cmd.Flags().Set("request-file", path)
	testsupport.Must(t, err, "setting --request-file: %v", err)
	w, _ := bufWriter(true)
	err = runRunStart(cmd, w)
	testsupport.Must(t, err, "run start: %v", err)

	run, err := db.GetRun(conn, 1)
	testsupport.Must(t, err, "reading run: %v", err)
	if run.Request != "do the thing" {
		t.Errorf("request = %q, want %q", run.Request, "do the thing")
	}

	missing := runStartCmdWithDB(conn)
	err = missing.Flags().Set("request-file", filepath.Join(t.TempDir(), "absent"))
	testsupport.Must(t, err, "setting --request-file: %v", err)
	missingW, _ := bufWriter(true)
	err = runRunStart(missing, missingW)
	if err == nil {
		t.Fatal("run start succeeded with a --request-file that does not exist")
	}
	if got := codeOf(t, err); got != output.ErrNotFound {
		t.Errorf("code = %q, want %q", got, output.ErrNotFound)
	}
}
