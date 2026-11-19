package verb_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// Every guard is a read: an anchored identity no project holds answers over
// no runs and registers no project.
func TestGuardsNeverRegisterTheCallersProject(t *testing.T) {
	ctx := context.Background()
	const identity = "/work/unregistered"
	calls := map[string]func(verb.Env) (verb.GuardDecision, error){
		"stop": func(env verb.Env) (verb.GuardDecision, error) {
			return verb.GuardStop(ctx, env, verb.GuardStopInput{})
		},
		"gate": func(env verb.Env) (verb.GuardDecision, error) {
			return verb.GuardGate(ctx, env, verb.GuardGateInput{Step: "approve"})
		},
		"record": func(env verb.Env) (verb.GuardDecision, error) {
			return verb.GuardRecord(ctx, env, verb.GuardRecordInput{})
		},
		"spawn --active": func(env verb.Env) (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Active: true})
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			if _, err := call(projectEnv(st, identity, true)); err != nil {
				t.Fatalf("guard %s: %v", name, err)
			}
			requireUnregistered(t, st, identity)
		})
	}
}

func TestGuardStopAllowsWithNoActiveRun(t *testing.T) {
	st := openStore(t)
	got, err := verb.GuardStop(context.Background(), projectEnv(st, "", false), verb.GuardStopInput{})
	if err != nil {
		t.Fatalf("GuardStop: %v", err)
	}
	if !got.Allowed {
		t.Errorf("GuardStop = %+v, want allowed", got)
	}
}

func TestGuardGateDeniesAnAbsentGate(t *testing.T) {
	st := openStore(t)
	got, err := verb.GuardGate(context.Background(), projectEnv(st, "", false),
		verb.GuardGateInput{Step: "approve"})
	if err != nil {
		t.Fatalf("GuardGate: %v", err)
	}
	if got.Allowed || got.Reason == "" {
		t.Errorf("GuardGate = %+v, want a denial with a reason", got)
	}
}

