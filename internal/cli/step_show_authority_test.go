package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// heldClusterStep drives an aggregate over a payload whose one cluster spreads
// past `hold_spread`, so the engine materializes `reconcile-held@0#0`, and
// returns that held step's id. Approving it is decideMaterializedStep's path.
func heldClusterStep(t *testing.T, conn *sql.DB) int {
	t.Helper()
	testsupport.Must(t, registerSchema(t, conn, "findings@1", findingsSchema),
		"registering findings@1: %v", nil)
	registerForRun(t, conn, `
[pipeline]
name = "holds"
version = 1
[match]
kind = ["task"]
[[step]]
name = "synthesize"
after = []
executor = "w"
emits = "findings"
[[step]]
name = "reconcile"
after = ["synthesize"]
action = "aggregate"
params = { field = "severity", method = "max", hold_spread = 2, output = "findings" }
inputs = ["synthesize.findings"]
payload = "findings@1"
`)
	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "hold me", Description: "a body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)
	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "adding issue: %v", nil)
	activateHolding(t, conn, run.ID, model.NowMS())

	synthesize := stepIDNamed(t, conn, "synthesize@0")
	claim, err := engine.ClaimStep(conn, synthesize,
		engine.ClaimOptions{Owner: "w", NowMS: model.NowMS()})
	testsupport.Must(t, err, "claim: %v", err)
	e := engine.NewEngine()
	err = e.CompleteStep(conn, synthesize, engine.CompleteOptions{
		Token: claim.Token, Artifact: []byte("synthesized"),
		Payload: []byte(`[{"id":"C-1","severity":["low","blocker"]}]`),
		NowMS:   model.NowMS(),
	})
	testsupport.Must(t, err, "complete: %v", err)
	testsupport.Must(t, e.RunActionStep(conn, stepIDNamed(t, conn, "reconcile@0"), model.NowMS()),
		"running reconcile@0: %v", nil)
	return stepIDNamed(t, conn, "reconcile-held@0#0")
}

// stepShowV2Row runs `step show --json=v2` on one step and returns its row's
// top-level keys undecoded, so an absent key is distinguishable from an empty
// one.
func stepShowV2Row(t *testing.T, conn *sql.DB, stepID int) (map[string]json.RawMessage, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := &output.Writer{JSONMode: true, JSONVersion: output.JSONV2, Stdout: buf, Stderr: &bytes.Buffer{}}
	err := runStepShow(cmdWithDB(conn), []string{model.FormatStepID(stepID)}, w)
	testsupport.Must(t, err, "step show: %v\n%s", err, buf.String())

	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	return envelope.Data, buf.String()
}

// TestStepShowReportsResolutionAuthority: a resolved step's `step show` row
// says under what authority it was resolved, from each of the three ruling
// sites, and names the grant only when a standing grant applied.
func TestStepShowReportsResolutionAuthority(t *testing.T) {
	const grantRef = "RUN NOTE 54"

	sites := []struct {
		name    string
		resolve func(t *testing.T, conn *sql.DB, flags map[string]string) int
	}{
		{name: "DecideStepWith", resolve: func(t *testing.T, conn *sql.DB, flags map[string]string) int {
			id := readyGate(t, conn)
			cmd := authorityStepCmd(conn)
			setAuthorityFlags(t, cmd, flags)
			w, buf := bufWriter(true)
			testsupport.Must(t, runDecide(cmd, []string{model.FormatStepID(id)}, true, w),
				"approve: %s", buf.String())
			return id
		}},
		{name: "resolveStep", resolve: func(t *testing.T, conn *sql.DB, flags map[string]string) int {
			id := parkedStep(t, conn)
			cmd := authorityStepCmd(conn)
			setAuthorityFlags(t, cmd, flags)
			testsupport.Must(t, cmd.Flags().Set("as", engine.ResolveSkip), "set --as: %v", nil)
			w, buf := bufWriter(true)
			testsupport.Must(t, runStepResolve(cmd, []string{model.FormatStepID(id)}, w),
				"resolve: %s", buf.String())
			return id
		}},
		{name: "decideMaterializedStep", resolve: func(t *testing.T, conn *sql.DB, flags map[string]string) int {
			id := heldClusterStep(t, conn)
			cmd := authorityStepCmd(conn)
			setAuthorityFlags(t, cmd, flags)
			w, buf := bufWriter(true)
			testsupport.Must(t, runDecide(cmd, []string{model.FormatStepID(id)}, true, w),
				"approve the held cluster: %s", buf.String())
			return id
		}},
	}

	for _, site := range sites {
		for _, authority := range []string{
			engine.AuthorityOperator, engine.AuthorityStandingGrant, engine.AuthorityConductor,
		} {
			t.Run(site.name+"/"+authority, func(t *testing.T) {
				conn := newTestDB(t)
				flags := map[string]string{"authority": authority}
				wantRef := ""
				if authority == engine.AuthorityStandingGrant {
					wantRef = grantRef
					flags["authority-ref"] = grantRef
				}
				id := site.resolve(t, conn, flags)

				row, raw := stepShowV2Row(t, conn, id)
				var got string
				if err := json.Unmarshal(row["authority"], &got); err != nil || got != authority {
					t.Errorf("authority = %s, want %q:\n%s", row["authority"], authority, raw)
				}

				gotRef, present := row["authority_ref"]
				if wantRef == "" {
					if present {
						t.Errorf("authority_ref = %s on a %s resolution; it names a "+
							"standing grant and must be absent otherwise:\n%s", gotRef, authority, raw)
					}
					return
				}
				var ref string
				if err := json.Unmarshal(gotRef, &ref); err != nil || ref != wantRef {
					t.Errorf("authority_ref = %s (present %t), want %q:\n%s", gotRef, present, wantRef, raw)
				}
			})
		}
	}
}
