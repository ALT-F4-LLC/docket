package verb_test

import (
	"context"
	"errors"
	"fmt"
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
