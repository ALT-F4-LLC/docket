package verb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// IssueClaimInput is an issue claim request.
type IssueClaimInput struct {
	Issue string
	Owner string
	// TTL is the lease duration; nil takes the configured TTL for Class.
	TTL   *time.Duration
	Class string
}

// IssueClaimResult is a won claim. Token is the plaintext capability token,
// which exists nowhere else: only its hash is stored.
type IssueClaimResult struct {
	Issue   int
	Token   string
	Lease   model.Lease
	Version int
	// Registered is the project this call registered, if it registered one,
	// for the caller to record its project-registered event. It is set even
	// when IssueClaim also returns an error, because the project stays
	// registered: read it before checking the error.
	Registered *engine.ProjectRegistration
}

// IssueClaim takes the lease on an issue and mints its capability token.
// It writes into the caller's project, so it may register it.
func IssueClaim(ctx context.Context, env Env, in IssueClaimInput) (IssueClaimResult, error) {
	id, err := ParseIssueRef(in.Issue)
	if err != nil {
		return IssueClaimResult{}, err
	}
	if in.Owner == "" {
		return IssueClaimResult{}, newError(
			errors.New("--owner is required: it identifies the lease holder"),
			output.ErrValidation)
	}
	if err := checkTTL(in.TTL); err != nil {
		return IssueClaimResult{}, err
	}

	projectID, registered, err := resolveRegisteringProject(env, "issue claim")
	res := IssueClaimResult{Registered: registered}
	if err != nil {
		return res, err
	}

	ttl, err := leaseTTL(env, projectID, in.TTL, in.Class)
	if err != nil {
		return res, err
	}

	token, lease, err := env.Store.ClaimIssue(id, in.Owner, ttl.Milliseconds(), model.NowMS())
	if err != nil {
		return res, leaseRefusal(env, projectID, id, err, "claiming issue")
	}

	version, err := env.Store.GetVersion("issues", id)
	if err != nil {
		return res, fmt.Errorf("reading issue version: %w", err)
	}

	res.Issue, res.Token, res.Lease, res.Version = id, token, *lease, version
	return res, nil
}

// IssueReleaseInput is an issue release request.
type IssueReleaseInput struct {
	Issue string
	Token string
}

// IssueReleaseResult is the ended lease. Attempt survives the release.
type IssueReleaseResult struct {
	Issue int
	Lease model.Lease
	// Registered is as on IssueClaimResult: set even when IssueRelease also
	// returns an error, so read it before checking the error.
	Registered *engine.ProjectRegistration
}

// IssueRelease ends the lease token holds on an issue and retires the token.
func IssueRelease(ctx context.Context, env Env, in IssueReleaseInput) (IssueReleaseResult, error) {
	id, err := ParseIssueRef(in.Issue)
	if err != nil {
		return IssueReleaseResult{}, err
	}
	if err := checkToken(in.Token); err != nil {
		return IssueReleaseResult{}, err
	}

	projectID, registered, err := resolveRegisteringProject(env, "issue release")
	res := IssueReleaseResult{Registered: registered}
	if err != nil {
		return res, err
	}

	lease, err := env.Store.ReleaseIssue(id, in.Token, model.NowMS())
	if err != nil {
		return res, leaseRefusal(env, projectID, id, err, "releasing lease")
	}

	res.Issue, res.Lease = id, *lease
	return res, nil
}

// IssueHeartbeatInput is an issue heartbeat request.
type IssueHeartbeatInput struct {
	Issue string
	Token string
	// TTL is the extension; nil takes the configured TTL for Class.
	TTL   *time.Duration
	Class string
}

// IssueHeartbeatResult is the extended lease.
type IssueHeartbeatResult struct {
	Issue int
	Lease model.Lease
	// Registered is as on IssueClaimResult: set even when IssueHeartbeat also
	// returns an error, so read it before checking the error.
	Registered *engine.ProjectRegistration
}

// IssueHeartbeat extends the lease token holds on an issue. The attempt
// counter is untouched: a heartbeat is not a new claim.
func IssueHeartbeat(ctx context.Context, env Env, in IssueHeartbeatInput) (IssueHeartbeatResult, error) {
	id, err := ParseIssueRef(in.Issue)
	if err != nil {
		return IssueHeartbeatResult{}, err
	}
	if err := checkToken(in.Token); err != nil {
		return IssueHeartbeatResult{}, err
	}
	if err := checkTTL(in.TTL); err != nil {
		return IssueHeartbeatResult{}, err
	}

	projectID, registered, err := resolveRegisteringProject(env, "issue heartbeat")
	res := IssueHeartbeatResult{Registered: registered}
	if err != nil {
		return res, err
	}

	ttl, err := leaseTTL(env, projectID, in.TTL, in.Class)
	if err != nil {
		return res, err
	}

	lease, err := env.Store.HeartbeatIssue(id, in.Token, ttl.Milliseconds(), model.NowMS())
	if err != nil {
		return res, leaseRefusal(env, projectID, id, err, "extending lease")
	}

	res.Issue, res.Lease = id, *lease
	return res, nil
}

// IssueCreateInput is the issue to create. Description is the text itself,
// never a path or "-". Parent is an issue reference, empty for a root issue.
// Scope is nil when no scope is declared; a non-nil empty list also stores
// none. A replayed IdempotencyKey returns the issue it first created and
// writes nothing to it.
type IssueCreateInput struct {
	Title          string
	Description    string
	Status         string
	Priority       string
	Size           string
	Kind           string
	Assignee       string
	Parent         string
	Labels         []string
	Files          []string
	Scope          *[]string
	IdempotencyKey string
}

// IssueCreateResult is the created issue with its labels, files and docs.
// Registered is as on IssueClaimResult.
type IssueCreateResult struct {
	Issue      *model.Issue
	Registered *engine.ProjectRegistration
}

// IssueCreate creates an issue in the caller's project. It writes into that
// project, so it may register it.
func IssueCreate(ctx context.Context, env Env, in IssueCreateInput) (IssueCreateResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "issue create")
	res := IssueCreateResult{Registered: registered}
	if err != nil {
		return res, err
	}

	if err := model.ValidateStatus(model.Status(in.Status)); err != nil {
		return res, newError(err, output.ErrValidation)
	}
	if err := model.ValidatePriority(model.Priority(in.Priority)); err != nil {
		return res, newError(err, output.ErrValidation)
	}
	// Unlike Priority, whose "none" is itself a valid value, an unset Size is
	// "" (SizeNone), which ValidateSize refuses as a settable value.
	if in.Size != "" {
		if err := model.ValidateSize(model.Size(in.Size)); err != nil {
			return res, newError(err, output.ErrValidation)
		}
	}
	if err := model.ValidateIssueKind(model.IssueKind(in.Kind)); err != nil {
		return res, newError(err, output.ErrValidation)
	}
	scopeJSON, err := optionalScopeGlobsJSON(in.Scope)
	if err != nil {
		return res, err
	}

	var parentID *int
	if in.Parent != "" {
		parent, err := resolveParentIssue(env, in.Parent, projectID)
		if err != nil {
			return res, err
		}
		parentID = &parent.ID
	}

	id, inserted, err := env.Store.CreateIssueIdempotent(&model.Issue{
		ProjectID:   projectID,
		ParentID:    parentID,
		Title:       in.Title,
		Description: in.Description,
		Status:      model.Status(in.Status),
		Priority:    model.Priority(in.Priority),
		Size:        model.Size(in.Size),
		Kind:        model.IssueKind(in.Kind),
		Assignee:    in.Assignee,
	}, in.Labels, in.Files, in.IdempotencyKey)
	if errors.Is(err, store.ErrConflict) {
		return res, newError(err, output.ErrConflict)
	}
	if err != nil {
		return res, newError(fmt.Errorf("creating issue: %w", err), output.ErrGeneral)
	}

	// Scope is written after the insert rather than through it, so a create
	// that declares no scope leaves scope_globs NULL. Only the create's own
	// answer can say whether a key was replayed; a replay writes nothing.
	if inserted && in.Scope != nil {
		if err := env.Store.SetIssueScopeGlobs(id, scopeJSON); err != nil {
			return res, newError(err, output.ErrGeneral)
		}
	}

	created, err := env.Store.GetIssue(id)
	if err != nil {
		return res, newError(fmt.Errorf("fetching created issue: %w", err), output.ErrGeneral)
	}
	if err := hydrateIssue(env, created); err != nil {
		return res, err
	}
	res.Issue = created
	return res, nil
}