func TestGuardRefusesMalformedInput(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)

	tests := []struct {
		name string
		call func() (verb.GuardDecision, error)
		code output.ErrorCode
		want string
	}{
		{"gate without a step", func() (verb.GuardDecision, error) {
			return verb.GuardGate(ctx, env, verb.GuardGateInput{})
		}, output.ErrValidation, "--step is required"},
		{"gate with a malformed run", func() (verb.GuardDecision, error) {
			return verb.GuardGate(ctx, env, verb.GuardGateInput{Step: "approve", Run: "nope"})
		}, output.ErrValidation, "invalid run ID"},
		{"record with a malformed run", func() (verb.GuardDecision, error) {
			return verb.GuardRecord(ctx, env, verb.GuardRecordInput{Run: "nope"})
		}, output.ErrValidation, "invalid run ID"},
		{"record with a missing run", func() (verb.GuardDecision, error) {
			return verb.GuardRecord(ctx, env, verb.GuardRecordInput{Run: "RUN-999"})
		}, output.ErrNotFound, "RUN-999 not found"},
		{"spawn without a run", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Rows: unreadableRows})
		}, output.ErrValidation, "--run is required"},
		{"spawn with a malformed run", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: "nope", Rows: unreadableRows})
		}, output.ErrValidation, "invalid run ID"},
		{"spawn with a missing run", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: "RUN-999"})
		}, output.ErrNotFound, "RUN-999 not found"},
		{"spawn acknowledging a non-seq", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{
				Run: "RUN-1", AckReap: []int64{0}, Rows: unreadableRows})
		}, output.ErrValidation, "--ack-reap 0 is not an event seq"},
		{"spawn with a malformed deciding vote", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: "RUN-1", DecidingVote: "nope"})
		}, output.ErrValidation, `invalid --deciding-vote "nope"`},
		{"spawn --active with a run", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Active: true, Run: "RUN-1"})
		}, output.ErrValidation, "--active and --run are mutually exclusive"},
		{"spawn --active with rows", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Active: true, Rows: unreadableRows})
		}, output.ErrValidation, "--active does not take --rows"},
		{"spawn --active with acknowledgments", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Active: true, AckReap: []int64{7}})
		}, output.ErrValidation, "--active does not take --ack-reap"},
		{"spawn --active with a malformed deciding vote", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Active: true, DecidingVote: "nope"})
		}, output.ErrValidation, `invalid --deciding-vote "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.call()
			requireCode(t, err, tt.code)
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

// errRowsUnreadable stands in for a transport's failed read of the proposed
// rows; unreadableRows is a Rows source that always fails with it.
var errRowsUnreadable = errors.New("rows unreadable")

func unreadableRows() ([]byte, error) { return nil, errRowsUnreadable }

// errLookupFailed is the store fault lookupFailingStore injects.
var errLookupFailed = errors.New("projects table unreadable")

// lookupFailingStore is a store whose project lookup fails, as a query
// against a store that opened but cannot be read does.
type lookupFailingStore struct{ store.Store }

func (lookupFailingStore) LookupProject(string) (int, bool, error) {
	return 0, false, errLookupFailed
}

// GuardSpawn resolves the project before it checks the request or reads the
// rows, so a store it cannot consult is the error every malformed request
// reports, and the rows source is never called.
func TestGuardSpawnResolvesTheProjectBeforeTheRequest(t *testing.T) {
	ctx := context.Background()
	env := projectEnv(lookupFailingStore{openStore(t)}, "/repos/runs", true)
	requests := map[string]verb.GuardSpawnInput{
		"no run":                   {},
		"a malformed run":          {Run: "nope"},
		"a non-seq acknowledgment": {Run: "RUN-1", AckReap: []int64{0}},
		"a well-formed run":        {Run: "RUN-1"},
		"--active":                 {Active: true, Run: "RUN-1"},
	}
	for name, in := range requests {
		t.Run(name, func(t *testing.T) {
			in.Rows = unreadableRows
			_, err := verb.GuardSpawn(ctx, env, in)
			if !errors.Is(err, errLookupFailed) {
				t.Errorf("err = %v, want the project lookup's failure", err)
			}
		})
	}
}

// Once the run and acknowledgments are well formed the rows are read, before
// the engine looks the run up and before the deciding vote is parsed, and a
// failed read comes back unchanged.
func TestGuardSpawnReadsRowsBeforeTheRunLookupAndTheDecidingVote(t *testing.T) {
	ctx := context.Background()
	env := projectEnv(openStore(t), "", false)
	requests := map[string]verb.GuardSpawnInput{
		"a run nobody started":      {Run: "RUN-999"},
		"a malformed deciding vote": {Run: "RUN-1", DecidingVote: "nope"},
	}
	for name, in := range requests {
		t.Run(name, func(t *testing.T) {
			in.Rows = unreadableRows
			_, err := verb.GuardSpawn(ctx, env, in)
			if !errors.Is(err, errRowsUnreadable) {
				t.Errorf("err = %v, want the rows source's failure", err)
			}
		})
	}
}

// guardWorkflow gives every guard something to answer over: "first" is a
// write-class executor step under a max-1 limit, so a reaped claim on it opens
// a reap hold, and "approve" is a human gate.
const guardWorkflow = `
[pipeline]
name = "unit-guard"
version = 1
[match]
kind = ["task"]
[limits]
write = { max = 1 }
[[step]]
name = "first"
after = []
executor = "someone"
class = "write"
emits = "result"
[[step]]
name = "approve"
after = []
type = "human"
on_fail = "skip"
`

// guardProjects binds the default project, where activatedStepOf puts its run,
// to one identity and registers a second project beside it. It returns an env
// for each.
func guardProjects(t *testing.T, st store.Store) (runs, other verb.Env) {
	t.Helper()
	// The first identity to touch the store claims the unclaimed default row,
	// so the run's identity goes first and the second one gets a new row.
	home, err := st.EnsureProject("/repos/runs", "runs", model.NowMS())
	if err != nil || home != store.DefaultProjectID {
		t.Fatalf("EnsureProject(runs) = %d, %v; want the default project %d", home, err, store.DefaultProjectID)
	}
	away, err := st.EnsureProject("/repos/other", "other", model.NowMS())
	if err != nil || away == store.DefaultProjectID {
		t.Fatalf("EnsureProject(other) = %d, %v; want a project of its own", away, err)
	}
	return projectEnv(st, "/repos/runs", true), projectEnv(st, "/repos/other", true)
}

// reapHeldRun activates a guardWorkflow run, claims its write-class step with
// a lease that has already lapsed, and lets `next` reap it, which opens a reap
// hold. It returns the run reference, the run's issue, and the lease-reaped
// event seq that acknowledges the hold.
func reapHeldRun(t *testing.T, st store.Store) (run string, issueID int, reapSeq int64) {
	t.Helper()
	stepID := activatedStepOf(t, st, guardWorkflow)
	step, err := st.GetStep(stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	now := model.NowMS()
	if _, err := engine.NewEngine().ClaimStepWithGatesStore(st, stepID, engine.ClaimOptions{
		Owner: "crashed-writer", NowMS: now - time.Hour.Milliseconds(),
	}); err != nil {
		t.Fatalf("ClaimStepWithGatesStore: %v", err)
	}
	if _, err := engine.NewEngine().NextStepsStore(st, step.RunID, 0, now); err != nil {
		t.Fatalf("NextStepsStore: %v", err)
	}
	page, err := engine.ListEventsStore(st, engine.EventQuery{RunID: step.RunID, Kind: engine.EventLeaseReaped})
	if err != nil {
		t.Fatalf("ListEventsStore: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("premise: %d lease-reaped events after the lease lapsed, want 1", len(page.Events))
	}
	return model.FormatRunID(step.RunID), step.IssueID, page.Events[0].Seq
}

func spawnAdmittedEvents(t *testing.T, st store.Store, run string) int {
	t.Helper()
	runID, err := model.ParseRunID(run)
	if err != nil {
		t.Fatalf("ParseRunID: %v", err)
	}
	page, err := engine.ListEventsStore(st, engine.EventQuery{RunID: runID, Kind: engine.EventSpawnAdmitted})
	if err != nil {
		t.Fatalf("ListEventsStore: %v", err)
	}
	return len(page.Events)
}

func requireAllowed(t *testing.T, got verb.GuardDecision, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if !got.Allowed {
		t.Errorf("%s denied (%s), want allowed", what, got.Reason)
	}
}

func requireDenied(t *testing.T, got verb.GuardDecision, err error, what, reason string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got.Allowed || !strings.Contains(got.Reason, reason) {
		t.Errorf("%s = %+v, want a denial naming %q", what, got, reason)
	}
}

// Each guard answers for the caller's project: another project's work does
// not move its verdict, and AllProjects asks the store-wide question.
func TestGuardsScopeToTheCallersProject(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	stepID := activatedStepOf(t, st, guardWorkflow)
	runs, other := guardProjects(t, st)
	step, err := st.GetStep(stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	openDispatch(t, runs, model.FormatRunID(step.RunID))

	const absentGate = "no `type=\"human\"`"
	guards := []struct {
		name  string
		call  func(env verb.Env, all bool) (verb.GuardDecision, error)
		held  string
		other string // the other project's denial, or "" for an allow
	}{
		{"stop", func(env verb.Env, all bool) (verb.GuardDecision, error) {
			return verb.GuardStop(ctx, env, verb.GuardStopInput{AllProjects: all})
		}, "first@0", ""},
		{"gate", func(env verb.Env, all bool) (verb.GuardDecision, error) {
			return verb.GuardGate(ctx, env, verb.GuardGateInput{Step: "approve", AllProjects: all})
		}, "not approved", absentGate},
		{"record", func(env verb.Env, all bool) (verb.GuardDecision, error) {
			return verb.GuardRecord(ctx, env, verb.GuardRecordInput{AllProjects: all})
		}, "dispatch", ""},
	}
	for _, g := range guards {
		t.Run(g.name, func(t *testing.T) {
			got, err := g.call(runs, false)
			requireDenied(t, got, err, "the run's own project", g.held)
			got, err = g.call(other, false)
			if g.other == "" {
				requireAllowed(t, got, err, "another project")
			} else {
				requireDenied(t, got, err, "another project", g.other)
			}
			got, err = g.call(other, true)
			requireDenied(t, got, err, "another project with AllProjects", g.held)
		})
	}
}

