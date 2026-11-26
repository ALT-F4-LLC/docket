package api

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/store/storetest"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// validClaim is a request on stepID every boundary check accepts.
func validClaim(stepID int) *docketv1.StepClaimRequest {
	return &docketv1.StepClaimRequest{Step: model.FormatStepID(stepID), Owner: "w", RecoverySecret: "s"}
}

func assertValidationRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("the claim was accepted, want an invalid_argument refusal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("connect code = %v, want %v (err: %v)", got, connect.CodeInvalidArgument, err)
	}
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("docket code = %s, want %s", got, output.ErrValidation)
	}
}

// assertNotClaimed fails when the step recorded a claim.
func assertNotClaimed(t *testing.T, conn *sql.DB, stepID int) {
	t.Helper()
	if n := countEvents(t, conn, engine.EventStepClaimed, stepID); n != 0 {
		t.Errorf("%d step-claimed events after a refused claim, want none", n)
	}
}

func TestClaimRequiresRecoverySecret(t *testing.T) {
	for _, secret := range []string{"", " \t\n"} {
		for _, render := range []bool{false, true} {
			conn, stepID, _ := activatedClaimDB(t)
			client := engineClaimClient(t, conn)
			req := validClaim(stepID)
			req.RecoverySecret = secret
			req.Render = render

			_, err := client.StepClaim(context.Background(), connect.NewRequest(req))

			assertValidationRefusal(t, err)
			assertNotClaimed(t, conn, stepID)
		}
	}
}

func TestClaimForwardsTheRecoverySecretUnchanged(t *testing.T) {
	for _, render := range []bool{false, true} {
		conn, stepID, _ := activatedClaimDB(t)
		client := engineClaimClient(t, conn)
		req := validClaim(stepID)
		req.RecoverySecret = " s "
		req.Render = render
		req.Executor = "hint"

		res, err := client.StepClaim(context.Background(), connect.NewRequest(req))
		testsupport.Must(t, err, "render=%v: a valid claim was refused: %v", render, err)
		if render && !strings.Contains(res.Msg.GetPacket(), "target: hint") {
			t.Errorf("rendered claim: the packet does not carry the executor hint:\n%s", res.Msg.GetPacket())
		}
		if !render && res.Msg.GetContext().GetStep().GetStep() != model.FormatStepID(stepID) {
			t.Errorf("claim: context bundle = %v, want %s's", res.Msg.GetContext(), model.FormatStepID(stepID))
		}

		// Only the secret the claim stored, untrimmed, re-mints the lease.
		trimmed := validClaim(stepID)
		_, err = client.StepClaim(context.Background(), connect.NewRequest(trimmed))
		if got := output.ErrorCodeFromConnectError(err); got != output.ErrConflict {
			t.Errorf("render=%v: a re-claim with the trimmed secret: code %s (err %v), want %s",
				render, got, err, output.ErrConflict)
		}
		again := validClaim(stepID)
		again.RecoverySecret = " s "
		reminted, err := client.StepClaim(context.Background(), connect.NewRequest(again))
		testsupport.Must(t, err, "render=%v: a re-claim with the stored secret was refused: %v", render, err)
		if !reminted.Msg.GetReMinted() {
			t.Errorf("render=%v: the re-claim with the stored secret did not re-mint", render)
		}
	}
}

func TestClaimRefusesAClientTemplatePath(t *testing.T) {
	conn, stepID, _ := activatedClaimDB(t)
	client := engineClaimClient(t, conn)
	req := validClaim(stepID)
	req.Render = true
	req.Template = "/etc/passwd"

	_, err := client.StepClaim(context.Background(), connect.NewRequest(req))

	assertValidationRefusal(t, err)
	assertNotClaimed(t, conn, stepID)
}

func TestClaimRefusesInvalidBoundaryFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*docketv1.StepClaimRequest)
	}{
		{"empty owner", func(r *docketv1.StepClaimRequest) { r.Owner = "" }},
		{"zero ttl", func(r *docketv1.StepClaimRequest) { r.TtlMs = proto.Int64(0) }},
		{"negative ttl", func(r *docketv1.StepClaimRequest) { r.TtlMs = proto.Int64(-1) }},
		{"zero cost multiplier", func(r *docketv1.StepClaimRequest) { r.CostMultiplier = proto.Float64(0) }},
		{"negative cost multiplier", func(r *docketv1.StepClaimRequest) { r.CostMultiplier = proto.Float64(-2) }},
		{"NaN cost multiplier", func(r *docketv1.StepClaimRequest) { r.CostMultiplier = proto.Float64(math.NaN()) }},
		{"infinite cost multiplier", func(r *docketv1.StepClaimRequest) { r.CostMultiplier = proto.Float64(math.Inf(1)) }},
		{"oversize secret", func(r *docketv1.StepClaimRequest) { r.RecoverySecret = strings.Repeat("s", 4097) }},
		{"malformed step", func(r *docketv1.StepClaimRequest) { r.Step = "not-a-step" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, stepID, _ := activatedClaimDB(t)
			client := engineClaimClient(t, conn)
			req := validClaim(stepID)
			tc.edit(req)

			_, err := client.StepClaim(context.Background(), connect.NewRequest(req))

			assertValidationRefusal(t, err)
			assertNotClaimed(t, conn, stepID)
		})
	}
}

// claimEventData is the data the step's one step-claimed event carries.
func claimEventData(t *testing.T, conn *sql.DB, stepID int) string {
	t.Helper()
	var data string
	err := conn.QueryRow(`SELECT COALESCE(data, '') FROM events WHERE kind = ? AND step_id = ?`,
		engine.EventStepClaimed, stepID).Scan(&data)
	testsupport.Must(t, err, "reading the claim event: %v", err)
	return data
}

func TestClaimForwardsOptionalFieldsOnlyWhenPresent(t *testing.T) {
	const ttl = int64(60000)
	inWindow := func(expires, from, to int64) bool { return expires >= from+ttl && expires <= to+ttl }

	conn, stepID, _ := activatedClaimDB(t)
	from := model.NowMS()
	plain, err := engineClaimClient(t, conn).StepClaim(context.Background(), connect.NewRequest(validClaim(stepID)))
	testsupport.Must(t, err, "a claim without optional fields was refused: %v", err)
	if inWindow(plain.Msg.GetLeaseExpiresMs(), from, model.NowMS()) {
		t.Errorf("a claim without ttl_ms leased for %d ms, want the step's configured TTL", ttl)
	}
	if data := claimEventData(t, conn, stepID); strings.Contains(data, "expected_cost") {
		t.Errorf("a claim without cost_multiplier recorded a scaled cost: %s", data)
	}

	conn, stepID, _ = activatedClaimDB(t)
	req := validClaim(stepID)
	req.TtlMs = proto.Int64(ttl)
	req.CostMultiplier = proto.Float64(2.5)
	from = model.NowMS()
	scaled, err := engineClaimClient(t, conn).StepClaim(context.Background(), connect.NewRequest(req))
	testsupport.Must(t, err, "a claim with ttl and multiplier was refused: %v", err)
	if !inWindow(scaled.Msg.GetLeaseExpiresMs(), from, model.NowMS()) {
		t.Errorf("lease_expires_ms = %d, want %d ms after the claim", scaled.Msg.GetLeaseExpiresMs(), ttl)
	}
	if data := claimEventData(t, conn, stepID); !strings.Contains(data, "expected_cost") {
		t.Errorf("a claim with cost_multiplier recorded no scaled cost: %q", data)
	}
}