// IssueEditInput names an issue and the fields to change; a nil field is left
// as it is. Description is the text itself. A Parent of "0" or "none" makes
// the issue a root. Files replaces the issue's files. Scope replaces its
// scope, and an empty list clears it. IfVersion, when set, makes the field
// update conditional on the issue's version.
type IssueEditInput struct {
	Issue       string
	Title       *string
	Description *string
	Status      *string
	Priority    *string
	Size        *string
	Kind        *string
	Assignee    *string
	Parent      *string
	Files       *[]string
	Scope       *[]string
	IfVersion   *int
}

// IssueEditResult is the issue after the edit, with its labels, files and
// docs. Changed is false when the input named nothing to change. Warnings name
// the live runs whose frozen snapshot the edit did not reach; they are set
// even when IssueEdit also returns an error, as Registered is (see
// IssueClaimResult), so read both before checking the error.
type IssueEditResult struct {
	Issue      *model.Issue
	Changed    bool
	Warnings   []string
	Registered *engine.ProjectRegistration
}

// IssueEdit changes an issue's fields, files and scope, attributing the
// writes to env.Author. It writes into the caller's project, so it may
// register it. Every input is validated before the first write.
func IssueEdit(ctx context.Context, env Env, in IssueEditInput) (IssueEditResult, error) {
	_, registered, err := resolveRegisteringProject(env, "issue edit")
	res := IssueEditResult{Registered: registered}
	if err != nil {
		return res, err
	}

	id, err := ParseIssueRef(in.Issue)
	if err != nil {
		return res, err
	}
	label := "issue " + in.Issue
	issue, err := env.Store.GetIssue(id)
	if err != nil {
		return res, issueFetchError(err, label)
	}

	updates := make(map[string]interface{})
	if in.Title != nil {
		updates["title"] = *in.Title
	}
	if in.Description != nil {
		updates["description"] = *in.Description
	}
	if in.Status != nil {
		if err := model.ValidateStatus(model.Status(*in.Status)); err != nil {
			return res, newError(err, output.ErrValidation)
		}
		updates["status"] = *in.Status
	}
	if in.Priority != nil {
		if err := model.ValidatePriority(model.Priority(*in.Priority)); err != nil {
			return res, newError(err, output.ErrValidation)
		}
		updates["priority"] = *in.Priority
	}
	if in.Size != nil {
		// An empty size clears the declaration back to SizeNone; ValidateSize
		// refuses "" only as a settable value.
		if *in.Size != "" {
			if err := model.ValidateSize(model.Size(*in.Size)); err != nil {
				return res, newError(err, output.ErrValidation)
			}
		}
		updates["size"] = *in.Size
	}
	if in.Kind != nil {
		if err := model.ValidateIssueKind(model.IssueKind(*in.Kind)); err != nil {
			return res, newError(err, output.ErrValidation)
		}
		updates["kind"] = *in.Kind
	}
	if in.Assignee != nil {
		updates["assignee"] = *in.Assignee
	}
	if in.Parent != nil {
		parentID, err := reparentTarget(env, id, issue.ProjectID, *in.Parent)
		if err != nil {
			return res, err
		}
		if parentID == nil {
			updates["parent_id"] = nil
		} else {
			updates["parent_id"] = *parentID
		}
	}
	scopeJSON, err := optionalScopeGlobsJSON(in.Scope)
	if err != nil {
		return res, err
	}

	if in.Files != nil {
		if err := env.Store.SetIssueFiles(id, *in.Files, env.Author); err != nil {
			return res, newError(fmt.Errorf("setting files: %w", err), output.ErrGeneral)
		}
	}
	if in.Scope != nil {
		if err := env.Store.SetIssueScopeGlobs(id, scopeJSON); err != nil {
			return res, newError(err, output.ErrGeneral)
		}
		// The write reached no already-activated run: packets and recorded
		// diffs read the activation snapshot, so each such run is named.
		res.Warnings = engine.ScopeEditFrozenForActiveRunsStore(env.Store, id)
	}

	if len(updates) == 0 && in.Files == nil && in.Scope == nil {
		if err := hydrateIssue(env, issue); err != nil {
			return res, err
		}
		res.Issue = issue
		return res, nil
	}
	res.Changed = true

	if len(updates) > 0 || in.IfVersion != nil {
		if err := env.Store.UpdateIssueCAS(id, updates, env.Author, in.IfVersion); err != nil {
			if e := CASError(err, label); e != nil {
				return res, e
			}
			return res, newError(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
		}
	}

	// The description advisory compares the live column, so it runs only once
	// the update above has landed.
	if in.Description != nil {
		res.Warnings = append(res.Warnings, engine.DescriptionEditFrozenForActiveRunsStore(env.Store, id)...)
	}

	updated, err := env.Store.GetIssue(id)
	if err != nil {
		return res, newError(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
	}
	if err := hydrateIssue(env, updated); err != nil {
		return res, err
	}
	res.Issue = updated
	return res, nil
}

// IssueCloseInput is an issue close request. Token returns the capability
// token; IssueClose calls it only while the issue holds a live lease, so a
// transport whose token channel can block is never read for an unclaimed
// issue. A nil Token presents no token.
type IssueCloseInput struct {
	Issue     string
	IfVersion *int
	Token     func() string
}

// IssueCloseResult is the issue after the close, with its labels, files and
// docs. Changed is false when the issue was already done and nothing was
// written. Registered is as on IssueClaimResult.
type IssueCloseResult struct {
	Issue      *model.Issue
	Changed    bool
	Registered *engine.ProjectRegistration
}

// IssueClose moves an issue to done, ending any live lease on it. A holder
// may always close; a non-holder is refused rather than evicting a working
// holder. It writes into the caller's project, so it may register it.
func IssueClose(ctx context.Context, env Env, in IssueCloseInput) (IssueCloseResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "issue close")
	res := IssueCloseResult{Registered: registered}
	if err != nil {
		return res, err
	}

	id, err := ParseIssueRef(in.Issue)
	if err != nil {
		return res, err
	}
	issue, err := env.Store.GetIssue(id)
	if err != nil {
		return res, issueFetchError(err, "issue "+in.Issue)
	}

	if issue.Status == model.StatusDone {
		if err := hydrateIssue(env, issue); err != nil {
			return res, err
		}
		res.Issue = issue
		return res, nil
	}

	if err := checkIfVersion(in.IfVersion); err != nil {
		return res, err
	}

	// The lease look is advisory: the authoritative check runs inside the
	// update transaction, where a lease claimed after this read still refuses
	// the empty token instead of being evicted.
	token := ""
	if lease, err := env.Store.GetIssueLease(id); err == nil && lease.Live(model.NowMS()) && in.Token != nil {
		token = in.Token()
	}

	err = env.Store.UpdateIssueCASLease(id, map[string]interface{}{"status": "done"}, env.Author, in.IfVersion, token)
	if err != nil {
		label := "issue " + issueDisplayID(env, projectID, id)
		if e := LeaseError(err, label); e != nil {
			return res, e
		}
		if e := CASError(err, label); e != nil {
			return res, e
		}
		return res, newError(fmt.Errorf("closing issue: %w", err), output.ErrGeneral)
	}

	res.Changed = true
	res.Issue, err = refetchIssue(env, id)
	return res, err
}

// IssueReopenInput is an issue reopen request.
type IssueReopenInput struct {
	Issue     string
	IfVersion *int
}

// IssueReopenResult is the issue after the reopen, with its labels, files and
// docs. Changed is false when the issue was not closed and nothing was
// written. Registered is as on IssueClaimResult.
type IssueReopenResult struct {
	Issue      *model.Issue
	Changed    bool
	Registered *engine.ProjectRegistration
}

// IssueReopen moves a closed issue back to backlog and clears its resolution:
// the issue is back on the board, so a recorded abandonment no longer
// describes it. It writes into the caller's project, so it may register it.
func IssueReopen(ctx context.Context, env Env, in IssueReopenInput) (IssueReopenResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "issue reopen")
	res := IssueReopenResult{Registered: registered}
	if err != nil {
		return res, err
	}

	id, err := ParseIssueRef(in.Issue)
	if err != nil {
		return res, err
	}
	issue, err := env.Store.GetIssue(id)
	if err != nil {
		return res, issueFetchError(err, "issue "+in.Issue)
	}

	if issue.Status != model.StatusDone {
		if err := hydrateIssue(env, issue); err != nil {
			return res, err
		}
		res.Issue = issue
		return res, nil
	}

	if err := checkIfVersion(in.IfVersion); err != nil {
		return res, err
	}

	fields := map[string]interface{}{"status": "backlog", "resolution": ""}
	if err := env.Store.UpdateIssueCAS(id, fields, env.Author, in.IfVersion); err != nil {
		if e := CASError(err, "issue "+issueDisplayID(env, projectID, id)); e != nil {
			return res, e
		}
		return res, newError(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
	}

	res.Changed = true
	res.Issue, err = refetchIssue(env, id)
	return res, err
}

// IssueMoveInput is an issue move request: a status move when Status is
// given, a project migration when Project is set, never both. A given empty
// Status is an invalid status, not an absent one. Note records the reason for
// a status move as a comment in the move's own transaction. IfVersion and
// Note apply to status moves only.
type IssueMoveInput struct {
	Issue     string
	Status    *string
	Project   string
	Note      string
	IfVersion *int
}

// IssueMoveResult is the outcome of a move. A status move sets Issue, with its
// labels, files and docs, and From, its status before the move; Changed is
// false when the issue was already at the status, and Issue is then the issue
// as read before any --if-version or --note write. A project migration sets
// Migration instead. Registered is as on IssueClaimResult.
type IssueMoveResult struct {
	Issue      *model.Issue
	From       model.Status
	Changed    bool
	Migration  *IssueMigration
	Registered *engine.ProjectRegistration
}

// IssueMigration is a migrated subtree: its root, the project it now lives in,
// and every issue that moved with it.
type IssueMigration struct {
	Root     int
	Project  *model.Project
	Migrated []int
}

// IssueMove moves an issue to a new status, or migrates a root issue and its
// whole sub-issue tree to another project. It writes into the caller's
// project, so it may register it.
func IssueMove(ctx context.Context, env Env, in IssueMoveInput) (IssueMoveResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "issue move")
	res := IssueMoveResult{Registered: registered}
	if err != nil {
		return res, err
	}

	id, err := ParseIssueRef(in.Issue)
	if err != nil {
		return res, err
	}

	if in.Project != "" {
		switch {
		case in.Status != nil:
			return res, newError(errors.New(
				"a status move and a project migration are separate operations; "+
					"pass a status or --project, not both"), output.ErrValidation)
		case in.IfVersion != nil:
			return res, newError(errors.New(
				"--if-version applies to status moves only"), output.ErrValidation)
		// Refused rather than dropped: a migration records no note, and a
		// note silently discarded is worse than one that never landed.
		case in.Note != "":
			return res, newError(errors.New(
				"--note applies to status moves only"), output.ErrValidation)
		}
		res.Migration, err = migrateIssue(env, projectID, id, in.Project)
		return res, err
	}
	if in.Status == nil {
		return res, newError(errors.New(
			"usage: docket issue move <id> <status>, or docket issue move <id> --project <target>"),
			output.ErrValidation)
	}

	newStatus := model.Status(*in.Status)
	if err := model.ValidateStatus(newStatus); err != nil {
		return res, newError(err, output.ErrValidation)
	}
	issue, err := env.Store.GetIssue(id)
	if err != nil {
		return res, issueFetchError(err, "issue "+in.Issue)
	}
	if err := checkIfVersion(in.IfVersion); err != nil {
		return res, err
	}
	res.From = issue.Status

	if issue.Status == newStatus {
		// A no-op move still honors IfVersion, so a caller cannot read a stale
		// version and conclude its precondition held, and still records Note,
		// as the comment-then-move pair it replaces would have.
		if in.IfVersion != nil || in.Note != "" {
			if err := env.Store.UpdateIssueCASNote(id, nil, env.Author, in.IfVersion, in.Note); err != nil {
				if e := CASError(err, "issue "+issueDisplayID(env, projectID, id)); e != nil {
					return res, e
				}
				return res, newError(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
			}
		}
		if err := hydrateIssue(env, issue); err != nil {
			return res, err
		}
		res.Issue = issue
		return res, nil
	}

	fields := map[string]interface{}{"status": string(newStatus)}
	if err := env.Store.UpdateIssueCASNote(id, fields, env.Author, in.IfVersion, in.Note); err != nil {
		if e := CASError(err, "issue "+issueDisplayID(env, projectID, id)); e != nil {
			return res, e
		}
		return res, newError(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
	}

	res.Changed = true
	res.Issue, err = refetchIssue(env, id)
	return res, err
}

// migrateIssue re-homes root issue id and its sub-issue tree to the project
// target names.
func migrateIssue(env Env, projectID, id int, target string) (*IssueMigration, error) {
	project, err := ResolveProjectRef(env.Store, target)
	if err != nil {
		return nil, err
	}

	ids, err := env.Store.MoveIssueProject(id, project.ID, env.Author, time.Now().UnixMilli())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, newError(fmt.Errorf("issue %s not found", issueDisplayID(env, projectID, id)), output.ErrNotFound)
	case errors.Is(err, store.ErrIssueHasParent):
		return nil, newError(fmt.Errorf(
			"issue %s has a parent; a sub-issue migrates with its root — migrate "+
				"the root, or reparent with `issue edit --parent none` first",
			issueDisplayID(env, projectID, id)), output.ErrValidation)
	case errors.Is(err, store.ErrIssueInRun):
		return nil, newError(fmt.Errorf(
			"issue %s (or one of its sub-issues) belongs to a run; a run's records "+
				"are project-scoped and would be stranded", issueDisplayID(env, projectID, id)),
			output.ErrConflict)
	case err != nil:
		return nil, newError(fmt.Errorf("migrating issue: %w", err), output.ErrGeneral)
	}
	return &IssueMigration{Root: id, Project: project, Migrated: ids}, nil
}

// ErrIssueHasSubIssues is the refusal IssueDelete wraps when the issue has
// sub-issues and the input chose neither Cascade nor Orphan.
var ErrIssueHasSubIssues = errors.New(
	"use --force to cascade-delete or --orphan to make them root issues")

// IssueHasSubIssuesError is IssueDelete's refusal of an issue with
// sub-issues: it names the issue and counts its sub-issues for a caller that
// prompts for the choice, and it matches ErrIssueHasSubIssues.
type IssueHasSubIssuesError struct {
	Issue     string
	SubIssues int
}

func (e *IssueHasSubIssuesError) Error() string {
	return fmt.Sprintf("issue %s has %d sub-issue(s): %v", e.Issue, e.SubIssues, ErrIssueHasSubIssues)
}

func (e *IssueHasSubIssuesError) Unwrap() error { return ErrIssueHasSubIssues }

// IssueDeleteInput is an issue delete request. Cascade deletes the issue's
// sub-issues with it; Orphan makes them root issues first. Either is needed
// only when the issue has sub-issues.
type IssueDeleteInput struct {
	Issue   string
	Cascade bool
	Orphan  bool
}

// IssueDeleteResult describes the issue IssueDelete deleted. Issue is its
// display id, rendered before the delete while its owning project can still
// be read. SubIssues counts the sub-issues it cascaded to or orphaned. On any
// error nothing is reported deleted and only Registered may be set; it is as
// on IssueClaimResult.
type IssueDeleteResult struct {
	Issue      string
	Title      string
	SubIssues  int
	Registered *engine.ProjectRegistration
}

// IssueDelete deletes an issue, cascading to or orphaning its sub-issues as
// the input chooses. It writes into the caller's project, so it may register
// it.
func IssueDelete(ctx context.Context, env Env, in IssueDeleteInput) (IssueDeleteResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "issue delete")
	failed := IssueDeleteResult{Registered: registered}
	if err != nil {
		return failed, err
	}

	if in.Cascade && in.Orphan {
		return failed, newError(errors.New("--force and --orphan are mutually exclusive"), output.ErrValidation)
	}
	id, err := model.ParseID(in.Issue)
	if err != nil {
		return failed, newError(err, output.ErrValidation)
	}

	issue, err := env.Store.GetIssue(id)
	if err != nil {
		return failed, issueFetchError(err, "issue "+issueDisplayID(env, projectID, id))
	}
	subIssues, err := env.Store.GetSubIssues(id)
	if err != nil {
		return failed, newError(fmt.Errorf("checking sub-issues: %w", err), output.ErrGeneral)
	}
	deleted := IssueDeleteResult{
		Issue: issueDisplayID(env, projectID, id), Title: issue.Title,
		SubIssues: len(subIssues), Registered: registered,
	}

	switch {
	case deleted.SubIssues == 0:
	case in.Cascade:
		if err := env.Store.CascadeDeleteIssue(id); err != nil {
			return failed, newError(fmt.Errorf("cascade deleting issue: %w", err), output.ErrGeneral)
		}
		return deleted, nil
	case in.Orphan:
		if err := env.Store.OrphanSubIssues(id, env.Author); err != nil {
			return failed, newError(fmt.Errorf("orphaning sub-issues: %w", err), output.ErrGeneral)
		}
	default:
		return failed, newError(&IssueHasSubIssuesError{Issue: deleted.Issue, SubIssues: deleted.SubIssues},
			output.ErrValidation)
	}
	if err := env.Store.DeleteIssue(id); err != nil {
		return failed, newError(fmt.Errorf("deleting issue: %w", err), output.ErrGeneral)
	}
	return deleted, nil
}

