package verb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
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
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{})
		}, output.ErrValidation, "--run is required"},
		{"spawn with a missing run", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: "RUN-999"})
		}, output.ErrNotFound, "RUN-999 not found"},
		{"spawn acknowledging a non-seq", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: "RUN-1", AckReap: []int64{0}})
		}, output.ErrValidation, "--ack-reap 0 is not an event seq"},
		{"spawn with a malformed deciding vote", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Run: "RUN-1", DecidingVote: "nope"})
		}, output.ErrValidation, `invalid --deciding-vote "nope"`},
		{"spawn --active with a run", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Active: true, Run: "RUN-1"})
		}, output.ErrValidation, "--active and --run are mutually exclusive"},
		{"spawn --active with rows", func() (verb.GuardDecision, error) {
			return verb.GuardSpawn(ctx, env, verb.GuardSpawnInput{Active: true, Rows: []byte{}})
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
