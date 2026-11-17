package verb_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
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

func TestIssueLeaseVerbsReportTheRegistrationWhenTheyFailAfterRegistering(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	claim(t, projectEnv(st, "", false), id)
	ref := model.FormatID(id)
	ctx := context.Background()

	cases := []struct {
		identity string
		code     output.ErrorCode
		call     func(env verb.Env) (*engine.ProjectRegistration, error)
	}{
		{"/repos/claim", output.ErrNotFound, func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueClaim(ctx, env, verb.IssueClaimInput{Issue: "999", Owner: "worker-1"})
			return res.Registered, err
		}},
		{"/repos/release", output.ErrAuth, func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueRelease(ctx, env, verb.IssueReleaseInput{Issue: ref, Token: "deadbeef"})
			return res.Registered, err
		}},
		{"/repos/heartbeat", output.ErrAuth, func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueHeartbeat(ctx, env, verb.IssueHeartbeatInput{Issue: ref, Token: "deadbeef"})
			return res.Registered, err
		}},
	}
	for _, c := range cases {
		t.Run(c.identity, func(t *testing.T) {
			registered, err := c.call(projectEnv(st, c.identity, true))
			requireCode(t, err, c.code)
			if registered == nil || registered.Identity != c.identity {
				t.Errorf("Registered = %+v, want %s: the project stays registered after the refusal", registered, c.identity)
			}
		})
	}
}

func setConfig(t *testing.T, st store.Store, projectID int, key, value string) {
	t.Helper()
	if err := st.SetConfig(projectID, key, value); err != nil {
		t.Fatalf("SetConfig(%d, %s): %v", projectID, key, err)
	}
}

// requireExpiresAfter asserts lease expires ttl after a moment within
// [beforeMS, afterMS], the window around the call that set it.
func requireExpiresAfter(t *testing.T, lease model.Lease, ttl time.Duration, beforeMS, afterMS int64) {
	t.Helper()
	lo, hi := beforeMS+ttl.Milliseconds(), afterMS+ttl.Milliseconds()
	if lease.ExpiresMS < lo || lease.ExpiresMS > hi {
		t.Errorf("lease expires at %d, want %s after the call (%d..%d)", lease.ExpiresMS, ttl, lo, hi)
	}
}

func createInput(title string) verb.IssueCreateInput {
	return verb.IssueCreateInput{
		Title: title, Status: "backlog", Priority: "none", Kind: "task",
	}
}

func scopeOf(t *testing.T, st store.Store, id int) string {
	t.Helper()
	_, scope, err := st.IssueDeclaration(id)
	if err != nil {
		t.Fatalf("IssueDeclaration(%d): %v", id, err)
	}
	return scope
}

func issueCount(t *testing.T, st store.Store) int {
	t.Helper()
	n, err := st.CountIssues(store.DefaultProjectID)
	if err != nil {
		t.Fatalf("CountIssues: %v", err)
	}
	return n
}

func TestIssueCreateEchoesTheStoredIssue(t *testing.T) {
	st := openStore(t)
	in := createInput("created")
	in.Labels = []string{"routing"}
	in.Files = []string{"internal/verb/issue.go"}
	in.Scope = &[]string{"z/**", " a/** "}

	res, err := verb.IssueCreate(context.Background(), projectEnv(st, "", false), in)
	if err != nil {
		t.Fatalf("IssueCreate: %v", err)
	}
	got := res.Issue
	if got == nil || got.ID == 0 || got.Title != "created" || got.ProjectID != store.DefaultProjectID {
		t.Fatalf("Issue = %+v, want the created row in the default project", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "routing" {
		t.Errorf("Labels = %v, want the applied [routing]", got.Labels)
	}
	if len(got.Files) != 1 || got.Files[0] != "internal/verb/issue.go" {
		t.Errorf("Files = %v, want the applied file", got.Files)
	}
	if scope := scopeOf(t, st, got.ID); scope != `["z/**","a/**"]` {
		t.Errorf("scope_globs = %s, want the declared order, trimmed", scope)
	}
}

func TestIssueCreateValidatesBeforeWriting(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	parent := createIssue(t, st)
	before := issueCount(t, st)

	for name, mutate := range map[string]func(*verb.IssueCreateInput){
		"status":     func(in *verb.IssueCreateInput) { in.Status = "nope" },
		"priority":   func(in *verb.IssueCreateInput) { in.Priority = "nope" },
		"size":       func(in *verb.IssueCreateInput) { in.Size = "nope" },
		"kind":       func(in *verb.IssueCreateInput) { in.Kind = "nope" },
		"blank glob": func(in *verb.IssueCreateInput) { in.Scope = &[]string{" "} },
		"bad parent": func(in *verb.IssueCreateInput) { in.Parent = "not-an-id" },
	} {
		t.Run(name, func(t *testing.T) {
			in := createInput("refused")
			mutate(&in)
			_, err := verb.IssueCreate(context.Background(), env, in)
			requireCode(t, err, output.ErrValidation)
		})
	}
	if after := issueCount(t, st); after != before {
		t.Errorf("issues = %d after refused creates, want %d", after, before)
	}

	in := createInput("child")
	in.Parent = model.FormatID(parent)
	res, err := verb.IssueCreate(context.Background(), env, in)
	if err != nil {
		t.Fatalf("IssueCreate under a parent: %v", err)
	}
	if res.Issue.ParentID == nil || *res.Issue.ParentID != parent {
		t.Errorf("ParentID = %v, want %d", res.Issue.ParentID, parent)
	}
}

func TestIssueCreateRefusesAParentInAnotherProject(t *testing.T) {
	st := openStore(t)
	home := ensureProject(t, st, "/repos/home", "home")
	ensureProject(t, st, "/repos/other", "other")
	foreign, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: home, Title: "foreign parent", Status: model.StatusTodo,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	in := createInput("child")
	in.Parent = model.FormatID(foreign)

	_, err = verb.IssueCreate(context.Background(), projectEnv(st, "/repos/other", true), in)
	requireVerbCode(t, err, output.ErrValidation, "a sub-issue lives in its parent's project")
}

func TestIssueCreateMayRegisterTheCallersProject(t *testing.T) {
	t.Run("anchored registers and creates there", func(t *testing.T) {
		st := openStore(t)
		res, err := verb.IssueCreate(context.Background(), projectEnv(st, "/repos/fresh", true), createInput("first"))
		if err != nil {
			t.Fatalf("IssueCreate: %v", err)
		}
		want, found, err := st.LookupProject("/repos/fresh")
		if err != nil || !found {
			t.Fatalf("LookupProject: found=%v err=%v, want the registered row", found, err)
		}
		if res.Registered == nil || res.Registered.ID != want || res.Registered.Verb != "issue create" {
			t.Errorf("Registered = %+v, want project %d registered by issue create", res.Registered, want)
		}
		if res.Issue.ProjectID != want {
			t.Errorf("ProjectID = %d, want the caller's project %d", res.Issue.ProjectID, want)
		}
	})

	t.Run("unanchored is refused before the insert", func(t *testing.T) {
		st := openStore(t)
		_, err := verb.IssueCreate(context.Background(), projectEnv(st, "/tmp/scratch", false), createInput("stray"))
		requireCode(t, err, output.ErrValidation)
		requireUnregistered(t, st, "/tmp/scratch")
		if n := issueCount(t, st); n != 0 {
			t.Errorf("issues = %d, want the refused create to write none", n)
		}
	})
}

func TestIssueCreateReplayedKeyWritesNoScope(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	first := createInput("original")
	first.IdempotencyKey = "K"
	created, err := verb.IssueCreate(context.Background(), env, first)
	if err != nil {
		t.Fatalf("IssueCreate: %v", err)
	}

	replay := first
	replay.Scope = &[]string{"a/**"}
	res, err := verb.IssueCreate(context.Background(), env, replay)
	if err != nil {
		t.Fatalf("replayed IssueCreate: %v", err)
	}
	if res.Issue.ID != created.Issue.ID {
		t.Errorf("replay returned issue %d, want the original %d", res.Issue.ID, created.Issue.ID)
	}
	if scope := scopeOf(t, st, created.Issue.ID); scope != "" {
		t.Errorf("scope_globs = %s after a replay, want the original NULL", scope)
	}
}

func TestIssueEditAppliesFieldsAsTheAuthor(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	env.Author = "editor-1"
	id := createIssue(t, st)
	title, status := "renamed", "in-progress"
	files := []string{"a.go"}

	res, err := verb.IssueEdit(context.Background(), env, verb.IssueEditInput{
		Issue: model.FormatID(id), Title: &title, Status: &status, Files: &files,
		Scope: &[]string{"a/**"},
	})
	if err != nil {
		t.Fatalf("IssueEdit: %v", err)
	}
	if !res.Changed || res.Issue.Title != title || res.Issue.Status != model.StatusInProgress {
		t.Fatalf("result = %+v, want the changed issue", res)
	}
	if len(res.Issue.Files) != 1 || res.Issue.Files[0] != "a.go" {
		t.Errorf("Files = %v, want [a.go]", res.Issue.Files)
	}
	if scope := scopeOf(t, st, id); scope != `["a/**"]` {
		t.Errorf("scope_globs = %s, want [\"a/**\"]", scope)
	}
	activity, err := st.GetActivity(id, 0)
	if err != nil {
		t.Fatalf("GetActivity: %v", err)
	}
	attributed := false
	for _, a := range activity {
		if a.FieldChanged == "title" {
			attributed = a.ChangedBy == "editor-1"
		}
	}
	if !attributed {
		t.Errorf("activity %+v has no title change attributed to env.Author", activity)
	}
}

func TestIssueEditWithNoChangesWritesNothing(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	before, err := st.GetVersion("issues", id)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}

	res, err := verb.IssueEdit(context.Background(), projectEnv(st, "", false),
		verb.IssueEditInput{Issue: model.FormatID(id)})
	if err != nil {
		t.Fatalf("IssueEdit: %v", err)
	}
	if res.Changed || res.Issue == nil || res.Issue.ID != id {
		t.Errorf("result = %+v, want the unchanged issue", res)
	}
	if after, _ := st.GetVersion("issues", id); after != before {
		t.Errorf("version %d -> %d, want an empty edit to write nothing", before, after)
	}
}

func TestIssueEditRefusals(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	parent := createIssue(t, st)
	child := createIssue(t, st)
	parentRef, childRef := model.FormatID(parent), model.FormatID(child)
	if _, err := verb.IssueEdit(context.Background(), env,
		verb.IssueEditInput{Issue: childRef, Parent: &parentRef}); err != nil {
		t.Fatalf("reparenting: %v", err)
	}
	stale := 1
	bad := "nope"
	files := []string{"never.go"}

	cases := []struct {
		name string
		in   verb.IssueEditInput
		code output.ErrorCode
		text string
	}{
		{"unknown issue", verb.IssueEditInput{Issue: "DKT-999", Title: &bad}, output.ErrNotFound, "issue DKT-999 not found"},
		{"bad status", verb.IssueEditInput{Issue: childRef, Status: &bad}, output.ErrValidation, ""},
		{"self parent", verb.IssueEditInput{Issue: childRef, Parent: &childRef}, output.ErrValidation, "cannot set parent to self"},
		{"cycle", verb.IssueEditInput{Issue: parentRef, Parent: &childRef}, output.ErrConflict, "would create a cycle"},
		{"blank glob", verb.IssueEditInput{Issue: childRef, Files: &files, Scope: &[]string{""}}, output.ErrValidation, "--scope glob must not be empty"},
		{"stale version", verb.IssueEditInput{Issue: childRef, Title: &bad, IfVersion: &stale}, output.ErrConflict, "version mismatch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := verb.IssueEdit(context.Background(), env, c.in)
			requireVerbCode(t, err, c.code, c.text)
		})
	}

	got, err := st.GetIssueFiles(child)
	if err != nil {
		t.Fatalf("GetIssueFiles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("files = %v, want the refused edit to write none", got)
	}
}

func TestIssueEditMayRegisterTheCallersProject(t *testing.T) {
	t.Run("anchored registers and reports it", func(t *testing.T) {
		st := openStore(t)
		id := createIssue(t, st)
		res, err := verb.IssueEdit(context.Background(), projectEnv(st, "/repos/fresh", true),
			verb.IssueEditInput{Issue: model.FormatID(id)})
		if err != nil {
			t.Fatalf("IssueEdit: %v", err)
		}
		if res.Registered == nil || res.Registered.Identity != "/repos/fresh" || res.Registered.Verb != "issue edit" {
			t.Errorf("Registered = %+v, want /repos/fresh registered by issue edit", res.Registered)
		}
	})

	t.Run("unanchored is refused before the edit", func(t *testing.T) {
		st := openStore(t)
		id := createIssue(t, st)
		title := "stray"
		_, err := verb.IssueEdit(context.Background(), projectEnv(st, "/tmp/scratch", false),
			verb.IssueEditInput{Issue: model.FormatID(id), Title: &title})
		requireCode(t, err, output.ErrValidation)
		requireUnregistered(t, st, "/tmp/scratch")
		issue, err := st.GetIssue(id)
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		if issue.Title == title {
			t.Error("the refused edit renamed the issue")
		}
	})
}

func TestScopeGlobsJSON(t *testing.T) {
	cases := []struct {
		globs []string
		want  string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"z/**", "a/**"}, `["z/**","a/**"]`},
	}
	for _, c := range cases {
		got, err := verb.ScopeGlobsJSON(c.globs)
		if err != nil || got != c.want {
			t.Errorf("ScopeGlobsJSON(%q) = %q, %v; want %q", c.globs, got, err, c.want)
		}
	}
	_, err := verb.ScopeGlobsJSON([]string{"a/**", " "})
	requireCode(t, err, output.ErrValidation)
}

