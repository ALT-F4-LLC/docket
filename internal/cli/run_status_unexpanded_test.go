package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4255: `run status` states which bound issues hold no step and what keeps
// them from getting one, so a close-out question cannot omit blocked roster
// issues or call a blocked one "absent from the run".

// unexpandedFixture builds a run where issue A holds a step but is not done,
// B depends on A and C depends on B, and neither B nor C holds a step. D is
// bound with no step and no blocker. E holds a step and is therefore not
// listed.
func unexpandedFixture(t *testing.T, conn *sql.DB) (run *model.Run, a, b, c, d int) {
	t.Helper()
	mk := func(title string) int {
		return createIssue(t, conn, title, model.StatusBacklog, model.PriorityNone)
	}
	a, b, c, d = mk("A held"), mk("B behind A"), mk("C behind B"), mk("D free")
	e := mk("E stepped")

	for _, rel := range []model.Relation{
		{SourceIssueID: b, TargetIssueID: a, RelationType: model.RelationDependsOn},
		{SourceIssueID: c, TargetIssueID: b, RelationType: model.RelationDependsOn},
	} {
		_, err := db.CreateRelation(conn, &rel)
		testsupport.Must(t, err, "CreateRelation: %v", err)
	}

	wf, _, err := db.InsertWorkflow(conn, &model.Workflow{
		Name: "unexpanded", Version: 1, SourcePath: "unexpanded.toml",
		SourceSHA256: "0", Body: "", Parsed: "{}",
	}, 1)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)

	run, err = db.InsertRun(conn, 1, "a run", 0, 1)
	testsupport.Must(t, err, "InsertRun: %v", err)
	for _, id := range []int{a, b, c, d, e} {
		testsupport.Must(t, db.AddRunIssue(conn, run.ID, id), "AddRunIssue")
	}
	testsupport.Must(t, db.SetRunStatus(conn, run.ID, model.RunActive, "", 1), "SetRunStatus")
	for _, id := range []int{a, e} {
		_, err = conn.Exec(
			`INSERT INTO steps
			   (run_id, issue_id, workflow_id, step_name, instance, kind, status,
			    created_at_ms, updated_at_ms)
			 VALUES (?, ?, ?, 'implement', 'implement@0', 'write', 'pending', 1, 1)`,
			run.ID, id, wf.ID)
		testsupport.Must(t, err, "inserting a step: %v", err)
	}
	return run, a, b, c, d
}

type unexpandedStatus struct {
	Data struct {
		Unexpanded []struct {
			Issue     string `json:"issue"`
			Status    string `json:"status"`
			BlockedBy [][]struct {
				Issue  string `json:"issue"`
				Status string `json:"status"`
			} `json:"blocked_by"`
		} `json:"unexpanded"`
	} `json:"data"`
}

func decodeRunStatus(t *testing.T, conn *sql.DB, runRef string) unexpandedStatus {
	t.Helper()
	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	testsupport.Must(t, runRunStatus(cmd, []string{runRef}, w), "run status")
	var got unexpandedStatus
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &got), "decoding %s", buf.String())
	return got
}