func TestGuardSpawnActiveScopesToTheCallersProject(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	reapHeldRun(t, st)
	runs, other := guardProjects(t, st)

	got, err := verb.GuardSpawn(ctx, runs, verb.GuardSpawnInput{Active: true})
	requireDenied(t, got, err, "the held run's project", "--ack-reap")
	got, err = verb.GuardSpawn(ctx, other, verb.GuardSpawnInput{Active: true})
	requireAllowed(t, got, err, "another project")
	got, err = verb.GuardSpawn(ctx, other, verb.GuardSpawnInput{Active: true, AllProjects: true})
	requireDenied(t, got, err, "another project with AllProjects", "--ack-reap")
}

// An acknowledgment is written before the predicate answers, and it persists:
// the next spawn without one is allowed too.
func TestGuardSpawnAcknowledgesTheReapItIsGiven(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	run, _, seq := reapHeldRun(t, st)
	env := projectEnv(st, "", false)

	got, err := verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: run})
	requireDenied(t, got, err, "a spawn under the hold", "--ack-reap")
	got, err = verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: run, AckReap: []int64{seq}})
	requireAllowed(t, got, err, "a spawn acknowledging the reap")
	got, err = verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: run})
	requireAllowed(t, got, err, "a later spawn after the acknowledgment")
}