func TestIssueLeaseVerbsTakeTheConfiguredTTLOfTheCallersProject(t *testing.T) {
	st := openStore(t)
	// The first registration takes over the default project's row; the
	// caller's project must be another one for its config to be told apart.
	ensureProject(t, st, "/repos/first", "first")
	own := ensureProject(t, st, "/repos/ttl", "ttl")
	if own == store.DefaultProjectID {
		t.Fatalf("caller project = %d, want one other than the default", own)
	}
	setConfig(t, st, store.DefaultProjectID, "lease.ttl.default", "1m")
	setConfig(t, st, store.DefaultProjectID, "lease.ttl.slow", "2m")
	setConfig(t, st, own, "lease.ttl.default", "3h")
	setConfig(t, st, own, "lease.ttl.slow", "5h")

	env := projectEnv(st, "/repos/ttl", true)
	ctx := context.Background()
	ref := model.FormatID(createIssue(t, st))

	before := model.NowMS()
	claimed, err := verb.IssueClaim(ctx, env, verb.IssueClaimInput{Issue: ref, Owner: "worker-1"})
	if err != nil {
		t.Fatalf("IssueClaim: %v", err)
	}
	requireExpiresAfter(t, claimed.Lease, 3*time.Hour, before, model.NowMS())

	before = model.NowMS()
	beat, err := verb.IssueHeartbeat(ctx, env, verb.IssueHeartbeatInput{
		Issue: ref, Token: claimed.Token, Class: "slow",
	})
	if err != nil {
		t.Fatalf("IssueHeartbeat: %v", err)
	}
	requireExpiresAfter(t, beat.Lease, 5*time.Hour, before, model.NowMS())
}

func TestIssueReleaseAndHeartbeatRefuseAnUnanchoredCaller(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	claimed := claim(t, projectEnv(st, "", false), id)
	before, err := st.GetIssueLease(id)
	if err != nil {
		t.Fatalf("GetIssueLease: %v", err)
	}

	ctx := context.Background()
	env := projectEnv(st, "/tmp/scratch", false)
	ref := model.FormatID(id)
	_, releaseErr := verb.IssueRelease(ctx, env, verb.IssueReleaseInput{Issue: ref, Token: claimed.Token})
	_, heartbeatErr := verb.IssueHeartbeat(ctx, env, verb.IssueHeartbeatInput{Issue: ref, Token: claimed.Token})

	for name, err := range map[string]error{"release": releaseErr, "heartbeat": heartbeatErr} {
		t.Run(name, func(t *testing.T) {
			requireVerbError(t, err, output.ErrValidation, "cannot tell which project this belongs to")
		})
	}
	requireUnregistered(t, st, "/tmp/scratch")
	after, err := st.GetIssueLease(id)
	if err != nil {
		t.Fatalf("GetIssueLease: %v", err)
	}
	if *after != *before {
		t.Errorf("lease = %+v after refused calls, want unchanged %+v", *after, *before)
	}
}

func TestIssueHeartbeatOnAnExpiredLeaseIsStale(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	// A 1ms lease taken at the epoch is long expired, with no sleep.
	token, _, err := st.ClaimIssue(id, "worker-1", 1, 1)
	if err != nil {
		t.Fatalf("ClaimIssue: %v", err)
	}

	_, err = verb.IssueHeartbeat(context.Background(), projectEnv(st, "", false),
		verb.IssueHeartbeatInput{Issue: model.FormatID(id), Token: token})
	requireCode(t, err, output.ErrStaleLease)
}

func TestIssueLeaseRefusalsNameTheIssueByItsOwningProject(t *testing.T) {
	st := openStore(t)
	prefixes := map[string]string{"/repos/owner": "OWN", "/repos/caller": "CALL"}
	projects := map[string]int{}
	for identity, prefix := range prefixes {
		projects[identity] = ensureProject(t, st, identity, prefix)
		if err := st.SetProjectPrefix(projects[identity], prefix); err != nil {
			t.Fatalf("SetProjectPrefix(%s): %v", identity, err)
		}
	}
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: projects["/repos/owner"],
		Title:     "lease subject",
		Status:    model.StatusTodo,
		Priority:  model.PriorityMedium,
		Kind:      model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	claim(t, projectEnv(st, "/repos/owner", true), id)

	ctx := context.Background()
	env := projectEnv(st, "/repos/caller", true)

	t.Run("an owned issue takes its owner's prefix", func(t *testing.T) {
		_, err := verb.IssueHeartbeat(ctx, env, verb.IssueHeartbeatInput{
			Issue: model.FormatID(id), Token: "deadbeef",
		})
		requireVerbError(t, err, output.ErrAuth, fmt.Sprintf("issue OWN-%d:", id))
	})

	t.Run("an unowned id takes the caller's prefix", func(t *testing.T) {
		_, err := verb.IssueClaim(ctx, env, verb.IssueClaimInput{Issue: "999", Owner: "worker-1"})
		requireVerbError(t, err, output.ErrNotFound, "issue CALL-999 not found")
	})
}

func issueStatus(t *testing.T, st store.Store, id int) model.Status {
	t.Helper()
	issue, err := st.GetIssue(id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	return issue.Status
}

func issueVersion(t *testing.T, st store.Store, id int) int {
	t.Helper()
	version, err := st.GetVersion("issues", id)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	return version
}

// TestIssueCloseReadsTheTokenOnlyUnderALiveLease pins the DKT-124 fix:
// closing an issue no one holds must not read the token at all, since the
// CLI's stdin fallback drains its reader to EOF and would hang under a parent
// that keeps the pipe open. Under a live lease the token is still required.
func TestIssueCloseReadsTheTokenOnlyUnderALiveLease(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	tripwire := func() string {
		t.Error("the token was read; this path must not read it")
		return ""
	}

	t.Run("unclaimed", func(t *testing.T) {
		id := createIssue(t, st)
		res, err := verb.IssueClose(ctx, env, verb.IssueCloseInput{Issue: model.FormatID(id), Token: tripwire})
		if err != nil {
			t.Fatalf("IssueClose: %v", err)
		}
		if !res.Changed || res.Issue.Status != model.StatusDone {
			t.Errorf("result = %+v, want the closed issue", res)
		}
	})

	t.Run("lapsed lease", func(t *testing.T) {
		id := createIssue(t, st)
		if _, _, err := st.ClaimIssue(id, "owner", -1, model.NowMS()); err != nil {
			t.Fatalf("ClaimIssue: %v", err)
		}
		if _, err := verb.IssueClose(ctx, env, verb.IssueCloseInput{Issue: model.FormatID(id), Token: tripwire}); err != nil {
			t.Fatalf("IssueClose: %v", err)
		}
		if got := issueStatus(t, st, id); got != model.StatusDone {
			t.Errorf("status = %q, want done", got)
		}
	})

	t.Run("live lease without the token", func(t *testing.T) {
		id := createIssue(t, st)
		if _, _, err := st.ClaimIssue(id, "owner", 60_000, model.NowMS()); err != nil {
			t.Fatalf("ClaimIssue: %v", err)
		}
		_, err := verb.IssueClose(ctx, env, verb.IssueCloseInput{Issue: model.FormatID(id)})
		requireCode(t, err, output.ErrAuth)
		if got := issueStatus(t, st, id); got == model.StatusDone {
			t.Error("a close without the token closed a claimed issue")
		}
	})

	t.Run("live lease with the token", func(t *testing.T) {
		id := createIssue(t, st)
		token, _, err := st.ClaimIssue(id, "owner", 60_000, model.NowMS())
		if err != nil {
			t.Fatalf("ClaimIssue: %v", err)
		}
		read := 0
		_, err = verb.IssueClose(ctx, env, verb.IssueCloseInput{
			Issue: model.FormatID(id), Token: func() string { read++; return token },
		})
		if err != nil {
			t.Fatalf("IssueClose: %v", err)
		}
		if got := issueStatus(t, st, id); read != 1 || got != model.StatusDone {
			t.Errorf("token read %d times, status %q; want one read and done", read, got)
		}
	})
}

func TestIssueCloseOfADoneIssueWritesNothing(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := createIssue(t, st)
	in := verb.IssueCloseInput{Issue: model.FormatID(id)}
	if _, err := verb.IssueClose(ctx, env, in); err != nil {
		t.Fatalf("IssueClose: %v", err)
	}
	before := issueVersion(t, st, id)

	// An unsatisfiable precondition is not checked on a close that writes
	// nothing.
	zero := 0
	in.IfVersion = &zero
	res, err := verb.IssueClose(ctx, env, in)
	if err != nil {
		t.Fatalf("second IssueClose: %v", err)
	}
	if res.Changed || res.Issue == nil || res.Issue.ID != id {
		t.Errorf("result = %+v, want the unchanged issue", res)
	}
	if after := issueVersion(t, st, id); after != before {
		t.Errorf("version %d -> %d, want a close of a done issue to write nothing", before, after)
	}
}

// requireChangedBy asserts the activity log attributes a change of issue id's
// field to value to author.
func requireChangedBy(t *testing.T, st store.Store, id int, field, value, author string) {
	t.Helper()
	activity, err := st.GetActivity(id, 0)
	if err != nil {
		t.Fatalf("GetActivity: %v", err)
	}
	for _, a := range activity {
		if a.FieldChanged == field && a.NewValue == value && a.ChangedBy == author {
			return
		}
	}
	t.Errorf("activity %+v has no change of %s to %q attributed to %q", activity, field, value, author)
}

func TestIssueCloseAttributesTheCloseToTheAuthor(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	env.Author = "closer-1"
	id := createIssue(t, st)

	if _, err := verb.IssueClose(context.Background(), env, verb.IssueCloseInput{Issue: model.FormatID(id)}); err != nil {
		t.Fatalf("IssueClose: %v", err)
	}
	requireChangedBy(t, st, id, "status", string(model.StatusDone), "closer-1")
}

func TestIssueCloseAndReopenRefusals(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	open := createIssue(t, st)
	closed := createIssue(t, st)
	if _, err := verb.IssueClose(ctx, env, verb.IssueCloseInput{Issue: model.FormatID(closed)}); err != nil {
		t.Fatalf("IssueClose: %v", err)
	}
	stale, zero := 99, 0

	cases := []struct {
		name string
		call func() error
		code output.ErrorCode
		text string
	}{
		{"close unknown issue", func() error {
			_, err := verb.IssueClose(ctx, env, verb.IssueCloseInput{Issue: "DKT-999"})
			return err
		}, output.ErrNotFound, "issue DKT-999 not found"},
		{"close stale version", func() error {
			_, err := verb.IssueClose(ctx, env, verb.IssueCloseInput{Issue: model.FormatID(open), IfVersion: &stale})
			return err
		}, output.ErrConflict, "version mismatch"},
		{"close zero version", func() error {
			_, err := verb.IssueClose(ctx, env, verb.IssueCloseInput{Issue: model.FormatID(open), IfVersion: &zero})
			return err
		}, output.ErrValidation, "--if-version must be >= 1"},
		{"reopen unknown issue", func() error {
			_, err := verb.IssueReopen(ctx, env, verb.IssueReopenInput{Issue: "DKT-999"})
			return err
		}, output.ErrNotFound, "issue DKT-999 not found"},
		{"reopen stale version", func() error {
			_, err := verb.IssueReopen(ctx, env, verb.IssueReopenInput{Issue: model.FormatID(closed), IfVersion: &stale})
			return err
		}, output.ErrConflict, "version mismatch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireVerbCode(t, c.call(), c.code, c.text)
		})
	}
	if got := issueStatus(t, st, open); got != model.StatusTodo {
		t.Errorf("open issue status = %q, want the refused closes to leave todo", got)
	}
	if got := issueStatus(t, st, closed); got != model.StatusDone {
		t.Errorf("closed issue status = %q, want the refused reopen to leave done", got)
	}
}

