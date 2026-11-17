package cli

import (
	"maps"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// An unanchored caller with no project is refused before the run is looked
// up, as the root hook refused it before run hold-issue resolved its own
// project.
func TestRunHoldIssueRefusesAnUnanchoredCaller(t *testing.T) {
	conn := newTestDB(t)
	unanchored := &config.Config{Identity: "/work/scratch"}
	testsupport.Must(t, runHoldIssueCmd.Flags().Set("reason", "x"), "set --reason")

	_, err := runLeaseCommandOutput(t, conn, unanchored, runHoldIssueCmd, "RUN-1", "DKT-1")
	requireCmdError(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	if err != nil && strings.Contains(err.Error(), "run RUN-1 not found") {
		t.Errorf("error = %v, want the refusal before the run lookup", err)
	}
}

// An anchored caller whose project is not yet registered registers it on
// first contact and records one project-registered event naming the verb.
func TestRunHoldIssueRecordsTheProjectItRegisters(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := seedRun(t, conn)
	activateHolding(t, conn, runID, model.NowMS())
	const identity = "/repos/hold-issue"
	anchored := &config.Config{Identity: identity, Anchored: true}
	testsupport.Must(t, runHoldIssueCmd.Flags().Set("reason", "operator note 1"), "set --reason")

	_, err := runLeaseCommandOutput(t, conn, anchored, runHoldIssueCmd,
		model.FormatRunID(runID), model.FormatID(issueID))
	testsupport.Must(t, err, "run hold-issue: %v", err)

	var projects int
	testsupport.Must(t, conn.QueryRow(`SELECT COUNT(*) FROM projects WHERE identity = ?`, identity).
		Scan(&projects), "counting projects")
	if projects != 1 {
		t.Errorf("%d project rows for %s, want 1", projects, identity)
	}
	want := map[string]string{identity: "run hold-issue"}
	if got := registrationEvents(t, conn); !maps.Equal(got, want) {
		t.Errorf("registration events = %v, want %v", got, want)
	}
}