// IssueShowInput names the issues to show, each by reference.
type IssueShowInput struct {
	Issues []string
}

// IssueDetail is one issue at full detail: the issue with its labels, files
// and docs, and the collections around it. Activity holds the latest ten
// entries. RunDisposition is the last terminal ruling a run made about the
// issue, nil when no run abandoned it. ScopeDeclared distinguishes an issue
// that declares no scope from one whose declared scope is empty.
type IssueDetail struct {
	Issue           *model.Issue
	SubIssues       []*model.Issue
	Relations       []model.Relation
	LinkedProposals []model.Proposal
	Comments        []*model.Comment
	Activity        []model.Activity
	RunDisposition  *model.IssueRunDisposition
	Scope           []string
	ScopeDeclared   bool
}

// IssueShowResult holds one detail per requested issue, in request order.
type IssueShowResult struct {
	Issues []IssueDetail
}

// IssueShow reads each requested issue at full detail, stopping at the first
// that fails. Issue ids are store-wide, so it is a read that names no project.
func IssueShow(ctx context.Context, env Env, in IssueShowInput) (IssueShowResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return IssueShowResult{}, err
	}
	res := IssueShowResult{Issues: make([]IssueDetail, 0, len(in.Issues))}
	for _, ref := range in.Issues {
		detail, err := issueDetail(env, ref)
		if err != nil {
			return IssueShowResult{}, err
		}
		res.Issues = append(res.Issues, detail)
	}
	return res, nil
}