func TestIssueReopenReturnsAClosedIssueToBacklog(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	env.Author = "reopener-1"
	id := createIssue(t, st)
	ref := model.FormatID(id)

	res, err := verb.IssueReopen(ctx, env, verb.IssueReopenInput{Issue: ref})
	if err != nil {
		t.Fatalf("IssueReopen of an open issue: %v", err)
	}
	if res.Changed || res.Issue.Status != model.StatusTodo {
		t.Errorf("result = %+v, want the open issue unchanged", res)
	}

	if _, err := verb.IssueClose(ctx, env, verb.IssueCloseInput{Issue: ref}); err != nil {
		t.Fatalf("IssueClose: %v", err)
	}
	res, err = verb.IssueReopen(ctx, env, verb.IssueReopenInput{Issue: ref})
	if err != nil {
		t.Fatalf("IssueReopen: %v", err)
	}
	if !res.Changed || res.Issue.Status != model.StatusBacklog {
		t.Errorf("result = %+v, want the issue back in backlog", res)
	}
	requireChangedBy(t, st, id, "status", string(model.StatusBacklog), "reopener-1")
}

// givenStatus is a status the caller supplied, empty included.
func givenStatus(s string) *string { return &s }

func TestIssueMoveChangesTheStatus(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := createIssue(t, st)

	res, err := verb.IssueMove(ctx, env, verb.IssueMoveInput{Issue: model.FormatID(id), Status: givenStatus("review")})
	if err != nil {
		t.Fatalf("IssueMove: %v", err)
	}
	if !res.Changed || res.From != model.StatusTodo || res.Issue.Status != model.StatusReview || res.Migration != nil {
		t.Errorf("result = %+v, want a todo -> review move", res)
	}
}

func TestIssueMoveToTheSameStatusRecordsTheNoteOnly(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	env.Author = "mover-1"
	id := createIssue(t, st)

	res, err := verb.IssueMove(ctx, env, verb.IssueMoveInput{
		Issue: model.FormatID(id), Status: givenStatus(string(model.StatusTodo)), Note: "still todo",
	})
	if err != nil {
		t.Fatalf("IssueMove: %v", err)
	}
	if res.Changed || res.Issue.Status != model.StatusTodo {
		t.Errorf("result = %+v, want the unchanged issue", res)
	}
	comments, err := st.ListComments(id)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want the one note", len(comments))
	}
	if c := comments[0]; c.Body != "still todo" || c.Author != "mover-1" {
		t.Errorf("note = %q by %q, want \"still todo\" by mover-1", c.Body, c.Author)
	}
}

// TestIssueMoveToTheSameStatusHonorsTheVersion: a no-op move with a stale
// IfVersion is a conflict, so the caller does not conclude its precondition
// held, and its note is not recorded.
func TestIssueMoveToTheSameStatusHonorsTheVersion(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := createIssue(t, st)
	stale := issueVersion(t, st, id) + 1

	for _, note := range []string{"", "still todo"} {
		t.Run(fmt.Sprintf("note %q", note), func(t *testing.T) {
			_, err := verb.IssueMove(ctx, env, verb.IssueMoveInput{
				Issue: model.FormatID(id), Status: givenStatus(string(model.StatusTodo)), IfVersion: &stale, Note: note,
			})
			requireVerbCode(t, err, output.ErrConflict, "version mismatch")
		})
	}
	comments, err := st.ListComments(id)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 0 {
		t.Errorf("the refused no-op moves recorded %d comment(s)", len(comments))
	}
}

func TestIssueMoveRefusesToMigrateARunHeldIssue(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := createIssue(t, st)
	if _, _, err := st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{}, "", id); err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	target := callerProject(t, st)

	_, err := verb.IssueMove(ctx, env, verb.IssueMoveInput{Issue: model.FormatID(id), Project: "/repos/caller"})
	requireVerbCode(t, err, output.ErrConflict, "belongs to a run")
	issue, err := st.GetIssue(id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.ProjectID == target {
		t.Errorf("issue migrated to project %d, want the run-held issue left in place", target)
	}
}

func TestIssueMoveRefusals(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := createIssue(t, st)
	ref := model.FormatID(id)
	stale, zero := 99, 0

	cases := []struct {
		name string
		in   verb.IssueMoveInput
		code output.ErrorCode
		text string
	}{
		{"status and project", verb.IssueMoveInput{Issue: ref, Status: givenStatus("done"), Project: "elsewhere"}, output.ErrValidation, "separate operations"},
		{"if-version on migration", verb.IssueMoveInput{Issue: ref, Project: "elsewhere", IfVersion: &stale}, output.ErrValidation, "--if-version applies to status moves only"},
		{"note on migration", verb.IssueMoveInput{Issue: ref, Project: "elsewhere", Note: "x"}, output.ErrValidation, "--note applies to status moves only"},
		{"empty status and project", verb.IssueMoveInput{Issue: ref, Status: givenStatus(""), Project: "elsewhere"}, output.ErrValidation, "separate operations"},
		{"neither", verb.IssueMoveInput{Issue: ref}, output.ErrValidation, "usage: docket issue move"},
		{"bad status", verb.IssueMoveInput{Issue: ref, Status: givenStatus("nope")}, output.ErrValidation, `invalid status "nope"`},
		{"empty status", verb.IssueMoveInput{Issue: ref, Status: givenStatus("")}, output.ErrValidation, `invalid status ""`},
		{"unknown issue", verb.IssueMoveInput{Issue: "DKT-999", Status: givenStatus("done")}, output.ErrNotFound, "issue DKT-999 not found"},
		{"stale version", verb.IssueMoveInput{Issue: ref, Status: givenStatus("done"), IfVersion: &stale}, output.ErrConflict, "version mismatch"},
		{"zero version on a no-op", verb.IssueMoveInput{Issue: ref, Status: givenStatus("todo"), IfVersion: &zero}, output.ErrValidation, "--if-version must be >= 1"},
		{"unknown project", verb.IssueMoveInput{Issue: ref, Project: "elsewhere"}, output.ErrNotFound, ""},
		{"unknown issue on migration", verb.IssueMoveInput{Issue: "DKT-999", Project: "1"}, output.ErrNotFound, "issue DKT-999 not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := verb.IssueMove(ctx, env, c.in)
			requireVerbCode(t, err, c.code, c.text)
		})
	}
	if got := issueStatus(t, st, id); got != model.StatusTodo {
		t.Errorf("status = %q, want the refused moves to leave todo", got)
	}
	comments, err := st.ListComments(id)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 0 {
		t.Errorf("the refused moves recorded %d comment(s)", len(comments))
	}
}

func TestIssueMoveMigratesARootWithItsSubtree(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	env := projectEnv(st, "", false)
	root := createIssue(t, st)
	child := createIssue(t, st)
	rootRef, childRef := model.FormatID(root), model.FormatID(child)
	if _, err := verb.IssueEdit(ctx, env, verb.IssueEditInput{Issue: childRef, Parent: &rootRef}); err != nil {
		t.Fatalf("reparenting: %v", err)
	}
	target := callerProject(t, st)
	env.Author = "migrator-1"

	_, err := verb.IssueMove(ctx, env, verb.IssueMoveInput{Issue: childRef, Project: "/repos/caller"})
	requireVerbCode(t, err, output.ErrValidation, "has a parent")

	res, err := verb.IssueMove(ctx, env, verb.IssueMoveInput{Issue: rootRef, Project: "/repos/caller"})
	if err != nil {
		t.Fatalf("IssueMove --project: %v", err)
	}
	m := res.Migration
	if m == nil || m.Root != root || m.Project.ID != target || len(m.Migrated) != 2 || res.Issue != nil {
		t.Fatalf("result = %+v, want root %d and its child migrated to project %d", res, root, target)
	}
	for _, id := range []int{root, child} {
		issue, err := st.GetIssue(id)
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		if issue.ProjectID != target {
			t.Errorf("issue %d project = %d, want %d", id, issue.ProjectID, target)
		}
		requireChangedBy(t, st, id, "project", "/repos/caller", "migrator-1")
	}
}

// TestIssueStatusVerbsMayRegisterTheCallersProject: close, reopen and move
// write into the caller's project, so an anchored identity is registered and
// reported, and an unanchored one is refused before the write.
func TestIssueStatusVerbsMayRegisterTheCallersProject(t *testing.T) {
	ctx := context.Background()
	verbs := []struct {
		path string
		from model.Status
		call func(env verb.Env, ref string) (*engine.ProjectRegistration, error)
	}{
		{"issue close", model.StatusTodo, func(env verb.Env, ref string) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueClose(ctx, env, verb.IssueCloseInput{Issue: ref})
			return res.Registered, err
		}},
		{"issue reopen", model.StatusDone, func(env verb.Env, ref string) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueReopen(ctx, env, verb.IssueReopenInput{Issue: ref})
			return res.Registered, err
		}},
		{"issue move", model.StatusTodo, func(env verb.Env, ref string) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueMove(ctx, env, verb.IssueMoveInput{Issue: ref, Status: givenStatus("review")})
			return res.Registered, err
		}},
	}
	for _, v := range verbs {
		t.Run(v.path, func(t *testing.T) {
			st := openStore(t)
			id := createIssue(t, st)
			ref := model.FormatID(id)
			if v.from == model.StatusDone {
				if _, err := verb.IssueClose(ctx, projectEnv(st, "", false), verb.IssueCloseInput{Issue: ref}); err != nil {
					t.Fatalf("IssueClose: %v", err)
				}
			}

			_, err := v.call(projectEnv(st, "/tmp/scratch", false), ref)
			requireCode(t, err, output.ErrValidation)
			requireUnregistered(t, st, "/tmp/scratch")
			if got := issueStatus(t, st, id); got != v.from {
				t.Errorf("status = %q, want the unanchored call refused before the write", got)
			}

			registered, err := v.call(projectEnv(st, "/repos/fresh", true), ref)
			if err != nil {
				t.Fatalf("%s: %v", v.path, err)
			}
			if registered == nil || registered.Identity != "/repos/fresh" || registered.Verb != v.path {
				t.Errorf("Registered = %+v, want /repos/fresh registered by %s", registered, v.path)
			}
		})
	}
}

// createChild writes a sub-issue of parent, in parent's project.
func createChild(t *testing.T, st store.Store, parent int, title string, status model.Status) int {
	t.Helper()
	p, err := st.GetIssue(parent)
	if err != nil {
		t.Fatalf("GetIssue(%d): %v", parent, err)
	}
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: p.ProjectID,
		ParentID:  &parent,
		Title:     title,
		Status:    status,
		Priority:  model.PriorityMedium,
		Kind:      model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent(%q): %v", title, err)
	}
	return id
}