// Each carve-out admits a spawn past the hold, audits it, and acknowledges
// nothing.
func TestGuardSpawnCarveOutsAdmitPastTheHold(t *testing.T) {
	ctx := context.Background()
	carveOuts := []struct {
		name string
		in   func(t *testing.T, st store.Store, run string, issueID int) verb.GuardSpawnInput
	}{
		{"deciding vote", func(t *testing.T, st store.Store, run string, _ int) verb.GuardSpawnInput {
			return verb.GuardSpawnInput{Run: run, DecidingVote: openProposal(t, st, 3, "")}
		}},
		{"usage join", func(_ *testing.T, _ store.Store, run string, _ int) verb.GuardSpawnInput {
			return verb.GuardSpawnInput{Run: run, UsageJoin: true}
		}},
		{"active deciding vote", func(t *testing.T, st store.Store, _ string, issueID int) verb.GuardSpawnInput {
			proposal := openProposal(t, st, 3, "")
			id, err := model.ParseProposalID(proposal)
			if err != nil {
				t.Fatalf("ParseProposalID: %v", err)
			}
			if err := st.LinkProposalIssue(id, issueID); err != nil {
				t.Fatalf("LinkProposalIssue: %v", err)
			}
			return verb.GuardSpawnInput{Active: true, DecidingVote: proposal}
		}},
		{"active usage join", func(*testing.T, store.Store, string, int) verb.GuardSpawnInput {
			return verb.GuardSpawnInput{Active: true, UsageJoin: true}
		}},
	}
	for _, c := range carveOuts {
		t.Run(c.name, func(t *testing.T) {
			st := openStore(t)
			run, issueID, _ := reapHeldRun(t, st)
			env := projectEnv(st, "", false)

			got, err := verb.GuardSpawn(ctx, env, c.in(t, st, run, issueID))
			requireAllowed(t, got, err, c.name)
			if n := spawnAdmittedEvents(t, st, run); n != 1 {
				t.Errorf("%d spawn-admitted events, want 1", n)
			}
			got, err = verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: run})
			requireDenied(t, got, err, "a plain spawn after the carve-out", "--ack-reap")
		})
	}
}

// Rows reach the row comparison: proposing a batch with no open dispatch
// denies, where proposing none passes the row half vacuously.
func TestGuardSpawnComparesTheProposedRows(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	run, _ := dispatchRun(t, st)

	got, err := verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: run})
	requireAllowed(t, got, err, "a spawn proposing no rows")
	got, err = verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: run, Rows: func() ([]byte, error) {
		return []byte("[]"), nil
	}})
	requireDenied(t, got, err, "rows with no open dispatch", "dispatch")
}