func issueDetail(env Env, ref string) (IssueDetail, error) {
	id, err := ParseIssueRef(ref)
	if err != nil {
		return IssueDetail{}, err
	}
	issue, err := env.Store.GetIssue(id)
	if err != nil {
		return IssueDetail{}, issueFetchError(err, "issue "+ref)
	}
	if err := hydrateIssueNamingDocs(env, issue, "linked docs"); err != nil {
		return IssueDetail{}, err
	}

	d := IssueDetail{Issue: issue}
	if d.SubIssues, err = env.Store.GetSubIssues(id); err != nil {
		return IssueDetail{}, newError(fmt.Errorf("fetching sub-issues: %w", err), output.ErrGeneral)
	}
	if d.Relations, err = env.Store.GetIssueRelations(id); err != nil {
		return IssueDetail{}, newError(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
	}
	if d.LinkedProposals, err = env.Store.GetIssueProposals(id); err != nil {
		return IssueDetail{}, newError(fmt.Errorf("fetching linked proposals: %w", err), output.ErrGeneral)
	}
	if d.Comments, err = env.Store.ListComments(id); err != nil {
		return IssueDetail{}, newError(fmt.Errorf("fetching comments: %w", err), output.ErrGeneral)
	}
	if d.Activity, err = env.Store.GetActivity(id, 10); err != nil {
		return IssueDetail{}, newError(fmt.Errorf("fetching activity: %w", err), output.ErrGeneral)
	}
	if d.RunDisposition, err = engine.LatestIssueDispositionStore(env.Store, id); err != nil {
		return IssueDetail{}, newError(fmt.Errorf("fetching the run disposition: %w", err), output.ErrGeneral)
	}

	// scope_globs holds a JSON array, or NULL, read as "", when none is declared.
	scopeJSON, err := env.Store.IssueScopeGlobs(id)
	if err != nil {
		return IssueDetail{}, newError(fmt.Errorf("fetching scope: %w", err), output.ErrGeneral)
	}
	d.ScopeDeclared = scopeJSON != ""
	if d.ScopeDeclared {
		if err := json.Unmarshal([]byte(scopeJSON), &d.Scope); err != nil {
			return IssueDetail{}, newError(fmt.Errorf("decoding scope: %w", err), output.ErrGeneral)
		}
	}
	return d, nil
}

// IssueCommentTargetInput names the issue a comment would be added to.
type IssueCommentTargetInput struct {
	Issue string
}

// IssueCommentTargetResult is the issue a comment would be added to.
// Registered is as on IssueClaimResult.
type IssueCommentTargetResult struct {
	Issue      *model.Issue
	Registered *engine.ProjectRegistration
}

// IssueCommentTarget runs every check IssueCommentAdd makes before it writes:
// the project, the issue reference and the issue's existence. A caller that
// has to ask a person for the comment body calls it first, so a comment that
// would be refused is refused before anyone types it. It resolves the project
// as IssueCommentAdd does, so it may register the caller's project.
func IssueCommentTarget(ctx context.Context, env Env, in IssueCommentTargetInput) (IssueCommentTargetResult, error) {
	_, registered, err := resolveRegisteringProject(env, "issue comment add")
	res := IssueCommentTargetResult{Registered: registered}
	if err != nil {
		return res, err
	}
	_, res.Issue, err = issueByRef(env, in.Issue)
	return res, err
}

// issueByRef parses ref and fetches the issue it names.
func issueByRef(env Env, ref string) (int, *model.Issue, error) {
	id, err := ParseIssueRef(ref)
	if err != nil {
		return 0, nil, err
	}
	issue, err := env.Store.GetIssue(id)
	if err != nil {
		return 0, nil, issueFetchError(err, "issue "+ref)
	}
	return id, issue, nil
}

// IssueCommentAddInput is the comment to add to Issue. A replayed
// IdempotencyKey returns the comment it first created.
type IssueCommentAddInput struct {
	Issue          string
	Body           string
	IdempotencyKey string
}

// IssueCommentAddResult is the commented issue and the stored comment.
// Registered is as on IssueClaimResult.
type IssueCommentAddResult struct {
	Issue      *model.Issue
	Comment    *model.Comment
	Registered *engine.ProjectRegistration
}

// IssueCommentAdd adds a comment authored by env.Author to an issue. It writes
// into the caller's project, so it may register it.
func IssueCommentAdd(ctx context.Context, env Env, in IssueCommentAddInput) (IssueCommentAddResult, error) {
	_, registered, err := resolveRegisteringProject(env, "issue comment add")
	res := IssueCommentAddResult{Registered: registered}
	if err != nil {
		return res, err
	}
	id, issue, err := issueByRef(env, in.Issue)
	if err != nil {
		return res, err
	}
	res.Issue = issue

	commentID, err := env.Store.CreateCommentIdempotent(&model.Comment{
		IssueID: id,
		Body:    in.Body,
		Author:  env.Author,
	}, in.IdempotencyKey)
	if err != nil {
		if e := notFound(err, "issue "+in.Issue); e != nil {
			return res, e
		}
		if errors.Is(err, store.ErrConflict) {
			return res, newError(err, output.ErrConflict)
		}
		return res, newError(fmt.Errorf("creating comment: %w", err), output.ErrGeneral)
	}

	if res.Comment, err = env.Store.GetComment(commentID); err != nil {
		return res, newError(fmt.Errorf("fetching created comment: %w", err), output.ErrGeneral)
	}
	return res, nil
}

// IssueCommentListInput names the issue whose comments to list.
type IssueCommentListInput struct {
	Issue string
}

// IssueCommentListResult is an issue's comments, oldest first.
type IssueCommentListResult struct {
	IssueID  int
	Comments []*model.Comment
}

// IssueCommentList lists the comments on one issue. Issue ids are store-wide,
// so it is a read that names no project.
func IssueCommentList(ctx context.Context, env Env, in IssueCommentListInput) (IssueCommentListResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return IssueCommentListResult{}, err
	}
	id, _, err := issueByRef(env, in.Issue)
	if err != nil {
		return IssueCommentListResult{}, err
	}
	comments, err := env.Store.ListComments(id)
	if err != nil {
		return IssueCommentListResult{}, newError(fmt.Errorf("fetching comments: %w", err), output.ErrGeneral)
	}
	return IssueCommentListResult{IssueID: id, Comments: comments}, nil
}

// IssueLogInput names the issue whose activity to read. Limit caps the
// entries; a Limit of 0 reads one entry, as the CLI's v1 contract has always
// clamped it, and a negative Limit is refused.
type IssueLogInput struct {
	Issue string
	Limit int
}

// IssueLogResult is an issue's latest activity, newest first. Total counts
// every entry before Limit applied, and Limit is the cap that applied.
type IssueLogResult struct {
	IssueID int
	Entries []model.Activity
	Total   int
	Limit   int
}