func requireIssueGone(t *testing.T, st store.Store, id int) {
	t.Helper()
	if _, err := st.GetIssue(id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetIssue(%d) error = %v, want ErrNotFound: the issue should be deleted", id, err)
	}
}

func TestIssueDeleteDeletesAnIssueWithoutSubIssues(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)

	res, err := verb.IssueDelete(context.Background(), projectEnv(st, "", false),
		verb.IssueDeleteInput{Issue: model.FormatID(id)})
	if err != nil {
		t.Fatalf("IssueDelete: %v", err)
	}
	if res.Issue != model.FormatID(id) || res.Title != "lease subject" || res.SubIssues != 0 {
		t.Errorf("result = %+v, want %s titled \"lease subject\" with no sub-issues", res, model.FormatID(id))
	}
	requireIssueGone(t, st, id)
}

// An issue with sub-issues is never deleted on a guess: the caller must choose
// cascade or orphan. The refusal error carries the count its prompt needs, and
// the result reports nothing deleted.
func TestIssueDeleteRefusesSubIssuesWithoutAChoice(t *testing.T) {
	st := openStore(t)
	parent := createIssue(t, st)
	child := createChild(t, st, parent, "child", model.StatusTodo)

	res, err := verb.IssueDelete(context.Background(), projectEnv(st, "", false),
		verb.IssueDeleteInput{Issue: model.FormatID(parent)})
	requireVerbError(t, err, output.ErrValidation,
		fmt.Sprintf("issue %s has 1 sub-issue(s): use --force", model.FormatID(parent)))
	if !errors.Is(err, verb.ErrIssueHasSubIssues) {
		t.Errorf("error = %v, want it to match ErrIssueHasSubIssues", err)
	}
	var refusal *verb.IssueHasSubIssuesError
	if !errors.As(err, &refusal) || refusal.SubIssues != 1 || refusal.Issue != model.FormatID(parent) {
		t.Errorf("error = %#v, want an IssueHasSubIssuesError naming the parent with 1 sub-issue", err)
	}
	if res != (verb.IssueDeleteResult{}) {
		t.Errorf("result = %+v, want the zero result: nothing was deleted", res)
	}
	for _, id := range []int{parent, child} {
		if _, err := st.GetIssue(id); err != nil {
			t.Errorf("GetIssue(%d) after a refused delete: %v", id, err)
		}
	}
}

func TestIssueDeleteCascadesToSubIssues(t *testing.T) {
	st := openStore(t)
	parent := createIssue(t, st)
	child := createChild(t, st, parent, "child", model.StatusTodo)

	res, err := verb.IssueDelete(context.Background(), projectEnv(st, "", false),
		verb.IssueDeleteInput{Issue: model.FormatID(parent), Cascade: true})
	if err != nil {
		t.Fatalf("IssueDelete --cascade: %v", err)
	}
	if res.SubIssues != 1 {
		t.Errorf("SubIssues = %d, want 1", res.SubIssues)
	}
	requireIssueGone(t, st, parent)
	requireIssueGone(t, st, child)
}

func TestIssueDeleteOrphansSubIssues(t *testing.T) {
	st := openStore(t)
	parent := createIssue(t, st)
	child := createChild(t, st, parent, "child", model.StatusTodo)
	env := projectEnv(st, "", false)
	env.Author = "deleter"

	if _, err := verb.IssueDelete(context.Background(), env,
		verb.IssueDeleteInput{Issue: model.FormatID(parent), Orphan: true}); err != nil {
		t.Fatalf("IssueDelete --orphan: %v", err)
	}
	requireIssueGone(t, st, parent)
	orphan, err := st.GetIssue(child)
	if err != nil {
		t.Fatalf("GetIssue(child): %v", err)
	}
	if orphan.ParentID != nil {
		t.Errorf("child parent = %d, want a root issue", *orphan.ParentID)
	}
	requireChangedBy(t, st, child, "parent_id", "", "deleter")
}

// IssueDelete names the deleted issue under its owning project's prefix, not
// the caller's, though the owner is gone from the store once it returns.
func TestIssueDeleteNamesTheIssueUnderItsOwnersPrefix(t *testing.T) {
	st := openStore(t)
	// The first identity registered claims the default project's row.
	ensureProject(t, st, "/repos/alpha", "alpha")
	beta := ensureProject(t, st, "/repos/beta", "beta")
	if err := st.SetProjectPrefix(beta, "BETA"); err != nil {
		t.Fatalf("SetProjectPrefix: %v", err)
	}
	id := createExportIssue(t, st, beta, "in beta", model.StatusTodo, "")

	res, err := verb.IssueDelete(context.Background(), projectEnv(st, "/repos/alpha", true),
		verb.IssueDeleteInput{Issue: model.FormatID(id)})
	if err != nil {
		t.Fatalf("IssueDelete: %v", err)
	}
	if want := model.FormatIDWithPrefix(id, "BETA"); res.Issue != want {
		t.Errorf("Issue = %q, want %q under the owner's prefix", res.Issue, want)
	}
	requireIssueGone(t, st, id)
}

func TestIssueDeleteRefusals(t *testing.T) {
	st := openStore(t)
	parent := createIssue(t, st)
	createChild(t, st, parent, "child", model.StatusTodo)
	ctx := context.Background()
	env := projectEnv(st, "", false)

	_, err := verb.IssueDelete(ctx, env, verb.IssueDeleteInput{
		Issue: model.FormatID(parent), Cascade: true, Orphan: true})
	requireVerbError(t, err, output.ErrValidation, "--force and --orphan are mutually exclusive")

	_, err = verb.IssueDelete(ctx, env, verb.IssueDeleteInput{Issue: "DKT-9999"})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")

	_, err = verb.IssueDelete(ctx, env, verb.IssueDeleteInput{Issue: "not-an-id"})
	requireCode(t, err, output.ErrValidation)

	if _, err := st.GetIssue(parent); err != nil {
		t.Errorf("GetIssue(parent) after refusals: %v", err)
	}
}

// IssueDelete writes, so it registers an anchored caller's project and refuses
// an unanchored one it would otherwise have to guess.
func TestIssueDeleteMayRegisterTheCallersProject(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()

	_, err := verb.IssueDelete(ctx, projectEnv(st, "/work/scratch", false),
		verb.IssueDeleteInput{Issue: model.FormatID(id)})
	requireCode(t, err, output.ErrValidation)
	requireUnregistered(t, st, "/work/scratch")
	if _, err := st.GetIssue(id); err != nil {
		t.Errorf("GetIssue after an unanchored delete: %v", err)
	}

	res, err := verb.IssueDelete(ctx, projectEnv(st, "/repos/fresh", true),
		verb.IssueDeleteInput{Issue: model.FormatID(id)})
	if err != nil {
		t.Fatalf("IssueDelete: %v", err)
	}
	if res.Registered == nil || res.Registered.Identity != "/repos/fresh" || res.Registered.Verb != "issue delete" {
		t.Errorf("Registered = %+v, want /repos/fresh registered by issue delete", res.Registered)
	}
}

func TestIssueShowReadsEachIssueAtFullDetailInOrder(t *testing.T) {
	st := openStore(t)
	parent := createIssue(t, st)
	child := createChild(t, st, parent, "child", model.StatusTodo)
	if err := st.SetIssueScopeGlobs(child, "[]"); err != nil {
		t.Fatalf("SetIssueScopeGlobs: %v", err)
	}

	res, err := verb.IssueShow(context.Background(), projectEnv(st, "", false),
		verb.IssueShowInput{Issues: []string{model.FormatID(child), model.FormatID(parent)}})
	if err != nil {
		t.Fatalf("IssueShow: %v", err)
	}
	if len(res.Issues) != 2 || res.Issues[0].Issue.ID != child || res.Issues[1].Issue.ID != parent {
		t.Fatalf("shown = %+v, want child then parent", res.Issues)
	}
	shownChild, shownParent := res.Issues[0], res.Issues[1]
	if !shownChild.ScopeDeclared || len(shownChild.Scope) != 0 {
		t.Errorf("child scope = %v (declared %v), want a declared empty scope",
			shownChild.Scope, shownChild.ScopeDeclared)
	}
	if shownParent.ScopeDeclared {
		t.Errorf("parent scope declared = true, want no declared scope")
	}
	if len(shownParent.SubIssues) != 1 || shownParent.SubIssues[0].ID != child {
		t.Errorf("parent sub-issues = %v, want [%d]", shownParent.SubIssues, child)
	}
	if shownParent.RunDisposition != nil {
		t.Errorf("RunDisposition = %+v, want nil for an issue no run abandoned", shownParent.RunDisposition)
	}
}

// IssueShow carries the issue's labels and files and the relations and
// comments around it, and caps activity at the latest ten entries.
func TestIssueShowCarriesEachCollectionAndTheLatestTenActivities(t *testing.T) {
	st := openStore(t)
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title:    "detailed",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, []string{"area:verb"}, []string{"internal/verb/issue.go"}, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	blocker := createIssue(t, st)
	relate(t, st, id, model.RelationBlocks, blocker)
	const comments = 11
	for i := range comments {
		if _, err := st.CreateCommentIdempotent(&model.Comment{
			IssueID: id, Body: fmt.Sprintf("comment %d", i), Author: "reviewer",
		}, ""); err != nil {
			t.Fatalf("CreateCommentIdempotent: %v", err)
		}
	}

	res, err := verb.IssueShow(context.Background(), projectEnv(st, "", false),
		verb.IssueShowInput{Issues: []string{model.FormatID(id)}})
	if err != nil {
		t.Fatalf("IssueShow: %v", err)
	}
	d := res.Issues[0]
	if len(d.Issue.Labels) != 1 || d.Issue.Labels[0] != "area:verb" {
		t.Errorf("labels = %v, want [area:verb]", d.Issue.Labels)
	}
	if len(d.Issue.Files) != 1 || d.Issue.Files[0] != "internal/verb/issue.go" {
		t.Errorf("files = %v, want [internal/verb/issue.go]", d.Issue.Files)
	}
	if len(d.Relations) != 1 || d.Relations[0].TargetIssueID != blocker {
		t.Errorf("relations = %+v, want one to %d", d.Relations, blocker)
	}
	if len(d.Comments) != comments {
		t.Errorf("comments = %d, want %d", len(d.Comments), comments)
	}
	if len(d.Activity) != 10 {
		t.Errorf("activity = %d entries, want the latest 10 of more than 10", len(d.Activity))
	}
}

func TestIssueShowStopsAtTheFirstMissingIssue(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)

	res, err := verb.IssueShow(context.Background(), projectEnv(st, "", false),
		verb.IssueShowInput{Issues: []string{model.FormatID(id), "DKT-9999"}})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	if len(res.Issues) != 0 {
		t.Errorf("result = %+v, want none on a failed show", res.Issues)
	}
}

// A docs read that fails after the issue read reports the linked docs, the
// text issue show has always printed for it.
func TestIssueShowNamesAFailedLinkedDocsRead(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	conn, err := store.SQLDB(st)
	if err != nil {
		t.Fatalf("SQLDB: %v", err)
	}
	if _, err := conn.Exec(`DROP TABLE doc_issue_links`); err != nil {
		t.Fatalf("dropping doc_issue_links: %v", err)
	}

	_, err = verb.IssueShow(context.Background(), projectEnv(st, "", false),
		verb.IssueShowInput{Issues: []string{model.FormatID(id)}})
	requireVerbCode(t, err, output.ErrGeneral, "fetching linked docs: ")
}

// IssueShow is a read: an anchored identity no project holds still shows an
// issue by id, and is not registered.
func TestIssueShowRegistersNothing(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)

	if _, err := verb.IssueShow(context.Background(), projectEnv(st, "/repos/fresh", true),
		verb.IssueShowInput{Issues: []string{model.FormatID(id)}}); err != nil {
		t.Fatalf("IssueShow: %v", err)
	}
	requireUnregistered(t, st, "/repos/fresh")
}

