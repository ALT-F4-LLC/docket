#!/usr/bin/env bash
#
# self-hygiene — the author's own check, before a reviewer spends attention.
#
# Wired as the `self-hygiene` gate on `implement`. It mechanizes the
# ONE bar docs/spec/review-strategy.md §4 states that no other gate covers: a
# CLI surface change must update skills/docket/SKILL.md in the same change,
# because "a stale table is drift and blocks review". (This used to cite
# CLAUDE.md, deleted at 408a294 — the rule moved, and this citation now points
# at the spec section that actually states it.)
#
# SKILL.md HAS A TWIN, AND THIS GATE CANNOT SEE IT. The corpus copy at
# dotfiles.vorpal.git/main/src/user/claude_code/skills/docket/SKILL.md is what
# `just activate` installs and what every session actually executes; this
# repo's copy is the one that moves first, because this gate makes a CLI
# surface change update it. Satisfying this gate therefore leaves the
# installed copy stale until the same edit lands there too — the observed
# direction of every drift so far. That repo's tests/docket-skill-sync.test.sh
# byte-diffs the pair and fails on any difference; it is not wired here
# because CI checks out only this repository, so the check would find no
# second copy and SKIP unconditionally. Convergence is the author's to carry:
# land the edit in BOTH copies, same session.
#
# It deliberately checks nothing else. `build`, `tests`, `genericity`, and
# `secret-scan` are their own gates, and a hygiene check that re-ran them would
# make one failure surface as two.
#
# CI has no staged or unstaged state to read — a checked-out pull
# request is a clean tree, and the change under review lives only in the
# COMMITTED diff between the PR's base and its head. So this script also
# accepts an optional base-ref argument: with one, "changed" means the files a
# `git diff <base>...HEAD` names; without one, behavior is unchanged from the
# working-tree scan the `implement` gate uses.
#
# CALLED WITH NO ARGUMENT vs. CALLED WITH AN EMPTY ONE are different things,
# and `${1:-}` cannot tell them apart. One argument, even an empty one,
# commits to CI mode and fails closed rather than silently downgrading to the
# working-tree scan on a clean checkout — see secret-scan.sh's header for the
# full reasoning (same defect, same fix).
#
# THE CHANGE-SET DEFINITION DIFFERS FROM secret-scan.sh's, DELIBERATELY
# (AC2). This gate uses a two-tree diff, `git diff --name-only
# A...HEAD`; secret-scan.sh walks each commit's own patch,
# `git log -p --diff-merges=first-parent A..HEAD`. Both headers state the
# difference so neither reads as an oversight.
#
# The reason is that the two gates ask different questions:
#
#   secret-scan asks "was a credential EVER ADDED in this range?" — a
#   historical question. A credential added in one commit and removed in the
#   next is still a leak: it is in the reflog, in any fetched copy, and in
#   GitHub's API. So it must see every intermediate state, which only a
#   per-commit walk gives.
#
#   self-hygiene asks "does the change AS IT WILL LAND keep SKILL.md in step
#   with internal/cli?" — a question about the end state. If a PR touches
#   internal/cli in one commit and reverts it in the next, the merged result
#   documents nothing new and there is nothing to update. A per-commit walk
#   would demand a SKILL.md edit for a surface change the PR does not
#   actually make, which is a gate failing on correct work — the failure mode
#   that teaches people to route around it.
#
# So the asymmetry is the point: a security gate errs toward seeing more, a
# documentation gate toward matching what ships. Note the two-tree form needs
# no merge-commit handling — `A...HEAD` compares the merge base against the
# final tree, so a conflict resolution is already inside what it compares.

set -euo pipefail

# Root resolution respects the caller's cwd: the engine spawns gates with Dir
# set to the step's worktree, and trust entries invoke this script by absolute
# path into the shared checkout — a script-relative cd would re-point the gate
# at the shared tree. Outside any git repo, fall back to the script's own root.
cd "$(git rev-parse --show-toplevel 2>/dev/null || echo "$(dirname "${BASH_SOURCE[0]}")/../..")"

if [ "$#" -gt 0 ]; then
  BASE_REF="$1"
  if [ -z "$BASE_REF" ]; then
    echo "self-hygiene FAILED: a base-ref argument was passed but empty; refusing" >&2
    echo "to fall back to the working-tree scan, which would pass a clean CI" >&2
    echo "checkout having scanned nothing." >&2
    exit 1
  fi
else
  BASE_REF=""
fi

