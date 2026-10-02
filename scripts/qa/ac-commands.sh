#!/usr/bin/env bash
#
# ac-commands — run the repo's checks and print their results for the verifier.
#
# Wired as a PRE-GATE on `verify`: it runs at claim, and its output
# lands in the step's context bundle. That is the whole point. `verify-ac`
# judges acceptance criteria against the diff, and an AC of the form "the tests
# pass" is one it cannot settle by reading code — without recorded command
# results it must return `unverifiable`, which parks the step on the operator.
#
# It reports AND propagates: it prints what ran and what each exited, and its
# own exit status is the WORST status any check recorded. The engine derives
# the gate verdict from the exit status (gate_exec.go), so a failing check
# records `fail` instead of a `pass` verdict sitting on top of a captured body
# full of nonzero exits. A failing pre-gate still cannot refuse the claim —
# pregate.go's PG2/PG3 carry the failure into the bundle as data — so the
# verifier sees an honest verdict and the step still runs.

set -euo pipefail

# Root resolution respects the caller's cwd: the engine spawns gates with Dir
# set to the step's worktree, and trust entries invoke this script by absolute
# path into the shared checkout — a script-relative cd would re-point the gate
# at the shared tree. Outside any git repo, fall back to the script's own root.
cd "$(git rev-parse --show-toplevel 2>/dev/null || echo "$(dirname "${BASH_SOURCE[0]}")/../..")"

# Worst status seen so far — a max, never a sum: summed exits wrap at 256
# back to 0, which would mask a failure as success.
worst=0

run() {
  local label="$1"; shift
  echo "--- $label: $* ---"
  local status=0
  # Keep the last 20 lines plus every failure-identifying line wherever it
  # occurs, in original order, each once: a package that fails early in a long
  # `go test ./...` run would otherwise lose its test names to a bare tail.
  # `--- FAIL:` may be indented because go test nests subtest failures;
  # `FAIL` and `panic:` are always printed at column 0. awk reads to EOF, so
  # it never cuts the check short with SIGPIPE.
  # Capture PIPESTATUS[0] in the `||` itself: any intervening command would
  # clobber it, and a bare $? here would be awk's status, not the check's.
  "$@" 2>&1 | awk '
    { line[NR] = $0 }
    /^[ \t]*--- FAIL:|^FAIL|^panic:/ { keep[NR] = 1 }
    END { for (i = 1; i <= NR; i++) if ((i in keep) || i > NR - 20) print line[i] }
  ' || status=${PIPESTATUS[0]}
  echo "[$label] exit $status"
  if [ "$status" -gt "$worst" ]; then
    worst=$status
  fi
  echo
}

echo "=== ac-commands: recorded results for the verifier ==="
echo "commit: $(git rev-parse --short HEAD)"
echo "tree:   $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
echo

# go1.26.6 rather than `go`: that is the binary this toolchain provides, and a
# gate that names a command the environment lacks records "unmatched" and
# teaches the verifier nothing.
run build go1.26.6 build ./...
# Acceptance criteria worded with -count=1 must be settled by a fresh run, not a cached result.
run tests go1.26.6 test -count=1 ./...

echo "=== end ac-commands ==="
exit "$worst"