// TestRunStatusListsUnexpandedIssuesWithBlockerChains is the acceptance check:
// B and C appear with blocked_by chains that end at the held issue A, and D
// appears unblocked. A and E hold steps and are absent.
func TestRunStatusListsUnexpandedIssuesWithBlockerChains(t *testing.T) {
	conn := newTestDB(t)
	run, a, b, c, d := unexpandedFixture(t, conn)

	got := decodeRunStatus(t, conn, run.Ref())

	listed := map[string]int{}
	for i, u := range got.Data.Unexpanded {
		listed[u.Issue] = i
	}
	if len(got.Data.Unexpanded) != 3 {
		t.Fatalf("unexpanded = %+v; want exactly B, C and D", got.Data.Unexpanded)
	}

	cItem := got.Data.Unexpanded[listed[model.FormatID(c)]]
	if len(cItem.BlockedBy) != 1 {
		t.Fatalf("C blocked_by = %+v; want one chain", cItem.BlockedBy)
	}
	chain := cItem.BlockedBy[0]
	if len(chain) != 2 || chain[0].Issue != model.FormatID(b) || chain[1].Issue != model.FormatID(a) {
		t.Errorf("C chain = %+v; want %s -> %s", chain, model.FormatID(b), model.FormatID(a))
	}
	if chain[1].Status != string(model.StatusBacklog) {
		t.Errorf("chain root status = %q; want backlog", chain[1].Status)
	}

	bItem := got.Data.Unexpanded[listed[model.FormatID(b)]]
	if len(bItem.BlockedBy) != 1 || len(bItem.BlockedBy[0]) != 1 ||
		bItem.BlockedBy[0][0].Issue != model.FormatID(a) {
		t.Errorf("B blocked_by = %+v; want the single chain [%s]", bItem.BlockedBy, model.FormatID(a))
	}

	dItem := got.Data.Unexpanded[listed[model.FormatID(d)]]
	if len(dItem.BlockedBy) != 0 {
		t.Errorf("D blocked_by = %+v; want none", dItem.BlockedBy)
	}
}

// TestRunStatusDropsDoneBlockersFromChains: a blocker that is done is met and
// does not hold the issue, so it leaves the chain.
func TestRunStatusDropsDoneBlockersFromChains(t *testing.T) {
	conn := newTestDB(t)
	run, a, b, _, _ := unexpandedFixture(t, conn)
	_, err := conn.Exec(`UPDATE issues SET status = ? WHERE id = ?`, string(model.StatusDone), a)
	testsupport.Must(t, err, "marking A done: %v", err)

	got := decodeRunStatus(t, conn, run.Ref())
	for _, u := range got.Data.Unexpanded {
		if u.Issue == model.FormatID(b) && len(u.BlockedBy) != 0 {
			t.Errorf("B still blocked by %+v after its blocker is done", u.BlockedBy)
		}
	}
}

// TestRunStatusOmitsUnexpandedWhenEveryIssueHoldsAStep: a fully expanded run's
// output carries no unexpanded key, so it stays byte-identical to before.
func TestRunStatusOmitsUnexpandedWhenEveryIssueHoldsAStep(t *testing.T) {
	conn := newTestDB(t)
	run, _, b, c, d := unexpandedFixture(t, conn)
	_, err := conn.Exec(
		`INSERT INTO steps (run_id, issue_id, workflow_id, step_name, instance, kind, status,
		                    created_at_ms, updated_at_ms)
		 SELECT run_id, ?, workflow_id, step_name, instance, kind, status, 1, 1
		   FROM steps WHERE run_id = ? LIMIT 1`, b, run.ID)
	testsupport.Must(t, err, "stepping B: %v", err)
	for _, id := range []int{c, d} {
		_, err = conn.Exec(
			`INSERT INTO steps (run_id, issue_id, workflow_id, step_name, instance, kind, status,
			                    created_at_ms, updated_at_ms)
			 SELECT run_id, ?, workflow_id, step_name, instance, kind, status, 1, 1
			   FROM steps WHERE run_id = ? LIMIT 1`, id, run.ID)
		testsupport.Must(t, err, "stepping an issue: %v", err)
	}

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	testsupport.Must(t, runRunStatus(cmd, []string{run.Ref()}, w), "run status")
	if strings.Contains(buf.String(), "unexpanded") {
		t.Errorf("a fully expanded run carries an unexpanded key: %s", buf.String())
	}
}

// TestRunStatusTextNamesUnexpandedChains: the human rendering states the same
// block, so a close-out question can paste it.
func TestRunStatusTextNamesUnexpandedChains(t *testing.T) {
	conn := newTestDB(t)
	run, a, b, c, _ := unexpandedFixture(t, conn)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	testsupport.Must(t, runRunStatus(cmd, []string{run.Ref()}, w), "run status")

	for _, want := range []string{
		"Unexpanded (no steps yet): 3",
		model.FormatID(c) + " (backlog)",
		"blocked by " + model.FormatID(b) + " (backlog) -> " + model.FormatID(a) + " (backlog)",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("status text missing %q:\n%s", want, buf.String())
		}
	}
}
