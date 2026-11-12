package verb

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// ProjectPolicy is what a verb declares about the project it runs in: whether
// it may register the caller's identity as a new project, and whether it needs
// one when it may not.
type ProjectPolicy int

const (
	// ProjectReadOnly observes: an unregistered identity resolves to
	// store.UnregisteredProjectID, which matches no rows, and nothing is
	// created.
	ProjectReadOnly ProjectPolicy = iota + 1
	// ProjectMayRegister writes into the caller's project: an anchored
	// identity is registered on first contact, and an unanchored one is
	// refused, since the verb would otherwise write into a project the caller
	// never named.
	ProjectMayRegister
	// ProjectRunAddressed takes its project from the run or step it is handed:
	// an anchored identity is still registered on first contact, but an
	// unanchored one resolves to store.UnregisteredProjectID instead of
	// failing.
	ProjectRunAddressed
)

// ResolveProject answers which project env.Project names under policy.
// created reports that this call registered the project; the caller records
// the project-registered event, since that write is outside store.Store.
//
// An empty identity is a store that is not multi-tenant and resolves to
// store.DefaultProjectID. An identity that is already registered resolves to
// its row under every policy, so rows registered by mistake stay reachable.
func (env Env) ResolveProject(policy ProjectPolicy) (id int, created bool, err error) {
	switch policy {
	case ProjectReadOnly, ProjectMayRegister, ProjectRunAddressed:
	default:
		return 0, false, fmt.Errorf("resolving the project: unknown policy %v", policy)
	}

	identity := env.Project.Identity
	if identity == "" {
		return store.DefaultProjectID, false, nil
	}

	id, found, err := env.Store.LookupProject(identity)
	if err != nil {
		return 0, false, fmt.Errorf("resolving the project: %w", err)
	}
	if found {
		return id, false, nil
	}

	if policy == ProjectReadOnly {
		return store.UnregisteredProjectID, false, nil
	}
	if !env.Project.Anchored {
		if policy == ProjectMayRegister {
			return 0, false, newError(fmt.Errorf(
				"cannot tell which project this belongs to: %s is not a "+
					"git worktree and has no docket project.\n"+
					"Run from inside the repository, or create a repo-local "+
					"store there with `docket init --local`",
				env.Cwd), output.ErrValidation)
		}
		return store.UnregisteredProjectID, false, nil
	}

	id, created, err = env.Store.EnsureProjectCreated(
		identity, filepath.Base(identity), time.Now().UnixMilli())
	if err != nil {
		return 0, false, fmt.Errorf("resolving the project: %w", err)
	}
	return id, created, nil
}
