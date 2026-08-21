#!/usr/bin/env bash
#
# tests — the test-suite gate.
#
# Wired as the `tests` gate on `implement` and `fix` (every `gates = [...]`
# array in .docket/config/workflows/ that names it). It runs the Go test suite
# and fails the step when any package fails.
#
# It runs `go test ./...` and NOT `scripts/qa.sh`. The two cover different
# things: qa.sh is the CLI-behavior harness the `qa` CI job runs (see
# .github/workflows/ci.yaml), while this gate is the unit/integration suite the
# `test` CI job runs. Running qa.sh here would make one failure surface twice
# and would serialize every gate behind a multi-minute harness.
#
# go1.26.6 rather than `go`: the name the other gates already use
# (see ac-commands.sh, build.sh).

set -euo pipefail

# Root resolution respects the caller's cwd: the engine spawns gates with Dir
# set to the step's worktree, and trust entries invoke this script by absolute
# path into the shared checkout — a script-relative cd would re-point the gate
# at the shared tree. Outside any git repo, fall back to the script's own root.
cd "$(git rev-parse --show-toplevel 2>/dev/null || echo "$(dirname "${BASH_SOURCE[0]}")/../..")"

echo "=== tests: go1.26.6 test ./... ==="

if ! go1.26.6 test ./... 2>&1; then
  cat >&2 <<'EOF'

tests FAILED: at least one package's tests did not pass.

The output above names the failing package and test. A gate failure here is a
real defect signal, not a flake to retry — this entry is NOT declared `flaky`
in the trust store precisely so a retry cannot hide it.
EOF
  exit 1
fi

echo "tests: ok"
exit 0
