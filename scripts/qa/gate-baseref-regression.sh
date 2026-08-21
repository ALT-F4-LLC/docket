#!/usr/bin/env bash
#
# gate-baseref-regression — pins the base-ref mode of secret-scan.sh and
# self-hygiene.sh, which had zero committed tests before this fix
# round and a fail-open bug in exactly that mode: an explicit but empty
# base-ref argument must fail closed rather than silently falling back to the
# (vacuous, on a clean CI checkout) working-tree scan.
#
# Runs each gate script against a throwaway git repo shaped like a real PR, so
# every case here drives the actual entry point CI invokes — not a helper
# function in isolation. Invoked as a standing gate from qa.sh, same as
# genericity.sh: a single PASS/FAIL per case, reported through this script's
# own exit code and stderr.
#
# Usage:
#   ./scripts/qa/gate-baseref-regression.sh

set -euo pipefail

# Hermetic git identity for every command below, including the ones the gate
# scripts under test run on their own — the same idiom
# internal/engine/gitdiff_test.go uses for the same reason. Without this, the
# fixture inherits the operator's global git config (commit.gpgsign, hooks,
# etc.); on a machine with a signing key configured but no reachable signer,
# every commit below aborts and this standing gate fails for a reason that has
# nothing to do with what it pins.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
FAIL=0

fail() {
  echo "gate-baseref-regression FAIL: $1" >&2
  FAIL=1
}