func TestIssueListReadsTheCallersProjectOrTheNamedOne(t *testing.T) {
	st := openStore(t)
	register := func(identity, name string) int {
		t.Helper()
		id, _, err := st.EnsureProjectCreated(identity, name, time.Now().UnixMilli())
		if err != nil {
			t.Fatalf("EnsureProjectCreated(%q): %v", identity, err)
		}
		return id
	}
	// The first identity registered claims the default project's row.
	createExportIssue(t, st, register("/repos/beta", "beta"), "in beta", model.StatusTodo, "")
	createExportIssue(t, st, register("/repos/alpha", "alpha"), "in alpha", model.StatusTodo, "")
	ctx := context.Background()
	alpha := projectEnv(st, "/repos/alpha", true)

	own, err := verb.IssueList(ctx, alpha, verb.IssueListInput{})
	if err != nil {
		t.Fatalf("IssueList: %v", err)
	}
	if got := issueTitles(own.Issues); len(got) != 1 || got[0] != "in alpha" || own.Total != 1 || own.Project != nil {
		t.Errorf("own listing = %v (total %d, project %v), want [in alpha] of the caller's project",
			got, own.Total, own.Project)
	}

	named, err := verb.IssueList(ctx, alpha, verb.IssueListInput{Project: "/repos/beta"})
	if err != nil {
		t.Fatalf("IssueList --project /repos/beta: %v", err)
	}
	if got := issueTitles(named.Issues); len(got) != 1 || got[0] != "in beta" {
		t.Errorf("named listing = %v, want [in beta]", got)
	}
	if named.Project == nil || named.Project.Identity != "/repos/beta" {
		t.Errorf("Project = %+v, want /repos/beta", named.Project)
	}
}

func TestIssueListOfAnUnregisteredProjectIsEmptyAndRegistersNothing(t *testing.T) {
	st := openStore(t)
	createExportIssue(t, st, store.DefaultProjectID, "in the default project", model.StatusTodo, "")

	res, err := verb.IssueList(context.Background(), projectEnv(st, "/repos/fresh", true), verb.IssueListInput{})
	if err != nil {
		t.Fatalf("IssueList: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Errorf("listed %v, want nothing for an unregistered project", issueTitles(res.Issues))
	}
	requireUnregistered(t, st, "/repos/fresh")
}

// An empty default listing for a table counts the done issues it hid, so its
// hint can say so instead of claiming the store is empty; a listing that
// included them, or one not read for a table, counts none.
func TestIssueListCountsTheDoneIssuesAnEmptyListingHid(t *testing.T) {
	st := openStore(t)
	for _, title := range []string{"first", "second"} {
		createExportIssue(t, st, store.DefaultProjectID, title, model.StatusDone, "")
	}
	ctx := context.Background()
	env := projectEnv(st, "", false)

	open, err := verb.IssueList(ctx, env, verb.IssueListInput{ForTable: true})
	if err != nil {
		t.Fatalf("IssueList: %v", err)
	}
	if len(open.Issues) != 0 || open.HiddenDone != 2 {
		t.Errorf("listing = %v, hidden %d; want none listed and 2 hidden", issueTitles(open.Issues), open.HiddenDone)
	}

	all, err := verb.IssueList(ctx, env, verb.IssueListInput{All: true, ForTable: true})
	if err != nil {
		t.Fatalf("IssueList --all: %v", err)
	}
	if len(all.Issues) != 2 || all.HiddenDone != 0 {
		t.Errorf("--all listing = %v, hidden %d; want both listed and none hidden", issueTitles(all.Issues), all.HiddenDone)
	}

	rows, err := verb.IssueList(ctx, env, verb.IssueListInput{})
	if err != nil {
		t.Fatalf("IssueList without ForTable: %v", err)
	}
	if len(rows.Issues) != 0 || rows.HiddenDone != 0 {
		t.Errorf("listing without ForTable = %v, hidden %d; want none listed and no count read",
			issueTitles(rows.Issues), rows.HiddenDone)
	}
}

// A --parent listing returns that parent's sub-issues and nothing else, and a
// table listing reads the progress of a parent it lists itself.
func TestIssueListParentFilterAndListedParentProgress(t *testing.T) {
	st := openStore(t)
	parent := createIssue(t, st)
	createChild(t, st, parent, "open child", model.StatusTodo)
	createChild(t, st, parent, "done child", model.StatusDone)
	other := createIssue(t, st)
	createChild(t, st, other, "other child", model.StatusTodo)
	ctx := context.Background()
	env := projectEnv(st, "", false)

	children, err := verb.IssueList(ctx, env, verb.IssueListInput{Parent: model.FormatID(parent), All: true})
	if err != nil {
		t.Fatalf("IssueList --parent: %v", err)
	}
	if got := issueTitles(children.Issues); len(got) != 2 || children.Total != 2 {
		t.Errorf("--parent listing = %v (total %d), want only parent's two children", got, children.Total)
	}
	for _, issue := range children.Issues {
		if issue.ParentID == nil || *issue.ParentID != parent {
			t.Errorf("--parent listed %q, which is not a child of %d", issue.Title, parent)
		}
	}

	table, err := verb.IssueList(ctx, env, verb.IssueListInput{All: true, ForTable: true})
	if err != nil {
		t.Fatalf("IssueList: %v", err)
	}
	if table.Parents[parent] != nil {
		t.Errorf("Parents = %v, want no fetch of a parent the listing holds", table.Parents)
	}
	if got := table.Progress[parent]; got != [2]int{1, 2} {
		t.Errorf("progress of the listed parent = %v, want 1 of 2 done", got)
	}
}

// Listing a run bound to another project lists that run's roster in its own
// project's voice: Project names the run's project and RunID the run.
func TestIssueListOfARunInAnotherProjectListsItsRoster(t *testing.T) {
	st := openStore(t)
	// The first identity registered claims the default project's row.
	ensureProject(t, st, "/repos/alpha", "alpha")
	beta := ensureProject(t, st, "/repos/beta", "beta")
	bound := createExportIssue(t, st, beta, "bound in beta", model.StatusDone, "")
	createExportIssue(t, st, beta, "unbound in beta", model.StatusTodo, "")
	runID := insertLedgerRun(t, st, beta, time.Now().UnixMilli())
	if err := st.AddRunIssue(runID, bound); err != nil {
		t.Fatalf("AddRunIssue: %v", err)
	}

	res, err := verb.IssueList(context.Background(), projectEnv(st, "/repos/alpha", true),
		verb.IssueListInput{Run: model.FormatRunID(runID)})
	if err != nil {
		t.Fatalf("IssueList --run: %v", err)
	}
	if res.RunID != runID {
		t.Errorf("RunID = %d, want %d", res.RunID, runID)
	}
	if res.Project == nil || res.Project.ID != beta {
		t.Errorf("Project = %+v, want the run's project %d", res.Project, beta)
	}
	if got := issueTitles(res.Issues); len(got) != 1 || got[0] != "bound in beta" {
		t.Errorf("roster = %v, want [bound in beta], done issues included", got)
	}
}

// ForTable reads the parent a filter left out and the sub-issue progress
// a grouped rendering shows under it.
func TestIssueListForTableReadsParentsAndProgress(t *testing.T) {
	st := openStore(t)
	parent := createIssue(t, st)
	createChild(t, st, parent, "open child", model.StatusBacklog)
	createChild(t, st, parent, "done child", model.StatusDone)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	backlog := []string{string(model.StatusBacklog)}

	res, err := verb.IssueList(ctx, env, verb.IssueListInput{Statuses: backlog, ForTable: true})
	if err != nil {
		t.Fatalf("IssueList: %v", err)
	}
	if got := issueTitles(res.Issues); len(got) != 1 || got[0] != "open child" {
		t.Fatalf("listed %v, want [open child]", got)
	}
	if res.Parents[parent] == nil {
		t.Errorf("Parents = %v, want the filtered-out parent %d", res.Parents, parent)
	}
	if got := res.Progress[parent]; got != [2]int{1, 2} {
		t.Errorf("progress = %v, want 1 of 2 done", got)
	}

	flat, err := verb.IssueList(ctx, env, verb.IssueListInput{Statuses: backlog})
	if err != nil {
		t.Fatalf("IssueList without parents: %v", err)
	}
	if flat.Parents != nil || flat.Progress != nil {
		t.Errorf("without ForTable: parents %v, progress %v; want neither read", flat.Parents, flat.Progress)
	}
}

func TestIssueListRefusals(t *testing.T) {
	env := projectEnv(openStore(t), "", false)

	for name, tc := range map[string]struct {
		in     verb.IssueListInput
		code   output.ErrorCode
		prefix string
	}{
		"run and project": {verb.IssueListInput{Run: "RUN-1", Project: "beta"},
			output.ErrValidation, "--run and --project cannot be combined"},
		"status": {verb.IssueListInput{Statuses: []string{"shipped"}},
			output.ErrValidation, `invalid status "shipped"`},
		"unknown run": {verb.IssueListInput{Run: "RUN-404"}, output.ErrNotFound, "run RUN-404 not found"},
		"parent":      {verb.IssueListInput{Parent: "nope"}, output.ErrValidation, "invalid parent ID: "},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verb.IssueList(context.Background(), env, tc.in)
			requireVerbError(t, err, tc.code, tc.prefix)
		})
	}
}

func TestIssueCommentAddStoresTheCommentAsTheAuthor(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	env := projectEnv(st, "", false)
	env.Author = "reviewer"

	res, err := verb.IssueCommentAdd(context.Background(), env, verb.IssueCommentAddInput{
		Issue: model.FormatID(id), Body: "looks right",
	})
	if err != nil {
		t.Fatalf("IssueCommentAdd: %v", err)
	}
	if res.Issue == nil || res.Issue.ID != id || res.Issue.Title != "lease subject" {
		t.Errorf("Issue = %+v, want the commented issue %d", res.Issue, id)
	}
	if res.Comment == nil || res.Comment.Body != "looks right" || res.Comment.Author != "reviewer" {
		t.Fatalf("Comment = %+v, want the stored comment by reviewer", res.Comment)
	}
	stored, err := st.ListComments(id)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(stored) != 1 || stored[0].ID != res.Comment.ID {
		t.Errorf("stored comments = %+v, want only comment %d", stored, res.Comment.ID)
	}
}

// A replayed idempotency key returns the comment it first created; reusing it
// with another body is CONFLICT and adds nothing.
func TestIssueCommentAddReplaysItsIdempotencyKey(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	add := func(body string) (verb.IssueCommentAddResult, error) {
		return verb.IssueCommentAdd(ctx, env, verb.IssueCommentAddInput{
			Issue: model.FormatID(id), Body: body, IdempotencyKey: "retry-key",
		})
	}

	first, err := add("first body")
	if err != nil {
		t.Fatalf("first IssueCommentAdd: %v", err)
	}
	replayed, err := add("first body")
	if err != nil {
		t.Fatalf("replayed IssueCommentAdd: %v", err)
	}
	if replayed.Comment.ID != first.Comment.ID {
		t.Errorf("replayed comment = %d, want the first, %d", replayed.Comment.ID, first.Comment.ID)
	}
	_, err = add("second body")
	requireCode(t, err, output.ErrConflict)
	if stored, _ := st.ListComments(id); len(stored) != 1 {
		t.Errorf("issue has %d comments, want only the first", len(stored))
	}
}