// IssueLog reads an issue's activity history. Issue ids are store-wide, so it
// is a read that names no project.
func IssueLog(ctx context.Context, env Env, in IssueLogInput) (IssueLogResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return IssueLogResult{}, err
	}
	id, _, err := issueByRef(env, in.Issue)
	if err != nil {
		return IssueLogResult{}, err
	}
	if in.Limit < 0 {
		return IssueLogResult{}, newError(
			fmt.Errorf("--limit must be >= 0, got %d (0 means no limit)", in.Limit), output.ErrValidation)
	}
	limit := max(in.Limit, 1)

	entries, err := env.Store.GetActivity(id, limit)
	if err != nil {
		return IssueLogResult{}, newError(fmt.Errorf("fetching activity: %w", err), output.ErrGeneral)
	}
	total, err := env.Store.CountActivity(id)
	if err != nil {
		return IssueLogResult{}, newError(fmt.Errorf("counting activity: %w", err), output.ErrGeneral)
	}
	return IssueLogResult{IssueID: id, Entries: entries, Total: total, Limit: limit}, nil
}

// IssueFileAddInput names the issue and the file paths to attach to it.
type IssueFileAddInput struct {
	Issue string
	Paths []string
}

// IssueFileAddResult is the issue and its full file list after the attach.
// Registered is as on IssueClaimResult.
type IssueFileAddResult struct {
	Issue      *model.Issue
	Files      []string
	Registered *engine.ProjectRegistration
}

// IssueFileAdd attaches files to an issue as env.Author. It writes into the
// caller's project, so it may register it.
func IssueFileAdd(ctx context.Context, env Env, in IssueFileAddInput) (IssueFileAddResult, error) {
	_, registered, err := resolveRegisteringProject(env, "issue file add")
	res := IssueFileAddResult{Registered: registered}
	if err != nil {
		return res, err
	}
	id, issue, err := issueByRef(env, in.Issue)
	if err != nil {
		return res, err
	}
	res.Issue = issue
	if err := env.Store.AttachFiles(id, in.Paths, env.Author); err != nil {
		return res, newError(fmt.Errorf("attaching files: %w", err), output.ErrGeneral)
	}
	res.Files, err = issueFiles(env, id)
	return res, err
}

// IssueFileRemoveInput names the issue and the file paths to detach from it.
type IssueFileRemoveInput struct {
	Issue string
	Paths []string
}

// IssueFileRemoveResult is the issue and its full file list after the
// detach. Registered is as on IssueClaimResult.
type IssueFileRemoveResult struct {
	Issue      *model.Issue
	Files      []string
	Registered *engine.ProjectRegistration
}

// IssueFileRemove detaches files from an issue as env.Author. It writes into
// the caller's project, so it may register it.
func IssueFileRemove(ctx context.Context, env Env, in IssueFileRemoveInput) (IssueFileRemoveResult, error) {
	_, registered, err := resolveRegisteringProject(env, "issue file remove")
	res := IssueFileRemoveResult{Registered: registered}
	if err != nil {
		return res, err
	}
	id, issue, err := issueByRef(env, in.Issue)
	if err != nil {
		return res, err
	}
	res.Issue = issue
	if err := env.Store.DetachFiles(id, in.Paths, env.Author); err != nil {
		return res, newError(fmt.Errorf("removing files: %w", err), output.ErrGeneral)
	}
	res.Files, err = issueFiles(env, id)
	return res, err
}

// IssueFileListInput names the issue whose attached files to list.
type IssueFileListInput struct {
	Issue string
}

// IssueFileListResult is an issue's attached file paths.
type IssueFileListResult struct {
	IssueID int
	Files   []string
}

// IssueFileList lists the files attached to one issue. Issue ids are
// store-wide, so it is a read that names no project.
func IssueFileList(ctx context.Context, env Env, in IssueFileListInput) (IssueFileListResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return IssueFileListResult{}, err
	}
	id, _, err := issueByRef(env, in.Issue)
	if err != nil {
		return IssueFileListResult{}, err
	}
	files, err := issueFiles(env, id)
	if err != nil {
		return IssueFileListResult{}, err
	}
	return IssueFileListResult{IssueID: id, Files: files}, nil
}

func issueFiles(env Env, id int) ([]string, error) {
	files, err := env.Store.GetIssueFiles(id)
	if err != nil {
		return nil, newError(fmt.Errorf("fetching files: %w", err), output.ErrGeneral)
	}
	return files, nil
}

// IssueLabelAddInput names the issue and the labels to attach to it. Color
// applies to labels this call creates.
type IssueLabelAddInput struct {
	Issue  string
	Labels []string
	Color  string
}

// IssueLabelAddResult is the issue and its full label set after the attach.
// Registered is as on IssueClaimResult.
type IssueLabelAddResult struct {
	Issue      *model.Issue
	Labels     []*model.Label
	Registered *engine.ProjectRegistration
}

// IssueLabelAdd attaches labels to an issue as env.Author, creating any label
// that does not exist yet. It writes into the caller's project, so it may
// register it.
func IssueLabelAdd(ctx context.Context, env Env, in IssueLabelAddInput) (IssueLabelAddResult, error) {
	_, registered, err := resolveRegisteringProject(env, "issue label add")
	res := IssueLabelAddResult{Registered: registered}
	if err != nil {
		return res, err
	}
	id, issue, err := issueByRef(env, in.Issue)
	if err != nil {
		return res, err
	}
	res.Issue = issue
	if err := checkLabelNames(in.Labels); err != nil {
		return res, err
	}

	if err := env.Store.AddLabelsToIssue(id, in.Labels, in.Color, env.Author); err != nil {
		if e := notFound(err, "issue "+in.Issue); e != nil {
			return res, e
		}
		if errors.Is(err, store.ErrLabelColorConflict) {
			return res, newError(errors.New("label already exists with a different color"), output.ErrValidation)
		}
		return res, newError(fmt.Errorf("adding labels: %w", err), output.ErrGeneral)
	}
	res.Labels, err = issueLabels(env, id)
	return res, err
}

// IssueLabelRmInput names the issue and the labels to detach from it.
type IssueLabelRmInput struct {
	Issue  string
	Labels []string
}

// IssueLabelRmResult is the issue and its full label set after the detach.
// Registered is as on IssueClaimResult.
type IssueLabelRmResult struct {
	Issue      *model.Issue
	Labels     []*model.Label
	Registered *engine.ProjectRegistration
}

// IssueLabelRm detaches labels from an issue as env.Author. It writes into
// the caller's project, so it may register it.
func IssueLabelRm(ctx context.Context, env Env, in IssueLabelRmInput) (IssueLabelRmResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "issue label rm")
	res := IssueLabelRmResult{Registered: registered}
	if err != nil {
		return res, err
	}
	id, issue, err := issueByRef(env, in.Issue)
	if err != nil {
		return res, err
	}
	res.Issue = issue
	if err := checkLabelNames(in.Labels); err != nil {
		return res, err
	}

	if err := env.Store.RemoveLabelsFromIssue(id, in.Labels, env.Author); err != nil {
		if e := notFound(err, "issue or label"); e != nil {
			return res, e
		}
		if errors.Is(err, store.ErrNotAttached) {
			return res, newError(fmt.Errorf("label is not attached to %s", issueDisplayID(env, projectID, id)), output.ErrValidation)
		}
		return res, newError(fmt.Errorf("removing labels: %w", err), output.ErrGeneral)
	}
	res.Labels, err = issueLabels(env, id)
	return res, err
}

func checkLabelNames(names []string) error {
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return newError(errors.New("label name cannot be empty"), output.ErrValidation)
		}
	}
	return nil
}

func issueLabels(env Env, id int) ([]*model.Label, error) {
	labels, err := env.Store.GetIssueLabelObjects(id)
	if err != nil {
		return nil, newError(fmt.Errorf("fetching labels: %w", err), output.ErrGeneral)
	}
	return labels, nil
}

// IssueLabelListInput is a request for the caller's project's labels.
type IssueLabelListInput struct{}

// IssueLabelListResult is every label in the caller's project, each with the
// number of issues it is attached to.
type IssueLabelListResult struct {
	Labels []*model.LabelWithCount
}

// IssueLabelList lists the caller's project's labels. It is a read, so an
// unregistered project lists none and registers nothing.
func IssueLabelList(ctx context.Context, env Env, in IssueLabelListInput) (IssueLabelListResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return IssueLabelListResult{}, err
	}
	labels, err := env.Store.ListAllLabels(projectID)
	if err != nil {
		return IssueLabelListResult{}, newError(fmt.Errorf("listing labels: %w", err), output.ErrGeneral)
	}
	return IssueLabelListResult{Labels: labels}, nil
}

// IssueLabelDeleteInput names the caller's project's label to delete. Force
// consents to deleting a label still attached to issues.
type IssueLabelDeleteInput struct {
	Label string
	Force bool
}

