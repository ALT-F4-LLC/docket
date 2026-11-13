package verb

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
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