// claimWorkflow has one claimable step and a human gate after it.
const claimWorkflow = `
[pipeline]
name = "api-claim"
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

// activatedClaimDB returns a store holding one activated run of claimWorkflow
// and the ids of its two steps.
func activatedClaimDB(t *testing.T) (conn *sql.DB, first, second int) {
	t.Helper()
	// Activation reads config beside the store; an empty one keeps the
	// operator's own config out of the run.
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	conn, err := db.Open(":memory:")
	testsupport.Must(t, err, "Open: %v", err)
	t.Cleanup(func() { conn.Close() })
	storetest.SeedMemory(t, conn)
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)

	def, err := workflow.Load([]byte(claimWorkflow))
	testsupport.Must(t, err, "loading the workflow: %v", err)
	parsed, err := workflow.Canonical(def)
	testsupport.Must(t, err, "serializing the workflow: %v", err)
	_, _, err = db.InsertWorkflow(conn, &model.Workflow{
		Name: def.Pipeline.Name, Version: def.Pipeline.Version,
		SourceSHA256: workflow.SHA256([]byte(claimWorkflow)),
		Body:         claimWorkflow, Parsed: string(parsed),
	}, model.NowMS())
	testsupport.Must(t, err, "registering the workflow: %v", err)

	issue, err := db.CreateIssue(conn, &model.Issue{
		Title: "claim me", Description: "a body",
		Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating the issue: %v", err)
	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting the run: %v", err)
	err = db.AddRunIssue(conn, run.ID, issue)
	testsupport.Must(t, err, "adding the issue: %v", err)
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "activating the run: %v", err)

	for name, id := range map[string]*int{"first": &first, "second": &second} {
		err := conn.QueryRow(`SELECT id FROM steps WHERE run_id = ? AND step_name = ?`, run.ID, name).Scan(id)
		testsupport.Must(t, err, "reading step %s: %v", name, err)
	}
	return conn, first, second
}

// engineClaimClient is a client of a Service over conn.
func engineClaimClient(t *testing.T, conn *sql.DB) docketv1connect.DocketServiceClient {
	t.Helper()
	server := serve(t, NewService(store.StoreOn(conn), "test"))
	return client.NewClient(server.Client(), server.URL, discardLogger)
}

func claimRequest(stepID int, owner, secret string) *connect.Request[docketv1.StepClaimRequest] {
	return connect.NewRequest(&docketv1.StepClaimRequest{
		Step: model.FormatStepID(stepID), Owner: owner, RecoverySecret: secret,
	})
}

func countEvents(t *testing.T, conn *sql.DB, kind string, stepID int) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND step_id = ?`, kind, stepID).Scan(&n)
	testsupport.Must(t, err, "counting %s events: %v", kind, err)
	return n
}

func TestClaimRetryRemintsWithSecret(t *testing.T) {
	conn, stepID, _ := activatedClaimDB(t)
	client := engineClaimClient(t, conn)
	const owner, secret, wrong = "wave:STEP-1", "recovery-secret-for-api-test", "a-different-secret"

	first, err := client.StepClaim(context.Background(), claimRequest(stepID, owner, secret))
	testsupport.Must(t, err, "the first claim: %v", err)

	_, err = client.StepClaim(context.Background(), claimRequest(stepID, owner, wrong))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrConflict {
		t.Fatalf("a re-claim with the wrong secret: code %s (err %v), want %s", got, err, output.ErrConflict)
	}
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("a re-claim with the wrong secret: connect code %v, want aborted", connect.CodeOf(err))
	}
	if msg := err.Error(); strings.Contains(msg, wrong) || strings.Contains(msg, secret) ||
		strings.Contains(msg, first.Msg.GetToken()) {
		t.Errorf("the refusal echoes a secret or the token: %q", msg)
	}

	second, err := client.StepClaim(context.Background(), claimRequest(stepID, owner, secret))
	testsupport.Must(t, err, "the same-owner re-claim with the secret was refused: %v", err)

	if !second.Msg.GetReMinted() {
		t.Error("the re-claim reported a fresh claim, not a re-mint")
	}
	if second.Msg.GetToken() == first.Msg.GetToken() {
		t.Error("the re-claim returned the first claim's token")
	}
	if second.Msg.GetAttempt() != first.Msg.GetAttempt() {
		t.Errorf("attempt = %d after the re-mint, want %d", second.Msg.GetAttempt(), first.Msg.GetAttempt())
	}
	now := model.NowMS()
	if err := db.AuthorizeStepRead(conn, stepID, second.Msg.GetToken(), now); err != nil {
		t.Errorf("the re-minted token does not authorize the lease: %v", err)
	}
	if db.AuthorizeStepRead(conn, stepID, first.Msg.GetToken(), now) == nil {
		t.Error("the first claim's token still authorizes after the re-mint")
	}
}
