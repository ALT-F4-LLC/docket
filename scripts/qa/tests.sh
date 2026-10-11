#!/usr/bin/env bash
#
# tests — the test-suite gate.
#
# Wired as the `tests` gate on `implement` and `fix` (every `gates = [...]`
# array in .docket/config/workflows/ that names it). It runs the Go test suite
# and fails the step when any package fails.
#
# It runs `go test ./...` and NOT `scripts/qa.sh`. The two cover different
# things: qa.sh is the CLI-behavior harness the `test` CI job runs (see
# .github/workflows/ci.yaml), while this gate is the unit/integration suite the
# `go-test` CI job runs. Running qa.sh here would make one failure surface twice
# and would serialize every gate behind a multi-minute harness.
#
# go1.26.9 rather than `go`: the name the other gates already use
# (see ac-commands.sh, build.sh).

set -euo pipefail

# Root resolution respects the caller's cwd: the engine spawns gates with Dir
# set to the step's worktree, and trust entries invoke this script by absolute
# path into the shared checkout — a script-relative cd would re-point the gate
# at the shared tree. Outside any git repo, fall back to the script's own root.
cd "$(git rev-parse --show-toplevel 2>/dev/null || echo "$(dirname "${BASH_SOURCE[0]}")/../..")"

# -timeout 60m: internal/engine alone took 1380-1570s under concurrent run load
# (RUN-135: 13-16 write steps at once on 18 CPUs, load average 32-56), against
# the former 30m (1800s) timeout. 60m leaves headroom over that range. The
# durable fix is DKT-3748's host-scoped class caps, which bound concurrent runs.
echo "=== tests: go1.26.9 test -timeout 60m ./... ==="

if ! go1.26.9 test -timeout 60m ./... 2>&1; then
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
