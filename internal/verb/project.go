package verb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// ProjectListInput takes nothing: the listing is the whole store's.
type ProjectListInput struct{}

// ProjectListResult is every project the store holds; Current is the
// caller's project, or store.UnregisteredProjectID when it has none.
type ProjectListResult struct {
	Projects []*model.Project
	Current  int
}

// ProjectList lists the store's projects. It is a read, so it never registers
// the caller's project.
func ProjectList(ctx context.Context, env Env, in ProjectListInput) (ProjectListResult, error) {
	current, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return ProjectListResult{}, err
	}
	projects, err := env.Store.ListProjects()
	if err != nil {
		return ProjectListResult{}, newError(fmt.Errorf("listing projects: %w", err), output.ErrGeneral)
	}
	return ProjectListResult{Projects: projects, Current: current}, nil
}

// ProjectSetPrefixInput is the display prefix to give the caller's project,
// as typed; the verb trims and upper-cases it.
type ProjectSetPrefixInput struct {
	Prefix string
}

// ProjectSetPrefixResult is the caller's project after the change. Registered
// is the row as this call registered it, nil when it already existed; the
// caller records the project-registered event for it, since that write is
// outside store.Store.
type ProjectSetPrefixResult struct {
	Project    *model.Project
	Registered *model.Project
}

// ProjectSetPrefix sets the caller's project's display prefix, refusing one
// another project holds. It writes into the caller's project, so it may
// register an anchored one. Registered is set even when the change itself is
// refused, because the registration has already happened.
func ProjectSetPrefix(ctx context.Context, env Env, in ProjectSetPrefixInput) (ProjectSetPrefixResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return ProjectSetPrefixResult{}, err
	}
	var result ProjectSetPrefixResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}

	prefix := strings.ToUpper(strings.TrimSpace(in.Prefix))
	if err := model.ValidateProjectPrefix(prefix); err != nil {
		return result, newError(err, output.ErrValidation)
	}

	// DKT-60: no two projects may display the same prefix. The prefix is a
	// project's ONLY discriminator in a listing, an event feed, or a run
	// report, so a shared one makes every id in the store ambiguous about who
	// owns it.
	holder, err := env.Store.PrefixHolder(prefix, projectID)
	if err != nil {
		return result, newError(err, output.ErrGeneral)
	}
	if holder != 0 {
		name := fmt.Sprintf("project %d", holder)
		if other, gerr := env.Store.GetProject(holder); gerr == nil {
			name = fmt.Sprintf("%q (%s)", other.Name, other.Identity)
		}
		return result, newError(fmt.Errorf(
			"prefix %s is already held by %s: two projects sharing a prefix "+
				"makes every id in the store ambiguous about its owner",
			prefix, name), output.ErrConflict)
	}

	if err := env.Store.SetProjectPrefix(projectID, prefix); err != nil {
		return result, newError(fmt.Errorf("setting the prefix: %w", err), output.ErrGeneral)
	}
	if result.Project, err = env.Store.GetProject(projectID); err != nil {
		return result, newError(fmt.Errorf("re-reading the project: %w", err), output.ErrGeneral)
	}
	return result, nil
}

// ProjectDeleteInput names the project to delete by any key ResolveProjectRef
// accepts.
type ProjectDeleteInput struct {
	Project string
}

// ProjectDeleteResult is the deleted row. Registered is as in
// ProjectSetPrefixResult.
type ProjectDeleteResult struct {
	Project    *model.Project
	Registered *model.Project
}

// ProjectDelete deletes a project that no row references, and refuses the
// default project. It is a write, so it may register the caller's anchored
// project, and an unanchored caller with no project is refused before the
// target is looked up.
func ProjectDelete(ctx context.Context, env Env, in ProjectDeleteInput) (ProjectDeleteResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return ProjectDeleteResult{}, err
	}
	var result ProjectDeleteResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}

	target, err := ResolveProjectRef(env.Store, in.Project)
	if err != nil {
		return result, err
	}

	if err := env.Store.DeleteProject(target.ID); err != nil {
		switch {
		case errors.Is(err, store.ErrProjectIsDefault):
			return result, newError(fmt.Errorf(
				"project %q is the default project and cannot be deleted: "+
					"every pre-tenancy row in this store belongs to it",
				target.Name), output.ErrValidation)
		case errors.Is(err, store.ErrProjectInUse):
			counts, cerr := env.Store.ProjectRefCounts(target.ID)
			if cerr != nil {
				return result, newError(cerr, output.ErrGeneral)
			}
			return result, newError(fmt.Errorf(
				"project %q still holds %s: re-home them with "+
					"`docket issue move --project` before deleting it",
				target.Name, describeRefCounts(counts)), output.ErrConflict)
		default:
			return result, newError(fmt.Errorf("deleting the project: %w", err), output.ErrGeneral)
		}
	}
	result.Project = target
	return result, nil
}

