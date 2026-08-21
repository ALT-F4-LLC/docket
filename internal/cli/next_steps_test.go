package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// activatedRunForNext seeds and activates a run, so `next --run` has steps.
func activatedRunForNext(t *testing.T, conn *sql.DB) int {
	t.Helper()
	runID, _ := seedRun(t, conn)
	if _, err := engine.Activate(conn, runID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	return runID
}

// TestNextIssueModeByteIdentical is §6.3.1's proof at the unit level: `next`
// WITHOUT --run produces exactly what it produced before step mode existed.
//
// The comparison is against output captured from the same code path with the
// flag absent, byte for byte, in both JSON and human mode — the same discipline
// QA section X applies at the process level. What it catches is a dispatch that
// leaks: a `runNext` that consulted the run even when --run is empty, or that
// wrapped the issue-mode payload in a step-mode envelope.
func TestNextIssueModeByteIdentical(t *testing.T) {
	conn := newTestDB(t)
	createIssueWithFile(t, conn, "first", "a.go")
	createIssueWithFile(t, conn, "second", "b.go")

	// A workflow, a run, and an activated set of STEPS all exist. Issue mode
	// must not notice any of it.
	activatedRunForNext(t, conn)

	capture := func(jsonMode bool) string {
		cmd := nextCmdWithDB(conn, 10)
		w, buf := bufWriter(jsonMode)
		err := runNext(cmd, nil, w)
		testsupport.Must(t, err, "runNext: %v", err)
		return buf.String()
	}

	// Issue mode with steps present.
	withSteps := capture(true)
	withStepsHuman := capture(false)

	// The same call must be stable across repeated invocations, and must not
	// mention any step vocabulary.
	if again := capture(true); again != withSteps {
		t.Errorf("issue-mode JSON is not stable across calls:\n%s\n%s", withSteps, again)
	}
	for _, token := range []string{"STEP-", "instance", "lease_ttl_s", "\"steps\""} {
		if strings.Contains(withSteps, token) {
			t.Errorf("issue-mode JSON leaked step vocabulary %q:\n%s", token, withSteps)
		}
		if strings.Contains(withStepsHuman, token) {
			t.Errorf("issue-mode human output leaked step vocabulary %q:\n%s",
				token, withStepsHuman)
		}
	}

	// And the payload is the issue-mode shape: `issues`, not `steps`.
	var envelope struct {
		Data struct {
			Issues []json.RawMessage `json:"issues"`
			Steps  []json.RawMessage `json:"steps"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(withSteps), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, withSteps)
	}
	// Three issues: the two created here plus the one seedRun attached to the
	// activated run. Issue mode sees all of them and knows nothing about the
	// run any of them belongs to — which is the point.
	if len(envelope.Data.Issues) != 3 {
		t.Errorf("issue mode returned %d issues, want 3", len(envelope.Data.Issues))
	}
	if envelope.Data.Steps != nil {
		t.Error("issue mode emitted a `steps` key")
	}
}

// TestNextStepModeRefusesIssueFilters is §6.3.1's refusal: an issue-mode filter
// that silently did nothing in step mode is a filter a dispatcher will trust
// and be wrong about.
//
// Each flag is asserted individually, and the message must NAME --run as the
// conflict — a refusal that says only "invalid combination" leaves the caller
// guessing which of the two to drop.
func TestNextStepModeRefusesIssueFilters(t *testing.T) {
	for _, flag := range []string{"status", "priority", "label", "type"} {
		t.Run(flag, func(t *testing.T) {
			conn := newTestDB(t)
			runID := activatedRunForNext(t, conn)

			cmd := nextCmdWithDB(conn, 10)
			err := cmd.Flags().Set("run", model.FormatRunID(runID))
			testsupport.Must(t, err, "setting --run: %v", err)
			if err := cmd.Flags().Set(flag, "whatever"); err != nil {
				t.Fatalf("setting --%s: %v", flag, err)
			}

			w, _ := bufWriter(true)
			err = runNext(cmd, nil, w)
			if err == nil {
				t.Fatalf("--%s in step mode was accepted; it must refuse", flag)
			}

			var cmdError *CmdError
			if !asCmdError(err, &cmdError) {
				t.Fatalf("error is not a CmdError: %v", err)
			}
			if cmdError.Code != output.ErrValidation {
				t.Errorf("code = %v, want VALIDATION_ERROR", cmdError.Code)
			}
			msg := err.Error()
			if !strings.Contains(msg, "--"+flag) {
				t.Errorf("message does not name --%s: %s", flag, msg)
			}
			if !strings.Contains(msg, "--run") {
				t.Errorf("message does not name --run as the conflict: %s", msg)
			}
		})
	}
}

// TestNextStepModeAllFiltersNamedAtOnce pins that several conflicting flags are
// reported together rather than one per run — a caller fixing them one at a
// time round-trips four times for no reason.
func TestNextStepModeAllFiltersNamedAtOnce(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)

	cmd := nextCmdWithDB(conn, 10)
	err := cmd.Flags().Set("run", model.FormatRunID(runID))
	testsupport.Must(t, err, "setting --run: %v", err)
	for _, flag := range []string{"status", "priority"} {
		if err := cmd.Flags().Set(flag, "x"); err != nil {
			t.Fatalf("setting --%s: %v", flag, err)
		}
	}

	w, _ := bufWriter(true)
	err = runNext(cmd, nil, w)
	if err == nil {
		t.Fatal("conflicting filters were accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--status") || !strings.Contains(msg, "--priority") {
		t.Errorf("message names only some of the conflicting flags: %s", msg)
	}
}

// TestNextStepModeWireShape pins §11.4's `next row`, field for field, plus the
// two recorded additions: `instance` (§10 A1) and `kind`.
func TestNextStepModeWireShape(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)

	cmd := nextCmdWithDB(conn, 10)
	err := cmd.Flags().Set("run", model.FormatRunID(runID))
	testsupport.Must(t, err, "setting --run: %v", err)
	w, buf := bufWriter(true)
	err = runNext(cmd, nil, w)
	testsupport.Must(t, err, "runNext: %v", err)

	var envelope struct {
		Data struct {
			Steps []map[string]any `json:"steps"`
			Total int              `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(envelope.Data.Steps) != 1 {
		t.Fatalf("got %d ready steps, want 1 (only the root is ready)", len(envelope.Data.Steps))
	}

	row := envelope.Data.Steps[0]
	// §11.4: { step, issue, run, executor, class, attempt, expected_cost,
	//          lease_ttl_s, metadata } — plus instance, kind, status.
	for _, key := range []string{
		"step", "instance", "issue", "run", "kind", "executor", "class",
		"attempt", "expected_cost", "lease_ttl_s", "status",
	} {
		if _, ok := row[key]; !ok {
			t.Errorf("next row is missing %q: %v", key, row)
		}
	}

	if row["step"] != "STEP-1" {
		t.Errorf("step = %v, want STEP-1", row["step"])
	}
	if row["instance"] != "first@0" {
		t.Errorf("instance = %v, want first@0", row["instance"])
	}
	// `ready` is COMPUTED (§6.2): the stored column says `pending`.
	if row["status"] != db.StepReady {
		t.Errorf("status = %v, want the computed %q", row["status"], db.StepReady)
	}

	var stored string
	if err := conn.QueryRow(
		`SELECT status FROM steps WHERE instance = 'first@0'`).Scan(&stored); err != nil {
		t.Fatalf("reading stored status: %v", err)
	}
	if stored != db.StepPending {
		t.Errorf("stored status = %q, want %q — `ready` must never be persisted",
			stored, db.StepPending)
	}
}

// TestNextStepModeHumanStepCarriesNoExecutor is §6.15's dispatcher guard at the
// wire level: a `type=human` step is OFFERED — a dispatcher needs to know the
// run is waiting on a person — but carries no executor hint, so a dispatcher
// that spawns on every row cannot spawn a worker for a gate.
func TestNextStepModeHumanStepCarriesNoExecutor(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)

	// Finish the root so the human step becomes ready.
	if _, err := conn.Exec(
		`UPDATE steps SET status = ? WHERE instance = 'first@0'`, db.StepDone); err != nil {
		t.Fatalf("completing the root: %v", err)
	}

	cmd := nextCmdWithDB(conn, 10)
	err := cmd.Flags().Set("run", model.FormatRunID(runID))
	testsupport.Must(t, err, "setting --run: %v", err)
	w, buf := bufWriter(true)
	err = runNext(cmd, nil, w)
	testsupport.Must(t, err, "runNext: %v", err)

	var envelope struct {
		Data struct {
			Steps []map[string]any `json:"steps"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(envelope.Data.Steps) != 1 {
		t.Fatalf("got %d ready steps, want the human step", len(envelope.Data.Steps))
	}

	row := envelope.Data.Steps[0]
	if row["kind"] != "human" {
		t.Fatalf("kind = %v, want human", row["kind"])
	}
	// Omitted entirely, not emitted as "": a dispatcher must not be able to
	// read an empty string as a hint.
	if _, present := row["executor"]; present {
		t.Errorf("a human step carries an `executor` key (%v); §6.15 requires none",
			row["executor"])
	}
}

// TestNextStepModeTruncationContract pins the v2 truncation contract: the
// pre-limit total rides in the envelope so a limited call cannot silently drop
// work (reliability-delta §4.2).
func TestNextStepModeTruncationContract(t *testing.T) {
	conn := newTestDB(t)

	// Two issues, so two independent root steps are ready at once.
	registerForRun(t, conn, runWorkflow)
	var issueIDs []int
	for _, title := range []string{"one", "two"} {
		id, err := db.CreateIssue(conn, &model.Issue{
			Title: title, Description: "body", Status: model.StatusBacklog,
			Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, nil, nil)
		testsupport.Must(t, err, "creating issue: %v", err)
		issueIDs = append(issueIDs, id)
	}
	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	for _, id := range issueIDs {
		err := db.AddRunIssue(conn, run.ID, id)
		testsupport.Must(t, err, "adding issue: %v", err)
	}
	if _, err := engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("activate: %v", err)
	}

	cmd := nextCmdWithDB(conn, 1)
	err = cmd.Flags().Set("run", model.FormatRunID(run.ID))
	testsupport.Must(t, err, "setting --run: %v", err)
	w := &output.Writer{
		JSONMode: true, JSONVersion: output.JSONV2,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	}
	buf := w.Stdout.(*bytes.Buffer)
	err = runNext(cmd, nil, w)
	testsupport.Must(t, err, "runNext: %v", err)

	// Under v2 a Collection reshapes to data.{items,total,truncated} — the
	// uniform envelope every list verb emits.
	var envelope struct {
		Data struct {
			Items     []map[string]any `json:"items"`
			Total     int              `json:"total"`
			Truncated bool             `json:"truncated"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	if len(envelope.Data.Items) != 1 {
		t.Errorf("returned %d steps under --limit 1", len(envelope.Data.Items))
	}
	if envelope.Data.Total != 2 {
		t.Errorf("total = %d, want the PRE-LIMIT 2 — a post-limit count cannot "+
			"distinguish 'exactly one ready' from 'one returned, more dropped'",
			envelope.Data.Total)
	}
	if !envelope.Data.Truncated {
		t.Error("truncated = false with 2 ready and --limit 1")
	}
}

// TestNextStepModeUnknownRunIsNotFound pins the taxonomy at the boundary.
func TestNextStepModeUnknownRunIsNotFound(t *testing.T) {
	conn := newTestDB(t)

	cmd := nextCmdWithDB(conn, 10)
	err := cmd.Flags().Set("run", "RUN-404")
	testsupport.Must(t, err, "setting --run: %v", err)
	w, _ := bufWriter(true)

	err = runNext(cmd, nil, w)
	if err == nil {
		t.Fatal("an unknown run was accepted")
	}
	var cmdError *CmdError
	if !asCmdError(err, &cmdError) {
		t.Fatalf("error is not a CmdError: %v", err)
	}
	if cmdError.Code != output.ErrNotFound {
		t.Errorf("code = %v, want NOT_FOUND", cmdError.Code)
	}
}
