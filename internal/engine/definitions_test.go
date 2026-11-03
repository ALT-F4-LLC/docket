package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A run whose issue is bound but not yet expanded has no step rows, so its
// definition reaches StepDefinitions only through the binding. The Tx form
// reads expanded steps alone and so finds nothing.
func TestStepDefinitionsIncludeABoundWorkflowWithNoExpandedSteps(t *testing.T) {
	conn := mustDB(t)
	wf := registerFixture(t, conn)
	issue := createIssue(t, conn, "bound", "body", "task", nil)
	run := startRun(t, conn, issue)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	workflowID := wf.ID
	err = db.BindRunIssueTx(tx, &db.RunIssue{RunID: run.ID, IssueID: issue, WorkflowID: &workflowID})
	testsupport.Must(t, err, "binding the issue: %v", err)
	testsupport.Must(t, tx.Commit(), "commit")

	defs, err := StepDefinitions(conn, run.ID)
	testsupport.Must(t, err, "StepDefinitions: %v", err)
	if len(defs) != 1 || defs[wf.ID] == nil {
		t.Fatalf("StepDefinitions = %v, want exactly the bound workflow %d", defs, wf.ID)
	}
	if got := defs[wf.ID].Pipeline.Name; got != wf.Name {
		t.Errorf("bound definition pipeline = %q, want %q", got, wf.Name)
	}

	readTx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer func() { _ = readTx.Rollback() }()
	txDefs, err := StepDefinitionsTx(readTx, run.ID)
	testsupport.Must(t, err, "StepDefinitionsTx: %v", err)
	if len(txDefs) != 0 {
		t.Errorf("StepDefinitionsTx = %v, want none: the run has no expanded steps", txDefs)
	}
}
