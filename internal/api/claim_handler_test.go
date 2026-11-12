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
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// recordingClaimer is a stepClaimer that records each claim it receives and
// answers with a fixed lease.
type recordingClaimer struct {
	calls []recordedClaim
}

type recordedClaim struct {
	stepID   int
	opts     engine.ClaimOptions
	rendered bool
	executor string
}

func (c *recordingClaimer) ClaimStepWithGates(stepID int, opts engine.ClaimOptions) (*engine.ClaimResult, error) {
	c.calls = append(c.calls, recordedClaim{stepID: stepID, opts: opts})
	return &engine.ClaimResult{Step: model.FormatStepID(stepID), Token: "lease-token", Attempt: 1}, nil
}

func (c *recordingClaimer) ClaimStepRendered(stepID int, opts engine.ClaimOptions, executor string) (*engine.ClaimResult, *engine.RenderResult, error) {
	c.calls = append(c.calls, recordedClaim{stepID: stepID, opts: opts, rendered: true, executor: executor})
	return &engine.ClaimResult{Step: model.FormatStepID(stepID), Token: "lease-token", Attempt: 1},
		&engine.RenderResult{Packet: "packet"}, nil
}

func claimClient(t *testing.T, claimer stepClaimer) docketv1connect.DocketServiceClient {
	t.Helper()
	server := serve(t, ClaimHandler{claimer: claimer})
	return client.NewClient(server.Client(), server.URL, discardLogger)
}

// validClaim is a request every boundary check accepts.
func validClaim() *docketv1.StepClaimRequest {
	return &docketv1.StepClaimRequest{Step: "STEP-1", Owner: "w", RecoverySecret: "s"}
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

func TestClaimRequiresRecoverySecret(t *testing.T) {
	for _, secret := range []string{"", " \t\n"} {
		for _, render := range []bool{false, true} {
			claimer := &recordingClaimer{}
			client := claimClient(t, claimer)
			req := validClaim()
			req.RecoverySecret = secret
			req.Render = render

			_, err := client.StepClaim(context.Background(), connect.NewRequest(req))

			assertValidationRefusal(t, err)
			if len(claimer.calls) != 0 {
				t.Errorf("secret %q, render=%v: the engine claim ran %d time(s), want none",
					secret, render, len(claimer.calls))
			}
		}
	}
}

func TestClaimForwardsTheRecoverySecretUnchanged(t *testing.T) {
	for _, render := range []bool{false, true} {
		claimer := &recordingClaimer{}
		client := claimClient(t, claimer)
		req := validClaim()
		req.RecoverySecret = " s "
		req.Render = render
		req.Executor = "hint"

		res, err := client.StepClaim(context.Background(), connect.NewRequest(req))

		testsupport.Must(t, err, "a valid claim was refused: %v", err)
		if len(claimer.calls) != 1 {
			t.Fatalf("render=%v: %d engine claims, want 1", render, len(claimer.calls))
		}
		call := claimer.calls[0]
		if call.opts.RecoverySecret != " s " || call.opts.Owner != "w" || call.stepID != 1 {
			t.Errorf("render=%v: forwarded step=%d owner=%q secret=%q, want 1, %q, %q",
				render, call.stepID, call.opts.Owner, call.opts.RecoverySecret, "w", " s ")
		}
		if call.rendered != render {
			t.Errorf("render=%v: the engine claim rendered=%v", render, call.rendered)
		}
		if render && (call.executor != "hint" || res.Msg.GetPacket() != "packet") {
			t.Errorf("rendered claim: executor=%q packet=%q", call.executor, res.Msg.GetPacket())
		}
	}
}

func TestClaimRefusesAClientTemplatePath(t *testing.T) {
	claimer := &recordingClaimer{}
	client := claimClient(t, claimer)
	req := validClaim()
	req.Render = true
	req.Template = "/etc/passwd"

	_, err := client.StepClaim(context.Background(), connect.NewRequest(req))

	assertValidationRefusal(t, err)
	if len(claimer.calls) != 0 {
		t.Errorf("the engine claim ran %d time(s) for a client template path, want none", len(claimer.calls))
	}
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
			claimer := &recordingClaimer{}
			client := claimClient(t, claimer)
			req := validClaim()
			tc.edit(req)

			_, err := client.StepClaim(context.Background(), connect.NewRequest(req))

			assertValidationRefusal(t, err)
			if len(claimer.calls) != 0 {
				t.Errorf("the engine claim ran %d time(s), want none", len(claimer.calls))
			}
		})
	}
}

func TestClaimForwardsOptionalFieldsOnlyWhenPresent(t *testing.T) {
	claimer := &recordingClaimer{}
	client := claimClient(t, claimer)

	_, err := client.StepClaim(context.Background(), connect.NewRequest(validClaim()))
	testsupport.Must(t, err, "a claim without optional fields was refused: %v", err)
	req := validClaim()
	req.TtlMs = proto.Int64(60000)
	req.CostMultiplier = proto.Float64(2.5)
	_, err = client.StepClaim(context.Background(), connect.NewRequest(req))
	testsupport.Must(t, err, "a claim with ttl and multiplier was refused: %v", err)

	if len(claimer.calls) != 2 {
		t.Fatalf("%d engine claims, want 2", len(claimer.calls))
	}
	if got := claimer.calls[0].opts; got.TTLOverride != 0 || got.CostMultiplier != 0 {
		t.Errorf("absent fields forwarded ttl=%d multiplier=%g, want both unset", got.TTLOverride, got.CostMultiplier)
	}
	if got := claimer.calls[1].opts; got.TTLOverride != 60000 || got.CostMultiplier != 2.5 {
		t.Errorf("forwarded ttl=%d multiplier=%g, want 60000 and 2.5", got.TTLOverride, got.CostMultiplier)
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

func engineClaimClient(t *testing.T, conn *sql.DB) docketv1connect.DocketServiceClient {
	t.Helper()
	server := serve(t, NewClaimHandler(engine.NewEngine(), conn))
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

func TestClaimKeepsTheEngineClaimGuarantees(t *testing.T) {
	conn, first, second := activatedClaimDB(t)
	client := engineClaimClient(t, conn)

	res, err := client.StepClaim(context.Background(), claimRequest(first, "w1", "s1"))
	testsupport.Must(t, err, "claiming the ready step: %v", err)
	if res.Msg.GetAttempt() != 1 || countEvents(t, conn, engine.EventStepClaimed, first) != 1 {
		t.Errorf("a ready claim: attempt %d, %d step-claimed events; want 1 and 1",
			res.Msg.GetAttempt(), countEvents(t, conn, engine.EventStepClaimed, first))
	}

	_, err = client.StepClaim(context.Background(), claimRequest(first, "w2", "s2"))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrConflict {
		t.Errorf("a second owner's claim: code %s (err %v), want %s", got, err, output.ErrConflict)
	}
	if n := countEvents(t, conn, engine.EventClaimRefused, first); n != 1 {
		t.Errorf("%d claim-refused events after a second owner's claim, want 1", n)
	}

	_, err = client.StepClaim(context.Background(), claimRequest(second, "w1", "s1"))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrConflict {
		t.Errorf("claiming a step whose predecessor is open: code %s (err %v), want %s", got, err, output.ErrConflict)
	}
	if n := countEvents(t, conn, engine.EventStepClaimed, second); n != 0 {
		t.Errorf("%d step-claimed events on the unready step, want 0", n)
	}

	_, err = client.StepClaim(context.Background(), claimRequest(999, "w1", "s1"))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrNotFound {
		t.Errorf("claiming a missing step: code %s (err %v), want %s", got, err, output.ErrNotFound)
	}
}
