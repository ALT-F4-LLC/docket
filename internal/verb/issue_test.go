package verb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func createIssue(t *testing.T, st store.Store) int {
	t.Helper()
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title:    "lease subject",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	return id
}

func requireCode(t *testing.T, err error, want output.ErrorCode) {
	t.Helper()
	var ve *verb.Error
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v (%T), want a *verb.Error coded %s", err, err, want)
	}
	if ve.Code != want {
		t.Fatalf("code = %s (%v), want %s", ve.Code, ve.Err, want)
	}
}

func claim(t *testing.T, env verb.Env, id int) verb.IssueClaimResult {
	t.Helper()
	res, err := verb.IssueClaim(context.Background(), env, verb.IssueClaimInput{
		Issue: model.FormatID(id), Owner: "worker-1",
	})
	if err != nil {
		t.Fatalf("IssueClaim: %v", err)
	}
	return res
}

func TestIssueLeaseVerbsRefuseEmptyToken(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := createIssue(t, st)
	claim(t, env, id)
	before, err := st.GetIssueLease(id)
	if err != nil {
		t.Fatalf("GetIssueLease: %v", err)
	}

	ctx := context.Background()
	ref := model.FormatID(id)
	_, releaseErr := verb.IssueRelease(ctx, env, verb.IssueReleaseInput{Issue: ref})
	_, heartbeatErr := verb.IssueHeartbeat(ctx, env, verb.IssueHeartbeatInput{Issue: ref})

	for name, err := range map[string]error{"release": releaseErr, "heartbeat": heartbeatErr} {
		t.Run(name, func(t *testing.T) {
			requireCode(t, err, output.ErrValidation)
		})
	}
	after, err := st.GetIssueLease(id)
	if err != nil {
		t.Fatalf("GetIssueLease: %v", err)
	}
	if *after != *before {
		t.Errorf("lease = %+v after empty-token calls, want unchanged %+v", *after, *before)
	}
}

func TestIssueLeaseVerbsCarryTheLeaseLifecycle(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := createIssue(t, st)
	ctx := context.Background()
	ref := model.FormatID(id)

	ttl := time.Hour
	claimed, err := verb.IssueClaim(ctx, env, verb.IssueClaimInput{Issue: ref, Owner: "worker-1", TTL: &ttl})
	if err != nil {
		t.Fatalf("IssueClaim: %v", err)
	}
	if claimed.Issue != id || claimed.Token == "" || claimed.Lease.Attempt != 1 || claimed.Version == 0 {
		t.Fatalf("claim = %+v, want issue %d, a token, attempt 1 and a version", claimed, id)
	}
	if got := claimed.Lease.ExpiresMS - model.NowMS(); got <= 0 || got > ttl.Milliseconds() {
		t.Errorf("lease expires in %dms, want within the explicit %s TTL", got, ttl)
	}

	_, err = verb.IssueClaim(ctx, env, verb.IssueClaimInput{Issue: ref, Owner: "worker-2"})
	requireCode(t, err, output.ErrConflict)

	_, err = verb.IssueHeartbeat(ctx, env, verb.IssueHeartbeatInput{Issue: ref, Token: "deadbeef"})
	requireCode(t, err, output.ErrAuth)

	beat, err := verb.IssueHeartbeat(ctx, env, verb.IssueHeartbeatInput{Issue: ref, Token: claimed.Token})
	if err != nil {
		t.Fatalf("IssueHeartbeat: %v", err)
	}
	if beat.Lease.Attempt != 1 {
		t.Errorf("heartbeat attempt = %d, want 1: a heartbeat is not a claim", beat.Lease.Attempt)
	}

	released, err := verb.IssueRelease(ctx, env, verb.IssueReleaseInput{Issue: ref, Token: claimed.Token})
	if err != nil {
		t.Fatalf("IssueRelease: %v", err)
	}
	if released.Lease.Attempt != 1 {
		t.Errorf("release attempt = %d, want 1: release keeps the count", released.Lease.Attempt)
	}

	_, err = verb.IssueRelease(ctx, env, verb.IssueReleaseInput{Issue: ref, Token: claimed.Token})
	requireCode(t, err, output.ErrAuth)
}

func TestIssueClaimValidatesBeforeTheStore(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := createIssue(t, st)
	ctx := context.Background()
	zero := time.Duration(0)

	for name, in := range map[string]verb.IssueClaimInput{
		"malformed ref": {Issue: "not-an-id", Owner: "worker-1"},
		"no owner":      {Issue: model.FormatID(id)},
		"zero ttl":      {Issue: model.FormatID(id), Owner: "worker-1", TTL: &zero},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verb.IssueClaim(ctx, env, in)
			requireCode(t, err, output.ErrValidation)
		})
	}
	lease, err := st.GetIssueLease(id)
	if err != nil {
		t.Fatalf("GetIssueLease: %v", err)
	}
	if lease.Owner != "" {
		t.Errorf("owner = %q after refused claims, want unclaimed", lease.Owner)
	}
}

func TestIssueClaimOnMissingIssueIsNotFound(t *testing.T) {
	st := openStore(t)
	_, err := verb.IssueClaim(context.Background(), projectEnv(st, "", false),
		verb.IssueClaimInput{Issue: "DKT-999", Owner: "worker-1"})
	requireCode(t, err, output.ErrNotFound)
}

func TestIssueClaimMayRegisterTheCallersProject(t *testing.T) {
	ctx := context.Background()

	t.Run("anchored registers and reports it", func(t *testing.T) {
		st := openStore(t)
		id := createIssue(t, st)
		res := claim(t, projectEnv(st, "/repos/fresh", true), id)
		reg := res.Registered
		if reg == nil {
			t.Fatal("Registered = nil, want the registration of /repos/fresh")
		}
		want, found, err := st.LookupProject("/repos/fresh")
		if err != nil || !found {
			t.Fatalf("LookupProject: found=%v err=%v, want the registered row", found, err)
		}
		if reg.ID != want || reg.Identity != "/repos/fresh" || reg.Name != "fresh" ||
			reg.Cwd != "/work/scratch" || reg.Verb != "issue claim" {
			t.Errorf("Registered = %+v, want project %d for /repos/fresh by issue claim", *reg, want)
		}
	})

	t.Run("known project reports no registration", func(t *testing.T) {
		st := openStore(t)
		if _, err := st.EnsureProject("/repos/known", "known", 1); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		res := claim(t, projectEnv(st, "/repos/known", true), createIssue(t, st))
		if res.Registered != nil {
			t.Errorf("Registered = %+v, want nil for an existing project", *res.Registered)
		}
	})

	t.Run("unanchored is refused before the lease", func(t *testing.T) {
		st := openStore(t)
		id := createIssue(t, st)
		_, err := verb.IssueClaim(ctx, projectEnv(st, "/tmp/scratch", false),
			verb.IssueClaimInput{Issue: model.FormatID(id), Owner: "worker-1"})
		requireCode(t, err, output.ErrValidation)
		requireUnregistered(t, st, "/tmp/scratch")
		lease, err := st.GetIssueLease(id)
		if err != nil {
			t.Fatalf("GetIssueLease: %v", err)
		}
		if lease.Owner != "" {
			t.Errorf("owner = %q, want the refused claim to take no lease", lease.Owner)
		}
	})
}