func TestIssueCommentAndLogRefusals(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	env := projectEnv(st, "", false)

	_, err := verb.IssueCommentTarget(ctx, env, verb.IssueCommentTargetInput{Issue: "DKT-9999"})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	_, err = verb.IssueCommentAdd(ctx, env, verb.IssueCommentAddInput{Issue: "DKT-9999", Body: "b"})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	_, err = verb.IssueCommentList(ctx, env, verb.IssueCommentListInput{Issue: "DKT-9999"})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	_, err = verb.IssueLog(ctx, env, verb.IssueLogInput{Issue: "DKT-9999", Limit: 20})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")

	_, err = verb.IssueCommentTarget(ctx, env, verb.IssueCommentTargetInput{Issue: "not-an-id"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueCommentAdd(ctx, env, verb.IssueCommentAddInput{Issue: "not-an-id", Body: "b"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueCommentList(ctx, env, verb.IssueCommentListInput{Issue: "not-an-id"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueLog(ctx, env, verb.IssueLogInput{Issue: "not-an-id", Limit: 20})
	requireCode(t, err, output.ErrValidation)
}

// Adding a comment writes, so it and its pre-write check register an anchored
// caller's project and refuse an unanchored one.
func TestIssueCommentAddMayRegisterTheCallersProject(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	ref := model.FormatID(id)

	unanchored := projectEnv(st, "/work/scratch", false)
	_, err := verb.IssueCommentTarget(ctx, unanchored, verb.IssueCommentTargetInput{Issue: ref})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueCommentAdd(ctx, unanchored, verb.IssueCommentAddInput{Issue: ref, Body: "b"})
	requireCode(t, err, output.ErrValidation)
	requireUnregistered(t, st, "/work/scratch")
	if stored, _ := st.ListComments(id); len(stored) != 0 {
		t.Errorf("issue has %d comments after unanchored adds, want none", len(stored))
	}

	target, err := verb.IssueCommentTarget(ctx, projectEnv(st, "/repos/fresh", true),
		verb.IssueCommentTargetInput{Issue: ref})
	if err != nil {
		t.Fatalf("IssueCommentTarget: %v", err)
	}
	if target.Registered == nil || target.Registered.Identity != "/repos/fresh" ||
		target.Registered.Verb != "issue comment add" {
		t.Errorf("Registered = %+v, want /repos/fresh registered by issue comment add", target.Registered)
	}
	if target.Issue == nil || target.Issue.ID != id {
		t.Errorf("target Issue = %+v, want issue %d", target.Issue, id)
	}

	res, err := verb.IssueCommentAdd(ctx, projectEnv(st, "/repos/other", true),
		verb.IssueCommentAddInput{Issue: ref, Body: "b"})
	if err != nil {
		t.Fatalf("IssueCommentAdd: %v", err)
	}
	if res.Registered == nil || res.Registered.Identity != "/repos/other" ||
		res.Registered.Verb != "issue comment add" {
		t.Errorf("Registered = %+v, want /repos/other registered by issue comment add", res.Registered)
	}
}

func TestIssueCommentListReadsTheCommentsOldestFirst(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	for _, body := range []string{"first", "second"} {
		if _, err := st.CreateCommentIdempotent(&model.Comment{IssueID: id, Body: body, Author: "a"}, ""); err != nil {
			t.Fatalf("CreateCommentIdempotent: %v", err)
		}
	}

	res, err := verb.IssueCommentList(context.Background(), projectEnv(st, "", false),
		verb.IssueCommentListInput{Issue: model.FormatID(id)})
	if err != nil {
		t.Fatalf("IssueCommentList: %v", err)
	}
	if res.IssueID != id {
		t.Errorf("IssueID = %d, want %d", res.IssueID, id)
	}
	if len(res.Comments) != 2 || res.Comments[0].Body != "first" || res.Comments[1].Body != "second" {
		t.Errorf("comments = %+v, want first then second", res.Comments)
	}
}

// IssueLog caps the entries at Limit, reads one entry for a zero Limit as the
// CLI always has, counts every entry, and refuses a negative Limit.
func TestIssueLogCapsTheEntriesAndCountsThemAll(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	for i := range 4 {
		if _, err := st.CreateCommentIdempotent(&model.Comment{
			IssueID: id, Body: fmt.Sprintf("comment %d", i), Author: "a",
		}, ""); err != nil {
			t.Fatalf("CreateCommentIdempotent: %v", err)
		}
	}
	all, err := st.CountActivity(id)
	if err != nil {
		t.Fatalf("CountActivity: %v", err)
	}
	if all < 4 {
		t.Fatalf("activity count = %d, want at least 4 to exercise the cap", all)
	}
	ctx := context.Background()
	env := projectEnv(st, "", false)

	for _, tc := range []struct{ limit, entries, effective int }{
		{limit: 3, entries: 3, effective: 3},
		{limit: 0, entries: 1, effective: 1},
	} {
		res, err := verb.IssueLog(ctx, env, verb.IssueLogInput{Issue: model.FormatID(id), Limit: tc.limit})
		if err != nil {
			t.Fatalf("IssueLog(limit %d): %v", tc.limit, err)
		}
		if res.IssueID != id || len(res.Entries) != tc.entries || res.Total != all || res.Limit != tc.effective {
			t.Errorf("IssueLog(limit %d) = issue %d, %d entries, total %d, limit %d; want issue %d, %d, %d, %d",
				tc.limit, res.IssueID, len(res.Entries), res.Total, res.Limit, id, tc.entries, all, tc.effective)
		}
	}

	_, err = verb.IssueLog(ctx, env, verb.IssueLogInput{Issue: model.FormatID(id), Limit: -1})
	requireVerbError(t, err, output.ErrValidation, "--limit must be >= 0, got -1 (0 means no limit)")
}

// The comment list and the log are reads: an anchored identity no project
// holds still reads an issue by id, and is not registered.
func TestIssueCommentListAndLogRegisterNothing(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "/repos/fresh", true)

	if _, err := verb.IssueCommentList(ctx, env, verb.IssueCommentListInput{Issue: model.FormatID(id)}); err != nil {
		t.Fatalf("IssueCommentList: %v", err)
	}
	if _, err := verb.IssueLog(ctx, env, verb.IssueLogInput{Issue: model.FormatID(id), Limit: 20}); err != nil {
		t.Fatalf("IssueLog: %v", err)
	}
	requireUnregistered(t, st, "/repos/fresh")
}

func TestIssueFileAddAndRemoveEchoTheStoredFilesAsTheAuthor(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	env.Author = "file-author"
	ref := model.FormatID(id)

	added, err := verb.IssueFileAdd(ctx, env, verb.IssueFileAddInput{Issue: ref, Paths: []string{"a/one.go", "b/two.go"}})
	if err != nil {
		t.Fatalf("IssueFileAdd: %v", err)
	}
	if added.Issue == nil || added.Issue.ID != id {
		t.Errorf("added Issue = %+v, want issue %d", added.Issue, id)
	}
	if len(added.Files) != 2 {
		t.Errorf("added Files = %v, want a/one.go and b/two.go", added.Files)
	}
	requireChangedBy(t, st, id, "files", "a/one.go, b/two.go", "file-author")

	removed, err := verb.IssueFileRemove(ctx, env, verb.IssueFileRemoveInput{Issue: ref, Paths: []string{"a/one.go"}})
	if err != nil {
		t.Fatalf("IssueFileRemove: %v", err)
	}
	if removed.Issue == nil || removed.Issue.ID != id {
		t.Errorf("removed Issue = %+v, want issue %d", removed.Issue, id)
	}
	if len(removed.Files) != 1 || removed.Files[0] != "b/two.go" {
		t.Errorf("removed Files = %v, want [b/two.go]", removed.Files)
	}
	activity, err := st.GetActivity(id, 0)
	if err != nil {
		t.Fatalf("GetActivity: %v", err)
	}
	detached := false
	for _, a := range activity {
		detached = detached || (a.FieldChanged == "files" && a.OldValue == "a/one.go" &&
			a.NewValue == "" && a.ChangedBy == "file-author")
	}
	if !detached {
		t.Errorf("activity %+v has no removal of a/one.go attributed to file-author", activity)
	}

	listed, err := verb.IssueFileList(ctx, env, verb.IssueFileListInput{Issue: ref})
	if err != nil {
		t.Fatalf("IssueFileList: %v", err)
	}
	if listed.IssueID != id || len(listed.Files) != 1 || listed.Files[0] != "b/two.go" {
		t.Errorf("IssueFileList = issue %d, files %v; want issue %d, [b/two.go]", listed.IssueID, listed.Files, id)
	}
}

func TestIssueFileVerbRefusals(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	paths := []string{"a.go"}

	_, err := verb.IssueFileAdd(ctx, env, verb.IssueFileAddInput{Issue: "DKT-9999", Paths: paths})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	_, err = verb.IssueFileRemove(ctx, env, verb.IssueFileRemoveInput{Issue: "DKT-9999", Paths: paths})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	_, err = verb.IssueFileList(ctx, env, verb.IssueFileListInput{Issue: "DKT-9999"})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")

	_, err = verb.IssueFileAdd(ctx, env, verb.IssueFileAddInput{Issue: "not-an-id", Paths: paths})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueFileRemove(ctx, env, verb.IssueFileRemoveInput{Issue: "not-an-id", Paths: paths})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueFileList(ctx, env, verb.IssueFileListInput{Issue: "not-an-id"})
	requireCode(t, err, output.ErrValidation)
}

// Attaching and detaching files write, so they register an anchored caller's
// project and refuse an unanchored one before writing; listing is a read.
func TestIssueFileVerbsDeclareTheirProjectPolicy(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	ref := model.FormatID(id)
	paths := []string{"a.go"}

	unanchored := projectEnv(st, "/work/scratch", false)
	_, err := verb.IssueFileAdd(ctx, unanchored, verb.IssueFileAddInput{Issue: ref, Paths: paths})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueFileRemove(ctx, unanchored, verb.IssueFileRemoveInput{Issue: ref, Paths: paths})
	requireCode(t, err, output.ErrValidation)
	requireUnregistered(t, st, "/work/scratch")
	if files, _ := st.GetIssueFiles(id); len(files) != 0 {
		t.Errorf("issue has files %v after unanchored adds, want none", files)
	}

	// The project refusal comes before the issue lookup.
	_, err = verb.IssueFileAdd(ctx, unanchored, verb.IssueFileAddInput{Issue: "DKT-9999", Paths: paths})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueFileRemove(ctx, unanchored, verb.IssueFileRemoveInput{Issue: "DKT-9999", Paths: paths})
	requireCode(t, err, output.ErrValidation)

	if _, err := verb.IssueFileList(ctx, projectEnv(st, "/repos/reader", true),
		verb.IssueFileListInput{Issue: ref}); err != nil {
		t.Fatalf("IssueFileList: %v", err)
	}
	requireUnregistered(t, st, "/repos/reader")

	added, err := verb.IssueFileAdd(ctx, projectEnv(st, "/repos/fresh", true),
		verb.IssueFileAddInput{Issue: ref, Paths: paths})
	if err != nil {
		t.Fatalf("IssueFileAdd: %v", err)
	}
	if added.Registered == nil || added.Registered.Identity != "/repos/fresh" ||
		added.Registered.Verb != "issue file add" {
		t.Errorf("Registered = %+v, want /repos/fresh registered by issue file add", added.Registered)
	}

	removed, err := verb.IssueFileRemove(ctx, projectEnv(st, "/repos/other", true),
		verb.IssueFileRemoveInput{Issue: ref, Paths: paths})
	if err != nil {
		t.Fatalf("IssueFileRemove: %v", err)
	}
	if removed.Registered == nil || removed.Registered.Identity != "/repos/other" ||
		removed.Registered.Verb != "issue file remove" {
		t.Errorf("Registered = %+v, want /repos/other registered by issue file remove", removed.Registered)
	}
}

// blocks records that blocker blocks blocked.
func blocks(t *testing.T, st store.Store, blocker, blocked int) {
	t.Helper()
	if _, err := st.CreateRelation(&model.Relation{
		SourceIssueID: blocker, TargetIssueID: blocked, RelationType: model.RelationBlocks,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
}

// The graph walks a chain up -> focal -> down -> further in the requested
// direction, stops at Depth, names every edge blocker-to-blocked, carries
// adjacency only for the issues it reached, and is a read that registers
// nothing.
func TestIssueGraphWalksTheRequestedDirectionToTheDepth(t *testing.T) {
	st := openStore(t)
	up, focal, down, further := createIssue(t, st), createIssue(t, st), createIssue(t, st), createIssue(t, st)
	blocks(t, st, up, focal)
	blocks(t, st, focal, down)
	blocks(t, st, down, further)
	blocks(t, st, createIssue(t, st), createIssue(t, st))
	ctx := context.Background()
	env := projectEnv(st, "/repos/reader", true)
	ref := model.FormatID(focal)

	for _, tc := range []struct {
		direction string
		depth     int
		nodes     []int
		edges     []verb.IssueGraphEdge
	}{
		{"both", 0, []int{up, focal, down, further}, []verb.IssueGraphEdge{
			{From: focal, To: down, Type: "blocks"}, {From: down, To: further, Type: "blocks"},
			{From: up, To: focal, Type: "blocks"}}},
		{"down", 1, []int{focal, down}, []verb.IssueGraphEdge{{From: focal, To: down, Type: "blocks"}}},
		{"up", 0, []int{up, focal}, []verb.IssueGraphEdge{{From: up, To: focal, Type: "blocks"}}},
	} {
		res, err := verb.IssueGraph(ctx, env, verb.IssueGraphInput{Issue: ref, Direction: tc.direction, Depth: tc.depth})
		if err != nil {
			t.Fatalf("IssueGraph(%s, %d): %v", tc.direction, tc.depth, err)
		}
		if res.Issue == nil || res.Issue.ID != focal {
			t.Errorf("IssueGraph(%s, %d) Issue = %+v, want issue %d", tc.direction, tc.depth, res.Issue, focal)
		}
		if len(res.Issues) != len(tc.nodes) {
			t.Errorf("IssueGraph(%s, %d) reached %d issues, want %v", tc.direction, tc.depth, len(res.Issues), tc.nodes)
		}
		for _, n := range tc.nodes {
			if res.Issues[n] == nil {
				t.Errorf("IssueGraph(%s, %d) did not reach issue %d", tc.direction, tc.depth, n)
			}
		}
		if fmt.Sprint(res.Edges) != fmt.Sprint(tc.edges) {
			t.Errorf("IssueGraph(%s, %d) edges = %v, want %v", tc.direction, tc.depth, res.Edges, tc.edges)
		}
		if got := res.Forward[focal]; len(got) != 1 || got[0] != down {
			t.Errorf("IssueGraph(%s, %d) Forward[focal] = %v, want [%d]", tc.direction, tc.depth, got, down)
		}
		if got := res.Backward[focal]; len(got) != 1 || got[0] != up {
			t.Errorf("IssueGraph(%s, %d) Backward[focal] = %v, want [%d]", tc.direction, tc.depth, got, up)
		}
		for name, adj := range map[string]map[int][]int{"Forward": res.Forward, "Backward": res.Backward} {
			for k := range adj {
				if res.Issues[k] == nil {
					t.Errorf("IssueGraph(%s, %d) %s carries unreached issue %d", tc.direction, tc.depth, name, k)
				}
			}
		}
	}
	requireUnregistered(t, st, "/repos/reader")
}

// A diamond (focal blocks left and right; both block join) reaches join by
// two paths: the walk keeps both edges into it in either direction, and a
// depth that stops short of join leaves it and its edges out.
func TestIssueGraphKeepsEveryEdgeIntoAConvergingIssue(t *testing.T) {
	st := openStore(t)
	focal, left, right, join := createIssue(t, st), createIssue(t, st), createIssue(t, st), createIssue(t, st)
	blocks(t, st, focal, left)
	blocks(t, st, focal, right)
	blocks(t, st, left, join)
	blocks(t, st, right, join)
	ctx := context.Background()
	env := projectEnv(st, "/repos/reader", true)
	all := []verb.IssueGraphEdge{
		{From: focal, To: left, Type: "blocks"}, {From: focal, To: right, Type: "blocks"},
		{From: left, To: join, Type: "blocks"}, {From: right, To: join, Type: "blocks"},
	}

	for _, tc := range []struct {
		start, direction string
		depth            int
		nodes            []int
		edges            []verb.IssueGraphEdge
	}{
		{model.FormatID(focal), "down", 0, []int{focal, left, right, join}, all},
		{model.FormatID(focal), "down", 2, []int{focal, left, right, join}, all},
		{model.FormatID(join), "up", 0, []int{focal, left, right, join}, all},
		{model.FormatID(focal), "down", 1, []int{focal, left, right}, all[:2]},
		{model.FormatID(join), "up", 1, []int{left, right, join}, all[2:]},
	} {
		res, err := verb.IssueGraph(ctx, env, verb.IssueGraphInput{Issue: tc.start, Direction: tc.direction, Depth: tc.depth})
		if err != nil {
			t.Fatalf("IssueGraph(%s, %s, %d): %v", tc.start, tc.direction, tc.depth, err)
		}
		if len(res.Issues) != len(tc.nodes) {
			t.Errorf("IssueGraph(%s, %s, %d) reached %d issues, want %v", tc.start, tc.direction, tc.depth, len(res.Issues), tc.nodes)
		}
		for _, n := range tc.nodes {
			if res.Issues[n] == nil {
				t.Errorf("IssueGraph(%s, %s, %d) did not reach issue %d", tc.start, tc.direction, tc.depth, n)
			}
		}
		got := map[verb.IssueGraphEdge]int{}
		for _, e := range res.Edges {
			got[e]++
		}
		want := map[verb.IssueGraphEdge]int{}
		for _, e := range tc.edges {
			want[e]++
		}
		if !maps.Equal(got, want) {
			t.Errorf("IssueGraph(%s, %s, %d) edges = %v, want each of %v once", tc.start, tc.direction, tc.depth, res.Edges, tc.edges)
		}
	}
}

func TestIssueGraphRefusals(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	ref := model.FormatID(id)

	_, err := verb.IssueGraph(ctx, env, verb.IssueGraphInput{Issue: "DKT-9999", Direction: "sideways"})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	_, err = verb.IssueGraph(ctx, env, verb.IssueGraphInput{Issue: "not-an-id", Direction: "both"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueGraph(ctx, env, verb.IssueGraphInput{Issue: ref, Direction: "sideways"})
	requireVerbError(t, err, output.ErrValidation, `invalid direction "sideways": must be one of [up, down, both]`)
	_, err = verb.IssueGraph(ctx, env, verb.IssueGraphInput{Issue: ref, Direction: "both", Depth: -1})
	requireVerbError(t, err, output.ErrValidation, "depth must be non-negative")
}

func labelNames(labels []*model.Label) []string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names
}

func TestIssueLabelAddAndRmEchoTheStoredLabelsAsTheAuthor(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	env.Author = "label-author"
	ref := model.FormatID(id)

	added, err := verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{
		Issue: ref, Labels: []string{"alpha", "beta"}, Color: "#ff0000",
	})
	if err != nil {
		t.Fatalf("IssueLabelAdd: %v", err)
	}
	if added.Issue == nil || added.Issue.ID != id {
		t.Errorf("added Issue = %+v, want issue %d", added.Issue, id)
	}
	if got := fmt.Sprint(labelNames(added.Labels)); got != "[alpha beta]" {
		t.Errorf("added Labels = %s, want [alpha beta]", got)
	}
	for _, l := range added.Labels {
		if l.Color != "#ff0000" {
			t.Errorf("label %s color = %q, want #ff0000", l.Name, l.Color)
		}
	}
	requireChangedBy(t, st, id, "label_added", "alpha", "label-author")
	requireChangedBy(t, st, id, "label_added", "beta", "label-author")

	removed, err := verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: ref, Labels: []string{"alpha"}})
	if err != nil {
		t.Fatalf("IssueLabelRm: %v", err)
	}
	if removed.Issue == nil || removed.Issue.ID != id {
		t.Errorf("removed Issue = %+v, want issue %d", removed.Issue, id)
	}
	if got := fmt.Sprint(labelNames(removed.Labels)); got != "[beta]" {
		t.Errorf("removed Labels = %s, want [beta]", got)
	}
	requireLabelRemovedBy(t, st, id, "alpha", "label-author")
}

// requireLabelRemovedBy asserts the activity log attributes the removal of
// label from issue id to author.
func requireLabelRemovedBy(t *testing.T, st store.Store, id int, label, author string) {
	t.Helper()
	activity, err := st.GetActivity(id, 0)
	if err != nil {
		t.Fatalf("GetActivity: %v", err)
	}
	for _, a := range activity {
		if a.FieldChanged == "label_removed" && a.OldValue == label && a.ChangedBy == author {
			return
		}
	}
	t.Errorf("activity %+v has no removal of %s attributed to %s", activity, label, author)
}

func TestIssueLabelVerbRefusals(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	ref := model.FormatID(id)
	labels := []string{"alpha"}

	_, err := verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{Issue: "DKT-9999", Labels: labels})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	_, err = verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: "DKT-9999", Labels: labels})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	_, err = verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{Issue: "not-an-id", Labels: labels})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: "not-an-id", Labels: labels})
	requireCode(t, err, output.ErrValidation)

	// The issue is looked up before the label names are checked.
	_, err = verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{Issue: "DKT-9999", Labels: []string{" "}})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")
	_, err = verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: "DKT-9999", Labels: []string{""}})
	requireVerbError(t, err, output.ErrNotFound, "issue DKT-9999 not found")

	_, err = verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{Issue: ref, Labels: []string{"alpha", " "}})
	requireVerbError(t, err, output.ErrValidation, "label name cannot be empty")
	if got, _ := st.GetIssueLabelObjects(id); len(got) != 0 {
		t.Errorf("a refused add attached %v, want nothing", labelNames(got))
	}
	_, err = verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: ref, Labels: []string{""}})
	requireVerbError(t, err, output.ErrValidation, "label name cannot be empty")

	_, err = verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: ref, Labels: []string{"never-made"}})
	requireVerbError(t, err, output.ErrNotFound, "issue or label not found")

	if _, err := verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{Issue: ref, Labels: labels, Color: "#ff0000"}); err != nil {
		t.Fatalf("IssueLabelAdd: %v", err)
	}
	_, err = verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{Issue: ref, Labels: labels, Color: "#00ff00"})
	requireVerbError(t, err, output.ErrValidation, "label already exists with a different color")

	other := createIssue(t, st)
	_, err = verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: model.FormatID(other), Labels: labels})
	requireVerbError(t, err, output.ErrValidation, "label is not attached to "+model.FormatID(other))
}