// registeredProject reads the row ResolveProject just registered, or returns
// nil when it registered none.
func registeredProject(env Env, id int, created bool) (*model.Project, error) {
	if !created {
		return nil, nil
	}
	p, err := env.Store.GetProject(id)
	if err != nil {
		return nil, newError(fmt.Errorf("reading the registered project: %w", err), output.ErrGeneral)
	}
	return p, nil
}

// ResolveProjectRef finds the project a `--project` value (or a `project
// delete` argument) names — by identity path, row id, name, or display PREFIX.
//
// It is the ONE resolver behind every project-addressing surface. There used to
// be two and they diverged: `issue move --project FLX` worked while `issue list
// --project FLX` answered NOT_FOUND (DKT-453). A second copy of a resolution
// ladder is a second set of keys.
//
// FOUR KEYS, because `project list` prints three columns — NAME, PREFIX,
// IDENTITY — and any of them reads as the way to say which project this is.
// The PREFIX matters most: it is the only project discriminator an issue id
// carries (FLX-141), so it is the identifier a caller has actually SEEN before
// typing it.
//
// AMBIGUITY IS REFUSED, NOT RESOLVED. Identity is unique (the column's
// constraint) and a row id names one row; NAME is not unique at all — it is a
// directory basename — and PREFIX is unique by construction but nothing makes an
// imported or hand-edited store obey that. When a key names more than one
// project the candidates are listed instead: picking the lowest id would answer
// a question nobody asked.
//
// Name and prefix match case-insensitively, but an EXACT match wins outright, so
// two rows differing only in case each still resolve by their own spelling.
func ResolveProjectRef(st store.Store, ref string) (*model.Project, error) {
	projects, err := st.ListProjects()
	if err != nil {
		return nil, newError(fmt.Errorf("listing projects: %w", err), output.ErrGeneral)
	}

	// The identity path is the canonical key and is unique, so it is tried first
	// and can never be ambiguous.
	for _, p := range projects {
		if p.Identity == ref {
			return p, nil
		}
	}

	// A numeric ref is a row id and nothing else: names are directory basenames
	// and prefixes are letters only (model.ValidateProjectPrefix), so falling
	// through to them would only make the miss message vaguer.
	if id, err := strconv.Atoi(ref); err == nil {
		for _, p := range projects {
			if p.ID == id {
				return p, nil
			}
		}
		return nil, newError(fmt.Errorf("no project with id %d", id), output.ErrNotFound)
	}

	matching := func(field func(*model.Project) string) []*model.Project {
		var exact, folded []*model.Project
		for _, p := range projects {
			switch value := field(p); {
			case value == "":
			case value == ref:
				exact = append(exact, p)
			case strings.EqualFold(value, ref):
				folded = append(folded, p)
			}
		}
		if len(exact) > 0 {
			return exact
		}
		return folded
	}

	for _, key := range []struct {
		verb  string
		field func(*model.Project) string
	}{
		{"names", func(p *model.Project) string { return p.Name }},
		{"is the prefix of", func(p *model.Project) string { return p.Prefix }},
	} {
		switch hits := matching(key.field); len(hits) {
		case 0:
		case 1:
			return hits[0], nil
		default:
			return nil, newError(fmt.Errorf(
				"%q %s %d projects — address one by id or identity: %s",
				ref, key.verb, len(hits), describeProjectCandidates(hits)),
				output.ErrValidation)
		}
	}

	return nil, newError(fmt.Errorf(
		"no project named %q; `docket project list` shows what this store holds "+
			"(its NAME, PREFIX, and IDENTITY columns all resolve here)",
		ref), output.ErrNotFound)
}

// describeProjectCandidates renders the candidates an ambiguous ref is refused
// with: the id to address one by, plus the name and identity that say which
// checkout each one is. Whichever key was ambiguous repeats across the list, so
// both of the others are printed — one of them is always the discriminator.
func describeProjectCandidates(ps []*model.Project) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		where := p.Identity
		if where == "" {
			where = "unclaimed"
		}
		out = append(out, fmt.Sprintf("%d (%s, %s)", p.ID, ProjectDisplayName(p), where))
	}
	return strings.Join(out, ", ")
}

// ProjectDisplayName picks the most human name a project row carries.
func ProjectDisplayName(p *model.Project) string {
	if p.Name != "" {
		return p.Name
	}
	if p.Identity != "" {
		return p.Identity
	}
	return strconv.Itoa(p.ID)
}

// describeRefCounts renders "3 issues, 1 run" for the refusal message. The
// counts are named because "still has rows" leaves the operator to go find
// which ones.
func describeRefCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "rows"
	}
	tables := make([]string, 0, len(counts))
	for table := range counts {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	parts := make([]string, 0, len(tables))
	for _, table := range tables {
		noun := strings.TrimSuffix(table, "s")
		if counts[table] != 1 {
			noun = table
		}
		parts = append(parts, fmt.Sprintf("%d %s", counts[table], noun))
	}
	return strings.Join(parts, ", ")
}