// ErrLabelInUse is the refusal IssueLabelDelete wraps when the label is still
// attached to issues and the input did not set Force. With
// ErrLabelInUse the result's Label is set and nothing was deleted.
var ErrLabelInUse = errors.New("use --force to delete it")

// IssueLabelDeleteResult is the label the call looked up and how many issues
// the delete detached it from. Label is set whenever the lookup succeeded,
// including when the delete was refused for want of Force. Registered is as
// on IssueClaimResult.
type IssueLabelDeleteResult struct {
	Label      *model.LabelWithCount
	Detached   int
	Registered *engine.ProjectRegistration
}

// IssueLabelDelete deletes a label from the caller's project as env.Author,
// detaching it from every issue. A label still attached to an issue is
// refused unless Force is set. It writes into the caller's project, so it may
// register it.
func IssueLabelDelete(ctx context.Context, env Env, in IssueLabelDeleteInput) (IssueLabelDeleteResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "issue label delete")
	res := IssueLabelDeleteResult{Registered: registered}
	if err != nil {
		return res, err
	}
	label, err := env.Store.GetLabelByName(projectID, in.Label)
	if err != nil {
		if e := notFound(err, fmt.Sprintf("label %q", in.Label)); e != nil {
			return res, e
		}
		return res, newError(fmt.Errorf("fetching label: %w", err), output.ErrGeneral)
	}
	res.Label = label

	if !in.Force && label.IssueCount > 0 {
		return res, newError(fmt.Errorf("label %q is attached to %d issue(s): %w",
			in.Label, label.IssueCount, ErrLabelInUse), output.ErrValidation)
	}

	affected, err := env.Store.DeleteLabel(label.ID, in.Label, env.Author)
	if err != nil {
		return res, newError(fmt.Errorf("deleting label: %w", err), output.ErrGeneral)
	}
	res.Detached = len(affected)
	return res, nil
}

// IssueLinkAddInput is a relation to create: Issue Relation Target, such as
// DKT-1 blocks DKT-2.
type IssueLinkAddInput struct {
	Issue    string
	Relation string
	Target   string
}

// IssueLinkAddResult is the stored relation. Registered is as on
// IssueClaimResult.
type IssueLinkAddResult struct {
	Relation   *model.Relation
	Registered *engine.ProjectRegistration
}

// IssueLinkAdd creates a relation between two issues. It writes into the
// caller's project, so it may register it.
func IssueLinkAdd(ctx context.Context, env Env, in IssueLinkAddInput) (IssueLinkAddResult, error) {
	_, registered, err := resolveRegisteringProject(env, "issue link add")
	res := IssueLinkAddResult{Registered: registered}
	if err != nil {
		return res, err
	}
	rel, err := parseRelation(in.Issue, in.Relation, in.Target)
	if err != nil {
		return res, err
	}

	relID, err := env.Store.CreateRelation(rel)
	if err != nil {
		if e := notFound(err, "issue"); e != nil {
			return res, e
		}
		switch {
		case errors.Is(err, store.ErrSelfRelation):
			return res, newError(errors.New("cannot link an issue to itself"), output.ErrValidation)
		case errors.Is(err, store.ErrDuplicateRelation):
			return res, newError(errors.New("relation already exists"), output.ErrConflict)
		case errors.Is(err, store.ErrCycleDetected):
			return res, newError(err, output.ErrConflict)
		}
		return res, newError(fmt.Errorf("creating relation: %w", err), output.ErrGeneral)
	}
	rel.ID = relID
	res.Relation = rel
	return res, nil
}

// IssueLinkRemoveInput is the relation to remove, spelled as on
// IssueLinkAddInput.
type IssueLinkRemoveInput struct {
	Issue    string
	Relation string
	Target   string
}

// IssueLinkRemoveResult is the removed relation, without its row id.
// Registered is as on IssueClaimResult.
type IssueLinkRemoveResult struct {
	Relation   *model.Relation
	Registered *engine.ProjectRegistration
}

// IssueLinkRemove removes a relation between two issues. It writes into the
// caller's project, so it may register it.
func IssueLinkRemove(ctx context.Context, env Env, in IssueLinkRemoveInput) (IssueLinkRemoveResult, error) {
	_, registered, err := resolveRegisteringProject(env, "issue link remove")
	res := IssueLinkRemoveResult{Registered: registered}
	if err != nil {
		return res, err
	}
	rel, err := parseRelation(in.Issue, in.Relation, in.Target)
	if err != nil {
		return res, err
	}

	if err := env.Store.DeleteRelation(rel.SourceIssueID, rel.TargetIssueID, string(rel.RelationType)); err != nil {
		if e := notFound(err, "relation"); e != nil {
			return res, e
		}
		return res, newError(fmt.Errorf("deleting relation: %w", err), output.ErrGeneral)
	}
	res.Relation = rel
	return res, nil
}

// parseRelation parses a relation's source issue, type and target issue, in
// that order, so the first malformed one is the one reported.
func parseRelation(issue, relation, target string) (*model.Relation, error) {
	sourceID, err := ParseIssueRef(issue)
	if err != nil {
		return nil, err
	}
	relType, err := model.ParseRelationType(relation)
	if err != nil {
		return nil, newError(err, output.ErrValidation)
	}
	targetID, err := model.ParseID(target)
	if err != nil {
		return nil, newError(fmt.Errorf("invalid target ID: %w", err), output.ErrValidation)
	}
	return &model.Relation{SourceIssueID: sourceID, TargetIssueID: targetID, RelationType: relType}, nil
}

// IssueLinkListInput names the issue whose relations to list.
type IssueLinkListInput struct {
	Issue string
}

// IssueLinkListResult is every relation the issue takes part in, as source or
// as target.
type IssueLinkListResult struct {
	IssueID   int
	Relations []model.Relation
}

// IssueLinkList lists one issue's relations. Issue ids are store-wide, so it
// is a read that names no project.
func IssueLinkList(ctx context.Context, env Env, in IssueLinkListInput) (IssueLinkListResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return IssueLinkListResult{}, err
	}
	id, err := ParseIssueRef(in.Issue)
	if err != nil {
		return IssueLinkListResult{}, err
	}
	exists, err := env.Store.IssueExists(id)
	if err != nil {
		return IssueLinkListResult{}, newError(fmt.Errorf("checking issue: %w", err), output.ErrGeneral)
	}
	if !exists {
		return IssueLinkListResult{}, newError(
			fmt.Errorf("issue not found: %s", issueDisplayID(env, projectID, id)), output.ErrNotFound)
	}
	relations, err := env.Store.GetIssueRelations(id)
	if err != nil {
		return IssueLinkListResult{}, newError(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
	}
	return IssueLinkListResult{IssueID: id, Relations: relations}, nil
}

// IssueGraphInput names the focal issue of a dependency graph. Direction is
// up (its blockers), down (what it blocks) or both; Depth caps the hops from
// the focal issue, and 0 means unlimited.
type IssueGraphInput struct {
	Issue     string
	Direction string
	Depth     int
}

// IssueGraphEdge is one dependency in a graph: From blocks To. Type is always
// "blocks", whichever relation recorded it.
type IssueGraphEdge struct {
	From int
	To   int
	Type string
}

// IssueGraphResult is the dependency graph around Issue. Issues holds every
// issue the walk reached, the focal one included, keyed by id; Edges lists
// the dependencies in walk order, down before up. Forward and Backward are
// the blocks and blocked-by adjacency the walk followed, keyed by reached
// issues only; a list can still name a neighbor the walk did not reach.
type IssueGraphResult struct {
	Issue    *model.Issue
	Issues   map[int]*model.Issue
	Edges    []IssueGraphEdge
	Forward  map[int][]int
	Backward map[int][]int
}