// IssueLabelRm names an issue the label is not attached to under the issue's
// project prefix, not the process-wide default one.
func TestIssueLabelRmNamesAnUnattachedIssueUnderItsProjectsPrefix(t *testing.T) {
	st := openStore(t)
	// The first identity registered claims the default project's row.
	ensureProject(t, st, "/repos/alpha", "alpha")
	beta := ensureProject(t, st, "/repos/beta", "beta")
	if err := st.SetProjectPrefix(beta, "BETA"); err != nil {
		t.Fatalf("SetProjectPrefix: %v", err)
	}
	labeled := createExportIssue(t, st, beta, "labeled", model.StatusTodo, "")
	other := createExportIssue(t, st, beta, "unlabeled", model.StatusTodo, "")
	ctx := context.Background()
	env := projectEnv(st, "/repos/beta", true)
	if _, err := verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{
		Issue: model.FormatID(labeled), Labels: []string{"alpha"},
	}); err != nil {
		t.Fatalf("IssueLabelAdd: %v", err)
	}

	_, err := verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: model.FormatID(other), Labels: []string{"alpha"}})
	requireVerbError(t, err, output.ErrValidation, "label is not attached to "+model.FormatIDWithPrefix(other, "BETA"))
}

func TestIssueLabelListCountsTheIssuesOfTheCallersProject(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	if _, err := verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{
		Issue: model.FormatID(id), Labels: []string{"alpha"},
	}); err != nil {
		t.Fatalf("IssueLabelAdd: %v", err)
	}

	listed, err := verb.IssueLabelList(ctx, env, verb.IssueLabelListInput{})
	if err != nil {
		t.Fatalf("IssueLabelList: %v", err)
	}
	if len(listed.Labels) != 1 || listed.Labels[0].Name != "alpha" || listed.Labels[0].IssueCount != 1 {
		t.Errorf("IssueLabelList = %+v, want alpha on one issue", listed.Labels)
	}

	reader, err := verb.IssueLabelList(ctx, projectEnv(st, "/repos/reader", true), verb.IssueLabelListInput{})
	if err != nil {
		t.Fatalf("IssueLabelList from an unregistered project: %v", err)
	}
	if len(reader.Labels) != 0 {
		t.Errorf("an unregistered project listed %+v, want no labels", reader.Labels)
	}
	requireUnregistered(t, st, "/repos/reader")
}