if [ -n "$BASE_REF" ]; then
  # CI mode: the change under review is the committed diff, not the working
  # tree — a checked-out PR has nothing staged or unstaged to find. A single
  # base...HEAD diff has no duplicates to remove, unlike the else branch below.
  if ! changed=$(git diff --name-only "$BASE_REF"...HEAD -- .); then
    echo "self-hygiene FAILED: could not collect the change set; nothing was scanned." >&2
    exit 1
  fi
else
  # --cached is not optional: this gate also runs on `implement`, before any
  # commit, so an executor that stages its work as it goes is ordinary — and a
  # staged file appears in NEITHER a bare working-tree diff NOR the untracked
  # list. A staged internal/cli/ change would pass this gate silently without
  # it.
  #
  # EACH command is checked SEPARATELY, and none of them runs inside the `if`
  # condition. The previous form was
  #
  #   if ! changed=$( { git diff --cached ...; git ...; } | sort -u ); then
  #
  # which took its status from `sort` — the LAST element of the pipeline —
  # so a `git` failure anywhere in the group was masked, and `set -e` is
  # suppressed inside an `if` condition anyway. The guard could not fire.
  # Measured: with the FIRST command forced to exit 128, the old form
  # collected the two surviving sources, reported success, and scanned a
  # change set with every STAGED file missing — the one source this gate's
  # own comment above calls non-optional.
  #
  # Assigning each source to its own variable, outside any condition, is what
  # makes `set -e` apply again. `|| exit_with` on each keeps the diagnostic
  # specific about which source failed rather than reporting a generic
  # collection error for any of the three.
  collect_failed() {
    echo "self-hygiene FAILED: $1 failed; nothing was scanned." >&2
    exit 1
  }

  staged=$(git diff --cached --name-only -- .) ||
    collect_failed "git diff --cached"
  unstaged=$(git diff --name-only -- .) ||
    collect_failed "git diff"
  untracked=$(git ls-files --others --exclude-standard) ||
    collect_failed "git ls-files"

  changed=$(printf '%s\n%s\n%s\n' "$staged" "$unstaged" "$untracked" |
    grep -v '^$' | sort -u || true)
fi

# DKT-63: under the engine, the change set narrows to THE GATED ISSUE'S
# declared scope. The engine exports DOCKET_SCOPE (newline-separated globs)
# for the step being gated; without it — a person running this by hand, CI —
# nothing changes. Per-step gates over a SHARED tree otherwise see every
# issue's in-flight edits: in one run, an already-adjudicated internal/cli
# change failed this gate for the next two unrelated writers, parking the run
# each time on a fact an operator had already weighed.
if [ -n "${DOCKET_SCOPE:-}" ]; then
  scoped=""
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    while IFS= read -r glob; do
      [ -n "$glob" ] || continue
      # An unquoted case pattern is the glob match itself; `*` crosses `/` in
      # case patterns, so `internal/cli/**` and `internal/cli/*` both cover
      # nested paths.
      case "$f" in
        $glob) scoped="${scoped}${f}"$'\n'; break ;;
      esac
    done <<< "$DOCKET_SCOPE"
  done <<< "$changed"
  changed=$(printf '%s' "$scoped" | sort -u)
  if [ -z "$changed" ]; then
    echo "self-hygiene: no changes within this issue's declared scope"
    exit 0
  fi
fi

if [ -z "$changed" ]; then
  echo "self-hygiene: no changes"
  exit 0
fi

# A CLI surface change is one that touches internal/cli — where flags, verbs,
# and their help text live. That is what SKILL.md's tables document.
#
# DKT-70 retro candidate 1: a diff that touches internal/cli/ but ONLY
# *_test.go files there changes no flag, verb, or help text — it is an
# internal refactor or a test addition, and this gate's own failure message
# already told the operator to say so and let the reviewer weigh it. That
# escape hatch shouldn't be needed for the mechanical case: if every
# internal/cli/ file in the (possibly scope-narrowed) change set is a test
# file, there is nothing for SKILL.md to document, so skip straight to ok.
if printf '%s\n' "$changed" | grep '^internal/cli/' | grep -qv '_test\.go$'; then
  if ! printf '%s\n' "$changed" | grep -q '^skills/docket/SKILL.md$'; then
    cat >&2 <<'EOF'
self-hygiene FAILED: internal/cli changed but skills/docket/SKILL.md did not.

docs/spec/review-strategy.md §4 makes this a PR bar: SKILL.md documents the
CLI, and its flag/verb tables must be updated in the same change as any
surface change — a stale table is drift and blocks review.

If this change genuinely alters no documented flag or verb (an internal
refactor, a comment, a test), say so in the step's report and the reviewer can
weigh it.
EOF
    exit 1
  fi
fi

echo "self-hygiene: ok"
