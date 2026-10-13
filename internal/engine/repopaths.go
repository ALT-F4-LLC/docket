package engine

import (
	"github.com/ALT-F4-LLC/docket/internal/config"
)

// RepoPaths carries the per-project execution facts a runner needs.
//
// The three fields USED TO BE ONE VALUE — the parent of the .docket store
// stood in for all of them, so a gate's binding check and its cwd could not
// disagree. A store shared by every project is exactly
// what breaks that identity apart: where a command runs (ExecRoot), which
// project authorized it (Identity), and which lockfile serializes its tree
// (LockPath) are separate facts once the store no longer lives inside the
// repository. For env- and locally-resolved stores config.Resolve still
// yields ExecRoot == Identity == parent-of-store, so every existing setup
// sees byte-identical behavior.
type RepoPaths struct {
	// ExecRoot is the cwd every gate and action runs in, and the containment
	// boundary for their binaries.
	ExecRoot string
	// Identity is what trust entries bind to — the project, not the
	// checkout.
	Identity string
	// LockPath is the tree-mutex lockfile serializing this project's
	// tree-declaring gates.
	LockPath string
}

// resolvePaths resolves the current invocation's execution facts.
//
// A repo that cannot be resolved yields empty paths, and a runner built from
// them reports every gate `unmatched` — fail-closed, the same direction every
// other unknown takes.
func resolvePaths() *config.Config {
	cfg, err := config.Resolve()
	if err != nil {
		return &config.Config{}
	}
	return cfg
}

// repoPathsFrom projects the resolved config onto what runners consume.
func repoPathsFrom(cfg *config.Config) RepoPaths {
	return RepoPaths{
		ExecRoot: cfg.ExecRoot,
		Identity: cfg.Identity,
		LockPath: cfg.TreeLockPath(),
	}
}