func TestIssueLabelDeleteRefusesALabelInUseWithoutForce(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	env.Author = "label-deleter"
	if _, err := verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{
		Issue: model.FormatID(id), Labels: []string{"alpha"},
	}); err != nil {
		t.Fatalf("IssueLabelAdd: %v", err)
	}

	refused, err := verb.IssueLabelDelete(ctx, env, verb.IssueLabelDeleteInput{Label: "alpha"})
	requireVerbError(t, err, output.ErrValidation, `label "alpha" is attached to 1 issue(s): use --force to delete it`)
	if !errors.Is(err, verb.ErrLabelInUse) {
		t.Errorf("refusal %v does not wrap ErrLabelInUse", err)
	}
	if refused.Label == nil || refused.Label.Name != "alpha" || refused.Label.IssueCount != 1 {
		t.Errorf("refused Label = %+v, want alpha on one issue", refused.Label)
	}
	if got, _ := st.GetIssueLabelObjects(id); len(got) != 1 {
		t.Fatalf("a refused delete left labels %v, want alpha still attached", labelNames(got))
	}

	deleted, err := verb.IssueLabelDelete(ctx, env, verb.IssueLabelDeleteInput{Label: "alpha", Force: true})
	if err != nil {
		t.Fatalf("IssueLabelDelete --force: %v", err)
	}
	if deleted.Detached != 1 {
		t.Errorf("Detached = %d, want 1", deleted.Detached)
	}
	if got, _ := st.GetIssueLabelObjects(id); len(got) != 0 {
		t.Errorf("issue still carries %v after the delete", labelNames(got))
	}
	requireLabelRemovedBy(t, st, id, "alpha", "label-deleter")

	_, err = verb.IssueLabelDelete(ctx, env, verb.IssueLabelDeleteInput{Label: "alpha", Force: true})
	requireVerbError(t, err, output.ErrNotFound, `label "alpha" not found`)
}

func TestIssueLabelDeleteOfAnUnusedLabelNeedsNoForce(t *testing.T) {
	st := openStore(t)
	id := createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	ref := model.FormatID(id)
	if _, err := verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{Issue: ref, Labels: []string{"alpha"}}); err != nil {
		t.Fatalf("IssueLabelAdd: %v", err)
	}
	if _, err := verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: ref, Labels: []string{"alpha"}}); err != nil {
		t.Fatalf("IssueLabelRm: %v", err)
	}

	deleted, err := verb.IssueLabelDelete(ctx, env, verb.IssueLabelDeleteInput{Label: "alpha"})
	if err != nil {
		t.Fatalf("IssueLabelDelete: %v", err)
	}
	if deleted.Detached != 0 {
		t.Errorf("Detached = %d, want 0", deleted.Detached)
	}
	listed, err := verb.IssueLabelList(ctx, env, verb.IssueLabelListInput{})
	if err != nil {
		t.Fatalf("IssueLabelList: %v", err)
	}
	if len(listed.Labels) != 0 {
		t.Errorf("labels %+v remain after the delete", listed.Labels)
	}
}

func TestIssueLinkAddListAndRemove(t *testing.T) {
	st := openStore(t)
	source, target := createIssue(t, st), createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	src, dst := model.FormatID(source), model.FormatID(target)

	added, err := verb.IssueLinkAdd(ctx, env, verb.IssueLinkAddInput{Issue: src, Relation: "blocks", Target: dst})
	if err != nil {
		t.Fatalf("IssueLinkAdd: %v", err)
	}
	rel := added.Relation
	if rel == nil || rel.ID == 0 || rel.SourceIssueID != source || rel.TargetIssueID != target ||
		rel.RelationType != model.RelationBlocks {
		t.Fatalf("added Relation = %+v, want a stored %d blocks %d", rel, source, target)
	}

	for _, issue := range []int{source, target} {
		listed, err := verb.IssueLinkList(ctx, env, verb.IssueLinkListInput{Issue: model.FormatID(issue)})
		if err != nil {
			t.Fatalf("IssueLinkList %d: %v", issue, err)
		}
		if listed.IssueID != issue || len(listed.Relations) != 1 || listed.Relations[0].ID != rel.ID {
			t.Errorf("IssueLinkList %d = %+v, want relation %d", issue, listed, rel.ID)
		}
	}

	removed, err := verb.IssueLinkRemove(ctx, env, verb.IssueLinkRemoveInput{Issue: src, Relation: "blocks", Target: dst})
	if err != nil {
		t.Fatalf("IssueLinkRemove: %v", err)
	}
	if r := removed.Relation; r == nil || r.SourceIssueID != source || r.TargetIssueID != target ||
		r.RelationType != model.RelationBlocks {
		t.Errorf("removed Relation = %+v, want %d blocks %d", r, source, target)
	}
	listed, err := verb.IssueLinkList(ctx, env, verb.IssueLinkListInput{Issue: src})
	if err != nil {
		t.Fatalf("IssueLinkList: %v", err)
	}
	if len(listed.Relations) != 0 {
		t.Errorf("relations %+v remain after the remove", listed.Relations)
	}
}

func TestIssueLinkVerbRefusals(t *testing.T) {
	st := openStore(t)
	a, b := createIssue(t, st), createIssue(t, st)
	ctx := context.Background()
	env := projectEnv(st, "", false)
	ra, rb := model.FormatID(a), model.FormatID(b)
	add := func(issue, relation, target string) error {
		_, err := verb.IssueLinkAdd(ctx, env, verb.IssueLinkAddInput{Issue: issue, Relation: relation, Target: target})
		return err
	}

	// Arguments are checked in order: source, relation, target.
	requireCode(t, add("not-an-id", "nonsense", "not-an-id"), output.ErrValidation)
	requireVerbError(t, add(ra, "nonsense", "not-an-id"), output.ErrValidation, "invalid relation type")
	requireVerbError(t, add(ra, "blocks", "not-an-id"), output.ErrValidation, "invalid target ID")
	_, err := verb.IssueLinkRemove(ctx, env, verb.IssueLinkRemoveInput{Issue: ra, Relation: "blocks", Target: "not-an-id"})
	requireVerbError(t, err, output.ErrValidation, "invalid target ID")

	requireVerbError(t, add(ra, "blocks", "DKT-9999"), output.ErrNotFound, "issue not found")
	requireVerbError(t, add(ra, "blocks", ra), output.ErrValidation, "cannot link an issue to itself")
	if err := add(ra, "blocks", rb); err != nil {
		t.Fatalf("IssueLinkAdd: %v", err)
	}
	requireVerbError(t, add(ra, "blocks", rb), output.ErrConflict, "relation already exists")
	requireCode(t, add(rb, "blocks", ra), output.ErrConflict)

	_, err = verb.IssueLinkRemove(ctx, env, verb.IssueLinkRemoveInput{Issue: rb, Relation: "blocks", Target: ra})
	requireVerbError(t, err, output.ErrNotFound, "relation not found")

	_, err = verb.IssueLinkList(ctx, env, verb.IssueLinkListInput{Issue: "DKT-9999"})
	requireVerbError(t, err, output.ErrNotFound, "issue not found: DKT-9999")
	_, err = verb.IssueLinkList(ctx, env, verb.IssueLinkListInput{Issue: "not-an-id"})
	requireCode(t, err, output.ErrValidation)
}

// IssueLinkList names an id no project owns under the caller's project
// prefix.
func TestIssueLinkListNamesAMissingIssueUnderTheCallersPrefix(t *testing.T) {
	st := openStore(t)
	// The first identity registered claims the default project's row.
	ensureProject(t, st, "/repos/alpha", "alpha")
	beta := ensureProject(t, st, "/repos/beta", "beta")
	if err := st.SetProjectPrefix(beta, "BETA"); err != nil {
		t.Fatalf("SetProjectPrefix: %v", err)
	}

	_, err := verb.IssueLinkList(context.Background(), projectEnv(st, "/repos/beta", true),
		verb.IssueLinkListInput{Issue: "DKT-9999"})
	requireVerbError(t, err, output.ErrNotFound, "issue not found: BETA-9999")
}

// Attaching, detaching and deleting labels and adding and removing links
// write, so they register an anchored caller's project and refuse an
// unanchored one before anything else; listing labels or links is a read.
func TestIssueLabelAndLinkVerbsDeclareTheirProjectPolicy(t *testing.T) {
	st := openStore(t)
	a, b := createIssue(t, st), createIssue(t, st)
	ctx := context.Background()
	ra, rb := model.FormatID(a), model.FormatID(b)
	labels := []string{"alpha"}

	writes := []struct {
		verb string
		call func(env verb.Env, issue string) (*engine.ProjectRegistration, error)
	}{
		{"issue label add", func(env verb.Env, issue string) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueLabelAdd(ctx, env, verb.IssueLabelAddInput{Issue: issue, Labels: labels})
			return res.Registered, err
		}},
		{"issue label rm", func(env verb.Env, issue string) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueLabelRm(ctx, env, verb.IssueLabelRmInput{Issue: issue, Labels: labels})
			return res.Registered, err
		}},
		{"issue label delete", func(env verb.Env, _ string) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueLabelDelete(ctx, env, verb.IssueLabelDeleteInput{Label: "alpha", Force: true})
			return res.Registered, err
		}},
		{"issue link add", func(env verb.Env, issue string) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueLinkAdd(ctx, env, verb.IssueLinkAddInput{Issue: issue, Relation: "blocks", Target: rb})
			return res.Registered, err
		}},
		{"issue link remove", func(env verb.Env, issue string) (*engine.ProjectRegistration, error) {
			res, err := verb.IssueLinkRemove(ctx, env, verb.IssueLinkRemoveInput{Issue: issue, Relation: "blocks", Target: rb})
			return res.Registered, err
		}},
	}

	unanchored := projectEnv(st, "/work/scratch", false)
	for _, w := range writes {
		// The project refusal comes before the issue lookup.
		for _, issue := range []string{ra, "DKT-9999"} {
			_, err := w.call(unanchored, issue)
			requireCode(t, err, output.ErrValidation)
		}
	}
	requireUnregistered(t, st, "/work/scratch")
	if got, _ := st.GetIssueLabelObjects(a); len(got) != 0 {
		t.Errorf("issue has labels %v after unanchored writes, want none", labelNames(got))
	}
	if got, _ := st.GetIssueRelations(a); len(got) != 0 {
		t.Errorf("issue has relations %+v after unanchored writes, want none", got)
	}

	if _, err := verb.IssueLabelList(ctx, projectEnv(st, "/repos/reader", true), verb.IssueLabelListInput{}); err != nil {
		t.Fatalf("IssueLabelList: %v", err)
	}
	if _, err := verb.IssueLinkList(ctx, projectEnv(st, "/repos/reader", true), verb.IssueLinkListInput{Issue: ra}); err != nil {
		t.Fatalf("IssueLinkList: %v", err)
	}
	requireUnregistered(t, st, "/repos/reader")

	// In this order each write succeeds, except that label delete looks the
	// label up in the project it just registered, which has none: it still
	// reports the registration it made before the lookup failed.
	for i, w := range writes {
		identity := fmt.Sprintf("/repos/fresh-%d", i)
		registered, err := w.call(projectEnv(st, identity, true), ra)
		if w.verb == "issue label delete" {
			requireVerbError(t, err, output.ErrNotFound, `label "alpha" not found`)
		} else if err != nil {
			t.Fatalf("%s: %v", w.verb, err)
		}
		if registered == nil || registered.Identity != identity || registered.Verb != w.verb {
			t.Errorf("%s Registered = %+v, want %s registered by %s", w.verb, registered, identity, w.verb)
		}
	}
}
