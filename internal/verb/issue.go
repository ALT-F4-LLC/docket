package verb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
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
	res := IssueClaimResult{Issue: id, Registered: registered}
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

	res.Token, res.Lease, res.Version = token, *lease, version
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
	res := IssueReleaseResult{Issue: id, Registered: registered}
	if err != nil {
		return res, err
	}

	lease, err := env.Store.ReleaseIssue(id, in.Token, model.NowMS())
	if err != nil {
		return res, leaseRefusal(env, projectID, id, err, "releasing lease")
	}

	res.Lease = *lease
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
	res := IssueHeartbeatResult{Issue: id, Registered: registered}
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

	res.Lease = *lease
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
		return newError(fmt.Errorf("fetching docs: %w", err), output.ErrGeneral)
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
// by its owning project's prefix, or the caller's project's for an id no
// project owns. An error the lease matrix does not cover is wrapped as action.
func leaseRefusal(env Env, projectID, id int, err error, action string) error {
	prefix, _ := env.Store.IssueOwnerPrefix(id)
	if prefix == "" {
		prefix, _ = env.Store.ProjectPrefix(projectID)
	}
	if e := LeaseError(err, "issue "+model.FormatIDWithPrefix(id, prefix)); e != nil {
		return e
	}
	return fmt.Errorf("%s: %w", action, err)
}