// IssueGraph walks the dependency graph from one issue. Issue ids are
// store-wide, so it is a read that names no project.
func IssueGraph(ctx context.Context, env Env, in IssueGraphInput) (IssueGraphResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return IssueGraphResult{}, err
	}
	id, issue, err := issueByRef(env, in.Issue)
	if err != nil {
		return IssueGraphResult{}, err
	}
	if in.Direction != "up" && in.Direction != "down" && in.Direction != "both" {
		return IssueGraphResult{}, newError(fmt.Errorf(
			"invalid direction %q: must be one of [up, down, both]", in.Direction), output.ErrValidation)
	}
	if in.Depth < 0 {
		return IssueGraphResult{}, newError(errors.New("depth must be non-negative"), output.ErrValidation)
	}

	relations, err := env.Store.GetAllDirectionalRelations()
	if err != nil {
		return IssueGraphResult{}, newError(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
	}
	res := IssueGraphResult{Issue: issue}
	res.Forward, res.Backward = planner.BuildAdjacency(relations)

	visited := map[int]bool{id: true}
	if in.Direction == "down" || in.Direction == "both" {
		walkGraph(id, res.Forward, true, visited, &res.Edges, in.Depth)
	}
	if in.Direction == "up" || in.Direction == "both" {
		walkGraph(id, res.Backward, false, visited, &res.Edges, in.Depth)
	}

	visitedIDs := make([]int, 0, len(visited))
	for nodeID := range visited {
		visitedIDs = append(visitedIDs, nodeID)
	}
	if res.Issues, err = env.Store.GetIssuesByIDs(visitedIDs); err != nil {
		return IssueGraphResult{}, newError(fmt.Errorf("fetching issues: %w", err), output.ErrGeneral)
	}
	res.Issues[id] = issue
	unreached := func(k int, _ []int) bool { return res.Issues[k] == nil }
	maps.DeleteFunc(res.Forward, unreached)
	maps.DeleteFunc(res.Backward, unreached)
	return res, nil
}

// walkGraph runs a breadth-first walk from start along adj, marking each
// issue it reaches in visited and appending each edge once. downward says adj
// runs from blocker to blocked; otherwise it runs from blocked to blocker.
// maxDepth caps the hops from start, and 0 means unlimited.
func walkGraph(start int, adj map[int][]int, downward bool, visited map[int]bool, edges *[]IssueGraphEdge, maxDepth int) {
	type queueItem struct {
		id    int
		depth int
	}
	type edgeKey struct{ from, to int }
	seen := make(map[edgeKey]bool)

	queue := []queueItem{{id: start, depth: 0}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if maxDepth > 0 && current.depth >= maxDepth {
			continue
		}

		for _, neighbor := range adj[current.id] {
			edge := IssueGraphEdge{From: neighbor, To: current.id, Type: "blocks"}
			if downward {
				edge = IssueGraphEdge{From: current.id, To: neighbor, Type: "blocks"}
			}

			k := edgeKey{edge.From, edge.To}
			if !seen[k] {
				seen[k] = true
				*edges = append(*edges, edge)
			}

			if !visited[neighbor] {
				visited[neighbor] = true
				queue = append(queue, queueItem{id: neighbor, depth: current.depth + 1})
			}
		}
	}
}

// IssueListInput filters an issue listing; every filter set must match.
// Project lists the project it names instead of the caller's; Run lists the
// issues a run's activation bound, done ones included. The two cannot be
// combined. Sort is field[:direction]. Limit caps the rows; 0 means no limit.
// ForTable also reads what a table rendering needs beyond the rows: the
// parents of listed issues that the listing itself leaves out, sub-issue
// progress, and the done issues an empty listing hid.
type IssueListInput struct {
	Statuses   []string
	Priorities []string
	Sizes      []string
	Labels     []string
	Types      []string
	Assignee   string
	Parent     string
	RootsOnly  bool
	All        bool
	Sort       string
	Limit      int
	Run        string
	Project    string
	ForTable   bool
}

// IssueListResult is the listing. Total counts every match before Limit.
// Project is the listed project when it is not the caller's: the one
// IssueListInput.Project names, or the run's. RunID is the listed run, 0
// without one. HiddenDone, Parents and Progress are set under ForTable.
// HiddenDone counts the done issues an empty listing left out, and is 0 for a
// run's roster or when done issues were included. Progress maps a parent id
// to its done and total sub-issue counts.
type IssueListResult struct {
	Issues     []*model.Issue
	Total      int
	Project    *model.Project
	RunID      int
	HiddenDone int
	Parents    map[int]*model.Issue
	Progress   map[int][2]int
}

// IssueList lists issues. It is a read: an identity no project holds lists no
// issues, and nothing is registered.
func IssueList(ctx context.Context, env Env, in IssueListInput) (IssueListResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return IssueListResult{}, err
	}

	// Refused before either scope is resolved, so a conflicting request
	// resolves neither.
	if in.Run != "" && in.Project != "" {
		return IssueListResult{}, newError(errors.New(
			"--run and --project cannot be combined: a run belongs to one "+
				"project and --run already lists it"), output.ErrValidation)
	}
	if err := validateIssueFilters(in); err != nil {
		return IssueListResult{}, err
	}

	var res IssueListResult
	if in.Project != "" {
		project, err := ResolveProjectRef(env.Store, in.Project)
		if err != nil {
			return IssueListResult{}, err
		}
		projectID, res.Project = project.ID, project
	}

	all := in.All
	if in.Run != "" {
		runID, err := model.ParseRunID(in.Run)
		if err != nil {
			return IssueListResult{}, newError(err, output.ErrValidation)
		}
		run, err := env.Store.GetRun(runID)
		if errors.Is(err, store.ErrRunNotFound) {
			return IssueListResult{}, newError(
				fmt.Errorf("run %s not found", model.FormatRunID(runID)), output.ErrNotFound)
		}
		if err != nil {
			return IssueListResult{}, newError(fmt.Errorf("reading run: %w", err), output.ErrGeneral)
		}
		res.RunID = run.ID
		if run.ProjectID != 0 && run.ProjectID != projectID {
			project, err := env.Store.GetProject(run.ProjectID)
			if err != nil {
				return IssueListResult{}, newError(
					fmt.Errorf("reading the run's project: %w", err), output.ErrGeneral)
			}
			projectID, res.Project = project.ID, project
		}
		// A roster is a closed set, and a finished run's is mostly done issues.
		all = true
	}

	opts := store.ListOptions{
		ProjectID:   projectID,
		RunID:       res.RunID,
		Statuses:    in.Statuses,
		Priorities:  in.Priorities,
		Sizes:       in.Sizes,
		Labels:      in.Labels,
		Types:       in.Types,
		Assignee:    in.Assignee,
		RootsOnly:   in.RootsOnly,
		IncludeDone: all,
		Limit:       in.Limit,
	}
	if in.Parent != "" {
		pid, err := model.ParseID(in.Parent)
		if err != nil {
			return IssueListResult{}, newError(fmt.Errorf("invalid parent ID: %w", err), output.ErrValidation)
		}
		opts.ParentID = &pid
	}
	if in.Sort != "" {
		parts := strings.SplitN(in.Sort, ":", 2)
		opts.Sort = parts[0]
		if len(parts) > 1 {
			opts.SortDir = parts[1]
		}
	}

	res.Issues, res.Total, err = env.Store.ListIssues(opts)
	if err != nil {
		return IssueListResult{}, newError(fmt.Errorf("listing issues: %w", err), output.ErrGeneral)
	}
	if err := env.Store.HydrateDocs(res.Issues); err != nil {
		return IssueListResult{}, newError(fmt.Errorf("fetching linked docs: %w", err), output.ErrGeneral)
	}

	if !in.ForTable {
		return res, nil
	}
	if len(res.Issues) == 0 && res.RunID == 0 && !all {
		withDone := opts
		withDone.IncludeDone, withDone.Limit = true, 1
		// The count only refines an empty listing's hint, so a failed read
		// leaves it at 0 rather than failing the listing.
		if _, total, err := env.Store.ListIssues(withDone); err == nil {
			res.HiddenDone = total
		}
	}
	if err := listParents(env, &res); err != nil {
		return IssueListResult{}, err
	}
	return res, nil
}

// validateIssueFilters checks each filter value against its enum.
func validateIssueFilters(in IssueListInput) error {
	for _, s := range in.Statuses {
		if err := model.ValidateStatus(model.Status(s)); err != nil {
			return newError(err, output.ErrValidation)
		}
	}
	for _, p := range in.Priorities {
		if err := model.ValidatePriority(model.Priority(p)); err != nil {
			return newError(err, output.ErrValidation)
		}
	}
	for _, sz := range in.Sizes {
		if err := model.ValidateSize(model.Size(sz)); err != nil {
			return newError(err, output.ErrValidation)
		}
	}
	for _, t := range in.Types {
		if err := model.ValidateIssueKind(model.IssueKind(t)); err != nil {
			return newError(err, output.ErrValidation)
		}
	}
	return nil
}

