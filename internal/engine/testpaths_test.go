package engine

import "path/filepath"

// testRepoPaths builds the env/local-shaped RepoPaths every runner test wants:
// exec root, identity, and lockfile all derived from one repo directory —
// exactly what config.Resolve yields for a repo-local store.
func testRepoPaths(repoRoot string) RepoPaths {
	return RepoPaths{
		ExecRoot: repoRoot,
		Identity: repoRoot,
		LockPath: filepath.Join(repoRoot, ".docket", "tree.lock"),
	}
}