# A throwaway repo, shaped like the real one only where it matters: the gate
# scripts `cd` to two directories up from their own path, so they need to live
# at <repo>/scripts/qa/<script>.sh inside the fixture too.
# Template form, deliberately, matching genericity.sh after 86c5633:
# bare `mktemp -d` on macOS ignores TMPDIR and uses the confstr per-user temp
# dir, which a sandboxed environment may deny — and this script is a STANDING
# gate, so that denial aborts the whole qa.sh suite before case 1 rather than
# failing one check. TMPDIR is on the gate env allowlist
# (internal/exec/env.go), so honor it when set.
WORK=$(mktemp -d "${TMPDIR:-/tmp}/gate-baseref-regression.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/repo/scripts/qa"
cp "$REPO_ROOT/scripts/qa/secret-scan.sh" "$WORK/repo/scripts/qa/"
cp "$REPO_ROOT/scripts/qa/self-hygiene.sh" "$WORK/repo/scripts/qa/"

git -C "$WORK/repo" init -q
git -C "$WORK/repo" config user.email "qa@example.invalid"
git -C "$WORK/repo" config user.name "qa"

echo "hello" > "$WORK/repo/readme.txt"
git -C "$WORK/repo" add -A
git -C "$WORK/repo" commit -q -m "base"
BASE=$(git -C "$WORK/repo" rev-parse HEAD)

run_scan() {
  # $1: script, remaining args passed through.
  local script="$1"; shift
  (cd "$WORK/repo" && bash "scripts/qa/$script" "$@")
}

# --- C1: an explicit but empty base-ref argument must fail closed, not fall
# back to the (vacuous, on a clean checkout) working-tree scan. -------------

C1_OUT="$WORK/c1-secret.out"
if run_scan secret-scan.sh "" >"$C1_OUT" 2>&1; then
  fail "secret-scan.sh '' exited 0; an empty base-ref must fail closed (C1)"
elif ! grep -q "refusing" "$C1_OUT"; then
  fail "secret-scan.sh '' failed for the wrong reason: $(cat "$C1_OUT")"
fi

C1_HYGIENE_OUT="$WORK/c1-hygiene.out"
if run_scan self-hygiene.sh "" >"$C1_HYGIENE_OUT" 2>&1; then
  fail "self-hygiene.sh '' exited 0; an empty base-ref must fail closed (C1)"
elif ! grep -q "refusing" "$C1_HYGIENE_OUT"; then
  fail "self-hygiene.sh '' failed for the wrong reason: $(cat "$C1_HYGIENE_OUT")"
fi

# --- legacy (no-argument) mode is unaffected. --------------------------------

if ! run_scan secret-scan.sh >/dev/null 2>&1; then
  fail "secret-scan.sh with no argument (legacy mode) failed on a clean tree"
fi
if ! run_scan self-hygiene.sh >/dev/null 2>&1; then
  fail "self-hygiene.sh with no argument (legacy mode) failed on a clean tree"
fi

# --- K6: legacy mode must actually SEE a staged credential, not merely exit 0
# on a clean tree. The two cases above both pass "for free" if collect()'s
# staged-diff line is gutted — this pins the line itself, the one the
# script's own comment calls "the one that matters most" (secret-scan.sh
# collect()), by driving the real no-argument entry point with something
# staged and never committed. -------------------------------------------

K6_PREFIX="AKIA"
K6_BODY="QQQQQQQQQQQQQQQQ"
echo "staged = \"${K6_PREFIX}${K6_BODY}\"" > "$WORK/repo/staged-leak.txt"
git -C "$WORK/repo" add staged-leak.txt

if run_scan secret-scan.sh >/dev/null 2>&1; then
  fail "secret-scan.sh legacy mode passed a staged (uncommitted) credential (K6)"
fi

git -C "$WORK/repo" reset -q staged-leak.txt
rm -f "$WORK/repo/staged-leak.txt"

# --- a clean committed diff against a real base ref passes. -----------------

echo "world" >> "$WORK/repo/readme.txt"
git -C "$WORK/repo" add -A
git -C "$WORK/repo" commit -q -m "feature"

if ! run_scan secret-scan.sh "$BASE" >/dev/null 2>&1; then
  fail "secret-scan.sh \$BASE failed on a clean feature commit"
fi
if ! run_scan self-hygiene.sh "$BASE" >/dev/null 2>&1; then
  fail "self-hygiene.sh \$BASE failed on a feature commit outside internal/cli"
fi

# --- C1's control: a real credential in the PR's diff must still fail. ------
#
# Built from two halves on separate lines rather than one literal, so THIS
# SCRIPT'S OWN source line never matches the AKIA[0-9A-Z]{16} pattern it is
# testing — a fixture credential that matches its own detector fails
# secret-scan on the commit that adds this file (K1). The produced fixture
# line is byte-identical to the literal at runtime; only the source shape
# changes.
AKIA_PREFIX="AKIA"
AKIA_BODY="ABCDEFGHIJKLMNOP"
echo "token = \"${AKIA_PREFIX}${AKIA_BODY}\"" >> "$WORK/repo/leak.txt"
git -C "$WORK/repo" add -A
git -C "$WORK/repo" commit -q -m "leak"

if run_scan secret-scan.sh "$BASE" >/dev/null 2>&1; then
  fail "secret-scan.sh \$BASE passed a PR diff containing a live-looking credential"
fi

# --- C5: a credential added then removed within the same PR's commit range
# must still fail — a two-tree diff (base...HEAD) cannot see it; only a
# per-commit walk can. ---------------------------------------------------

git -C "$WORK/repo" rm -q leak.txt
git -C "$WORK/repo" commit -q -m "remove the leak"

if run_scan secret-scan.sh "$BASE" >/dev/null 2>&1; then
  fail "secret-scan.sh \$BASE missed a credential added then removed within the PR (C5)"
fi

# --- self-hygiene: an internal/cli change with no matching SKILL.md update
# in the same committed diff must fail, in base-ref mode too. ---------------

mkdir -p "$WORK/repo/internal/cli"
echo "package cli" > "$WORK/repo/internal/cli/root.go"
git -C "$WORK/repo" add -A
git -C "$WORK/repo" commit -q -m "cli surface change, no SKILL.md update"

if run_scan self-hygiene.sh "$BASE" >/dev/null 2>&1; then
  fail "self-hygiene.sh \$BASE passed an internal/cli change with no SKILL.md update"
fi

mkdir -p "$WORK/repo/skills/docket"
echo "# SKILL" > "$WORK/repo/skills/docket/SKILL.md"
git -C "$WORK/repo" add -A
git -C "$WORK/repo" commit -q -m "update SKILL.md"

if ! run_scan self-hygiene.sh "$BASE" >/dev/null 2>&1; then
  fail "self-hygiene.sh \$BASE failed once SKILL.md was updated in the same PR"
fi

# --- K7: a credential landed in the BASE commit itself — before the range
# under test starts — must NOT trip base-ref mode. Every case above passes
# for two reasons at once (correct range scanning, AND a base commit that
# never had a credential to begin with); this pins that base-ref mode scans
# only the RANGE, not all reachable history. Widening `git log A..HEAD` to
# `git log HEAD` (all history) would still pass every earlier case here but
# fails this one, because the credential this fixture puts in the base is
# outside the true range. ---------------------------------------------------

K7_PREFIX="AKIA"
K7_BODY="ZZZZZZZZZZZZZZZZ"
echo "old = \"${K7_PREFIX}${K7_BODY}\"" >> "$WORK/repo/old-secret.txt"
git -C "$WORK/repo" add -A
git -C "$WORK/repo" commit -q -m "a credential that landed before this PR's base"
BASE2=$(git -C "$WORK/repo" rev-parse HEAD)

echo "unrelated clean work" >> "$WORK/repo/readme.txt"
git -C "$WORK/repo" add -A
git -C "$WORK/repo" commit -q -m "clean feature work on top of the old credential"

if ! run_scan secret-scan.sh "$BASE2" >/dev/null 2>&1; then
  fail "secret-scan.sh \$BASE2 failed on a range with no new credential — only one already in the base (K7)"
fi

# --- A credential introduced by a MERGE COMMIT's conflict resolution
# must be caught. `git log -p` defaults to `--diff-merges=off`, so a merge
# commit contributes no patch and such a credential is invisible — while
# being present in the merged tree. On `pull_request` the checked-out HEAD IS
# GitHub's synthetic merge commit, so this is the common case there, not an
# exotic one.
#
# Uses its own throwaway repo: the fixture above is a linear history, and
# giving it a merge would change what every earlier case is testing. ------

MERGE_REPO="$WORK/merge-repo"
mkdir -p "$MERGE_REPO/scripts/qa"
cp "$REPO_ROOT/scripts/qa/secret-scan.sh" "$MERGE_REPO/scripts/qa/"

git -C "$MERGE_REPO" init -q -b main
git -C "$MERGE_REPO" config user.email "qa@example.invalid"
git -C "$MERGE_REPO" config user.name "qa"

echo "base" > "$MERGE_REPO/f.txt"
git -C "$MERGE_REPO" add -A
git -C "$MERGE_REPO" commit -q -m "base"

# Two branches that conflict on the same line, so the merge REQUIRES a manual
# resolution — which is where the credential goes.
git -C "$MERGE_REPO" checkout -q -b side
echo "side edit" > "$MERGE_REPO/f.txt"
git -C "$MERGE_REPO" commit -qam "side edit"

git -C "$MERGE_REPO" checkout -q main
echo "main edit" > "$MERGE_REPO/f.txt"
git -C "$MERGE_REPO" commit -qam "main edit"
MERGE_BASE_REF=$(git -C "$MERGE_REPO" rev-parse HEAD)

git -C "$MERGE_REPO" merge -q side >/dev/null 2>&1 || true
# Same two-halves shape as the fixture above, for the same reason: no single
# source line here may match the pattern this script is testing.
MERGE_PREFIX="AKIA"
MERGE_BODY="MNOPQRSTUVWXYZAB"
printf 'resolved = "%s%s"\n' "$MERGE_PREFIX" "$MERGE_BODY" > "$MERGE_REPO/f.txt"
git -C "$MERGE_REPO" add f.txt
git -C "$MERGE_REPO" commit -q --no-edit

# Guard the FIXTURE ITSELF: if this ever stops being a real merge commit, the
# case below would pass for the wrong reason and pin nothing.
MERGE_PARENTS=$(git -C "$MERGE_REPO" rev-list --parents -n1 HEAD | wc -w | tr -d ' ')
if [ "$MERGE_PARENTS" -ne 3 ]; then
  fail "the DKT-10 fixture's HEAD is not a merge commit; the case pins nothing"
fi

if (cd "$MERGE_REPO" && bash scripts/qa/secret-scan.sh "$MERGE_BASE_REF") >/dev/null 2>&1; then
  fail "secret-scan.sh missed a credential introduced by a merge commit's conflict resolution (DKT-10)"
fi

# --- C15: an unreadable base ref must fail closed with a diagnostic naming
# the gate and "nothing was scanned", not just git's raw fatal (exit 128). ---

C15_OUT="$WORK/c15-hygiene.out"
if run_scan self-hygiene.sh deadbeefdeadbeefdeadbeefdeadbeefdeadbeef >"$C15_OUT" 2>&1; then
  fail "self-hygiene.sh with an unreadable base ref exited 0 (C15)"
elif ! grep -q "nothing was scanned" "$C15_OUT"; then
  fail "self-hygiene.sh with an unreadable base ref gave no diagnostic: $(cat "$C15_OUT")"
fi

if [ "$FAIL" -eq 0 ]; then
  echo "gate-baseref-regression: ok"
fi

exit "$FAIL"