// listParents fills res.Parents with the parents of listed issues that the
// listing left out, and res.Progress with the sub-issue counts of every
// parent of a listed issue, listed or not.
func listParents(env Env, res *IssueListResult) error {
	listed := make(map[int]bool, len(res.Issues))
	for _, issue := range res.Issues {
		listed[issue.ID] = true
	}

	var missing []int
	seen := make(map[int]bool)
	for _, issue := range res.Issues {
		if issue.ParentID != nil && !listed[*issue.ParentID] && !seen[*issue.ParentID] {
			seen[*issue.ParentID] = true
			missing = append(missing, *issue.ParentID)
		}
	}
	if len(missing) > 0 {
		parents, err := env.Store.GetIssuesByIDs(missing)
		if err != nil {
			return newError(fmt.Errorf("fetching parent issues: %w", err), output.ErrGeneral)
		}
		res.Parents = parents
	}

	parentIDs := make(map[int]bool)
	for id := range res.Parents {
		parentIDs[id] = true
	}
	for _, issue := range res.Issues {
		if issue.ParentID == nil {
			continue
		}
		if pid := *issue.ParentID; listed[pid] || res.Parents[pid] != nil {
			parentIDs[pid] = true
		}
	}
	if len(parentIDs) == 0 {
		return nil
	}
	ids := make([]int, 0, len(parentIDs))
	for id := range parentIDs {
		ids = append(ids, id)
	}
	progress, err := env.Store.GetBatchSubIssueProgress(ids)
	if err != nil {
		return newError(fmt.Errorf("fetching sub-issue progress: %w", err), output.ErrGeneral)
	}
	res.Progress = progress
	return nil
}

// refetchIssue reads issue id back after a write, with its labels, files and
// docs.
func refetchIssue(env Env, id int) (*model.Issue, error) {
	issue, err := env.Store.GetIssue(id)
	if err != nil {
		return nil, newError(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
	}
	if err := hydrateIssue(env, issue); err != nil {
		return nil, err
	}
	return issue, nil
}

// reparentTarget is the parent_id an edit of issue id in projectID to parent
// ref writes: nil for "0" or "none", else the parent's id, refusing a parent
// that is the issue itself or one of its descendants.
func reparentTarget(env Env, id, projectID int, ref string) (*int, error) {
	if strings.EqualFold(ref, "0") || strings.EqualFold(ref, "none") {
		return nil, nil
	}
	// The parent must share the edited issue's project, never the caller's.
	parent, err := resolveParentIssue(env, ref, projectID)
	if err != nil {
		return nil, err
	}
	if parent.ID == id {
		return nil, newError(errors.New("cannot set parent to self"), output.ErrValidation)
	}
	isCycle, err := env.Store.IsDescendant(id, parent.ID)
	if err != nil {
		return nil, newError(fmt.Errorf("checking for cycles: %w", err), output.ErrGeneral)
	}
	if isCycle {
		return nil, newError(errors.New("cannot reparent: would create a cycle"), output.ErrConflict)
	}
	return &parent.ID, nil
}

// resolveParentIssue resolves a prospective parent reference and refuses one
// homed in a project other than childProjectID, the project the child lives
// in or will be created in. Issue ids are store-wide, but a subtree spanning
// projects breaks `issue move --project`, which migrates whole subtrees.
func resolveParentIssue(env Env, ref string, childProjectID int) (*model.Issue, error) {
	pid, err := model.ParseID(ref)
	if err != nil {
		return nil, newError(fmt.Errorf("invalid parent ID: %w", err), output.ErrValidation)
	}
	parent, err := env.Store.GetIssue(pid)
	if err != nil {
		return nil, issueFetchError(err, "parent issue "+ref)
	}
	if parent.ProjectID != childProjectID {
		return nil, newError(fmt.Errorf(
			"parent issue %s belongs to %s, this issue to %s — a sub-issue lives in "+
				"its parent's project; re-home the subtree with `docket issue move "+
				"--project` on its root instead",
			model.FormatID(pid), projectLabel(env.Store, parent.ProjectID),
			projectLabel(env.Store, childProjectID)), output.ErrValidation)
	}
	return parent, nil
}

// issueFetchError reports a missing issue as NOT_FOUND naming label, and any
// other failure as GENERAL_ERROR.
func issueFetchError(err error, label string) error {
	if e := notFound(err, label); e != nil {
		return e
	}
	return newError(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
}

// hydrateIssue fills an issue's labels, files and docs from their join
// tables, which GetIssue does not read, so a result echoes what is stored.
func hydrateIssue(env Env, issue *model.Issue) error {
	return hydrateIssueNamingDocs(env, issue, "docs")
}

// hydrateIssueNamingDocs is hydrateIssue with the noun a failed docs read
// reports: issue show has always called them "linked docs".
func hydrateIssueNamingDocs(env Env, issue *model.Issue, docsNoun string) error {
	labels, err := env.Store.GetIssueLabels(issue.ID)
	if err != nil {
		return newError(fmt.Errorf("fetching labels: %w", err), output.ErrGeneral)
	}
	issue.Labels = labels

	files, err := env.Store.GetIssueFiles(issue.ID)
	if err != nil {
		return newError(fmt.Errorf("fetching files: %w", err), output.ErrGeneral)
	}
	issue.Files = files

	if err := env.Store.HydrateDocs([]*model.Issue{issue}); err != nil {
		return newError(fmt.Errorf("fetching %s: %w", docsNoun, err), output.ErrGeneral)
	}
	return nil
}

// ScopeGlobsJSON encodes an issue's declared scope as the JSON array
// scope_globs stores, trimming each glob and keeping the declared order, which
// is the author's and is echoed back in the issue snapshot. It returns "",
// which stores NULL, for an empty list: "no scope declared" and "declared to
// touch nothing" are different facts, and only the first is the default.
func ScopeGlobsJSON(globs []string) (string, error) {
	cleaned := make([]string, 0, len(globs))
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			return "", newError(errors.New("--scope glob must not be empty"), output.ErrValidation)
		}
		cleaned = append(cleaned, g)
	}
	if len(cleaned) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return "", newError(fmt.Errorf("encoding --scope: %w", err), output.ErrGeneral)
	}
	return string(encoded), nil
}

// optionalScopeGlobsJSON is ScopeGlobsJSON for a scope that may be absent.
func optionalScopeGlobsJSON(scope *[]string) (string, error) {
	if scope == nil {
		return "", nil
	}
	return ScopeGlobsJSON(*scope)
}

// checkToken refuses an empty token before it reaches the store, where it
// would read as a wrong holder rather than as no token at all.
func checkToken(token string) error {
	if token == "" {
		return newError(errors.New("no capability token supplied"), output.ErrValidation)
	}
	return nil
}

// checkIfVersion refuses a version precondition below 1, which no row can
// satisfy since versions start at 1.
func checkIfVersion(v *int) error {
	if v != nil && *v < 1 {
		return newError(fmt.Errorf("--if-version must be >= 1, got %d (versions start at 1)", *v),
			output.ErrValidation)
	}
	return nil
}

func checkTTL(ttl *time.Duration) error {
	if ttl != nil && *ttl <= 0 {
		return newError(fmt.Errorf("--ttl must be positive, got %s", *ttl), output.ErrValidation)
	}
	return nil
}

// resolveRegisteringProject resolves the caller's project for a verb that
// writes there and so may register it. When it registers one, it returns the
// registration, attributed to verbPath, for the caller to record as an event.
func resolveRegisteringProject(env Env, verbPath string) (int, *engine.ProjectRegistration, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil || !created {
		return projectID, nil, err
	}
	prefix, _ := env.Store.ProjectPrefix(projectID)
	return projectID, &engine.ProjectRegistration{
		ID:       projectID,
		Name:     filepath.Base(env.Project.Identity),
		Identity: env.Project.Identity,
		Cwd:      env.Cwd,
		Verb:     verbPath,
		Prefix:   prefix,
	}, nil
}

// leaseTTL returns ttl when the caller set one, else the project's configured
// TTL for class, which falls back to lease.ttl.default.
func leaseTTL(env Env, projectID int, ttl *time.Duration, class string) (time.Duration, error) {
	if ttl != nil {
		return *ttl, nil
	}
	configured, err := env.Store.LeaseTTL(projectID, class)
	if err != nil {
		return 0, fmt.Errorf("resolving lease TTL: %w", err)
	}
	return configured, nil
}

// leaseRefusal translates a failed lease call on issue id, naming the issue
// as issueDisplayID does. An error the lease matrix does not cover is wrapped
// as action.
func leaseRefusal(env Env, projectID, id int, err error, action string) error {
	if e := LeaseError(err, "issue "+issueDisplayID(env, projectID, id)); e != nil {
		return e
	}
	return fmt.Errorf("%s: %w", action, err)
}

// issueDisplayID renders issue id under its owning project's prefix, or the
// caller's project's (projectID) for an id no project owns.
func issueDisplayID(env Env, projectID, id int) string {
	prefix, _ := env.Store.IssueOwnerPrefix(id)
	if prefix == "" {
		prefix, _ = env.Store.ProjectPrefix(projectID)
	}
	return model.FormatIDWithPrefix(id, prefix)
}
