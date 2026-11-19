package verb

import (
	"context"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// RegistryAuditInput narrows the store-wide audit.
type RegistryAuditInput struct {
	// Project audits one project, named by any key ResolveProjectRef accepts;
	// blank audits every project in the store.
	Project string
}

// RegistryAuditResult is the store's verdict, including the roots "current"
// was read from.
type RegistryAuditResult struct {
	Audit *engine.RegistryAudit
}

// RegistryAudit compares every project's registered workflow and schema names
// against one scan of the shared store root's config. It is a read: the
// caller's project is never registered, and the audit does not depend on it.
//
// It refuses before any verdict when nothing could be scanned, since every
// registered name would then look orphaned and behind nothing.
func RegistryAudit(ctx context.Context, env Env, in RegistryAuditInput) (RegistryAuditResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return RegistryAuditResult{}, err
	}

	var opts engine.RegistryAuditOptions
	if ref := strings.TrimSpace(in.Project); ref != "" {
		target, err := ResolveProjectRef(env.Store, ref)
		if err != nil {
			return RegistryAuditResult{}, err
		}
		opts.ProjectID = target.ID
	}

	// The two refusals are separate messages because they send an operator to
	// different places. `workflow list --orphans` refuses on the same two
	// conditions, with the same codes.
	index, err := engine.ScanCorpus()
	if err != nil {
		return RegistryAuditResult{}, newError(err, output.ErrValidation)
	}
	if !index.Scanned() {
		return RegistryAuditResult{}, newError(fmt.Errorf(
			"the shared store root has no config directory, and the audit never "+
				"reads a repository's .docket/config, so no registry can be "+
				"compared against anything: with nothing to scan, every "+
				"registered name would look orphaned"), output.ErrValidation)
	}
	if err := index.Err(); err != nil {
		return RegistryAuditResult{}, newError(fmt.Errorf(
			"scanning the instance config for current definitions: %w", err),
			output.ErrGeneral)
	}

	audit, err := engine.AuditRegistriesStore(env.Store, index, opts)
	if err != nil {
		return RegistryAuditResult{}, newError(err, output.ErrGeneral)
	}
	return RegistryAuditResult{Audit: audit}, nil
}
