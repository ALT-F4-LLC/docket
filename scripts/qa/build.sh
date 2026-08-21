#!/usr/bin/env bash
#
# build — the compile gate.
#
# Wired as the `build` gate on `implement`, `fix`, and the doc pipelines'
# write steps (every `gates = [...]` array in .docket/config/workflows/ that
# names it). It answers one question: does the tree compile.
#
# It deliberately checks nothing else. `tests` runs the test suite, `genericity`
# scans the core surface, `secret-scan` looks for credentials — each is its own
# gate so one failure surfaces as one failure.
#
# go1.26.6 rather than `go`: that is the binary this toolchain provides under
# the name the other gates already use (see ac-commands.sh), and a gate naming
# a command the environment lacks records "unmatched" and teaches nobody
# anything.

set -euo pipefail

# Root resolution respects the caller's cwd: the engine spawns gates with Dir
# set to the step's worktree, and trust entries invoke this script by absolute
# path into the shared checkout — a script-relative cd would re-point the gate
# at the shared tree. Outside any git repo, fall back to the script's own root.
cd "$(git rev-parse --show-toplevel 2>/dev/null || echo "$(dirname "${BASH_SOURCE[0]}")/../..")"

echo "=== build: go1.26.6 build ./... ==="

if ! go1.26.6 build ./... 2>&1; then
  cat >&2 <<'EOF'

build FAILED: the tree does not compile.

Every downstream gate is meaningless on a tree that does not build — `tests`
cannot run, and a reviewer reading a non-compiling diff is reading a draft.
Fix the compile error before anything else in the step.
EOF
  exit 1
fi

echo "build: ok"
exit 0
