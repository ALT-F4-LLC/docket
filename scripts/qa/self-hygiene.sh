#!/usr/bin/env bash
#
# self-hygiene — name the CLI surface change so its reference gets updated.
#
# Wired as the `self-hygiene` gate on `implement`. It collects the change set
# under review, narrows it to the gated issue's scope, and reports every
# non-test file under internal/cli/ — where flags, verbs, and help text live —
# so the author and the reviewer both see that the CLI surface moved. It exits
# non-zero only when it cannot measure: an empty or unreadable base, a git that
# refuses. A measured change set, whatever it holds, exits 0.
#
# WHY IT NO LONGER REQUIRES A SKILL.md EDIT. This gate used to fail any
# non-test internal/cli/ change that did not also touch skills/docket/SKILL.md,
# the CLI reference. That file left this repository at a54e3a2; the only copy
# now lives in the dotfiles corpus
# (dotfiles.vorpal.git/main/src/user/claude_code/skills/docket/SKILL.md),
# which `just activate` installs and which no gate here can see or diff. A
# gate that demands an edit to a path that cannot exist in the change set is
# unsatisfiable by hand, so the requirement is retired rather than re-pointed.
# No in-repository reference covers the surface either: README.md's Command
# Reference lists a handful of the top-level verbs and no flags, and `--help`
# text is not generated into any checked-in file. Keeping the reference
# current is therefore the author's to carry into the corpus, and this gate's
# job is to make sure that need is stated in the step's output rather than
# discovered at review.
#
# It deliberately checks nothing else. `build`, `tests`, `genericity`, and
# `secret-scan` are their own gates, and a hygiene check that re-ran them would
# make one failure surface as two.
#
# THREE WAYS TO NAME THE CHANGE SET, in precedence order:
#
#   1. An explicit base-ref argument (CI). A checked-out pull request is a
#      clean tree; the change under review is the COMMITTED diff between the
#      PR's base and its head, `git diff --name-only <base>...HEAD`.
#   2. DOCKET_GATE_BASE (the engine). Executors commit before `step record`,
#      so a completion gate on a worktree-recorded step also runs on a clean
#      tree; the engine exports the step's fork point (internal/engine/gate.go,
#      `Base`) and the change set is `git diff --name-only $DOCKET_GATE_BASE...HEAD`
#      in the gate's own cwd. Before this mode the working-tree scan below ran
#      on that clean tree and reported "no changes" for every step.
#   3. The working-tree scan (an author by hand, before committing): staged,
#      unstaged, and untracked files together.
#
# CALLED WITH NO ARGUMENT vs. CALLED WITH AN EMPTY ONE are different things,
# and `${1:-}` cannot tell them apart. One argument, even an empty one,
# commits to base-ref mode and fails closed rather than silently downgrading
# to the working-tree scan on a clean checkout — see secret-scan.sh's header
# for the full reasoning. DOCKET_GATE_BASE follows the same rule: the engine
# leaves it UNSET when it has no base (a shared-checkout step, the pre-claim
# path), never empty, so a set-but-empty value is a caller defect and fails
# closed the same way.
#
# THE CHANGE-SET DEFINITION DIFFERS FROM secret-scan.sh's, DELIBERATELY.
# This gate uses a two-tree diff, `git diff --name-only A...HEAD`;
# secret-scan.sh walks each commit's own patch,
# `git log -p --diff-merges=first-parent A..HEAD`. secret-scan asks "was a
# credential EVER ADDED in this range?" — a historical question, where an add
# reverted in the next commit is still a leak. This gate asks which files the
# change AS IT WILL LAND touches: a surface change made in one commit and
# reverted in the next changes nothing that a reference would document, and
# reporting it would name a change the step does not make. The two-tree form
# needs no merge-commit handling — `A...HEAD` compares the merge base against
# the final tree, so a conflict resolution is already inside what it compares.

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
elif [ -n "${DOCKET_GATE_BASE+set}" ]; then
  BASE_REF="$DOCKET_GATE_BASE"
  if [ -z "$BASE_REF" ]; then
    echo "self-hygiene FAILED: DOCKET_GATE_BASE is set but empty; refusing to fall" >&2
    echo "back to the working-tree scan, which would pass a committed worktree" >&2
    echo "having scanned nothing." >&2
    exit 1
  fi
else
  BASE_REF=""
fi

if [ -n "$BASE_REF" ]; then
  # Base-ref mode (CI or the engine): the change under review is the committed
  # diff, not the working tree. A single base...HEAD diff has no duplicates to
  # remove, unlike the else branch below.
  if ! changed=$(git diff --name-only "$BASE_REF"...HEAD -- .); then
    echo "self-hygiene FAILED: could not collect the change set; nothing was scanned." >&2
    exit 1
  fi
else
  # --cached is not optional: by hand this runs before any commit, so an
  # author who stages work as they go is ordinary — and a staged file appears
  # in NEITHER a bare working-tree diff NOR the untracked list. A staged
  # internal/cli/ change would go unreported without it.
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

# A CLI surface change is one that touches a non-test file under internal/cli.
# A diff that touches only *_test.go files there changes no flag, verb, or
# help text — it is a refactor or a test addition, and there is nothing for a
# reference to document.
surface=$(printf '%s\n' "$changed" | grep '^internal/cli/' | grep -v '_test\.go$' || true)

if [ -z "$surface" ]; then
  echo "self-hygiene: ok (no CLI surface change)"
  exit 0
fi

count=$(printf '%s\n' "$surface" | wc -l | tr -d ' ')
echo "self-hygiene: CLI surface changed — $count non-test file(s) under internal/cli:"
printf '%s\n' "$surface" | sed 's/^/  /'
cat <<'EOF'
The CLI reference (skills/docket/SKILL.md) lives in the dotfiles corpus, not
in this repository, so this gate cannot check it. If this change adds,
removes, or renames a flag, verb, or help text, update the corpus copy in the
same session; if it alters nothing documented, say so in the step's report.
EOF
