#!/usr/bin/env bash
#
# secret-scan — refuse a change that adds a credential to the tree.
#
# Wired as the `secret-scan` gate on `implement` and `fix`. Without
# it, a committed credential is caught only by a judge who happens to read that
# hunk — a reviewer's attention standing in for a mechanical check.
#
# Scope is the working tree's own ADDED lines: a gate runs while a step holds
# the tree, so the question is "did this step add a secret". A secret already
# in the file is not this step's doing, and failing on it would make the gate
# unclearable for every later step that touches the file.
#
# CI has no staged or unstaged state to read — a checked-out pull
# request is a clean tree, and the change under review lives only in the
# COMMITTED diff between the PR's base and its head. So this script also
# accepts an optional base-ref argument: with one, "added" means an added line
# in ANY commit between the base and HEAD (a credential added then removed
# inside the same PR still trips this, deliberately — see collect() below);
# without one, behavior is unchanged from the working-tree scan the
# `implement`/`fix` gates use.
#
# CALLED WITH NO ARGUMENT vs. CALLED WITH AN EMPTY ONE are different things,
# and `${1:-}` cannot tell them apart — both read as "". A CI caller that
# passes an interpolated GitHub Actions expression (ci.yaml) means "scan this
# committed diff"; if that expression ever resolves empty (a workflow trigger
# with no PR base — merge_group, workflow_dispatch, a future widening of
# `on:`), silently falling back to the working-tree scan would pass a clean CI
# checkout having inspected nothing. So the check below is on `$#`, the
# argument COUNT, not the value: one argument, even an empty one, commits to
# CI mode and fails closed rather than downgrading to legacy mode.
#
# THE CHANGE-SET DEFINITION DIFFERS FROM self-hygiene.sh's, DELIBERATELY
# (AC2). This gate walks each commit's own patch
# (`git log -p --diff-merges=first-parent A..HEAD`); self-hygiene.sh uses a
# two-tree diff (`git diff --name-only A...HEAD`). Both headers state the
# difference so neither reads as an oversight.
#
# The reason is that the two gates ask different questions. This one asks
# "was a credential EVER ADDED in this range?", which is historical: a
# credential added in one commit and removed in the next is still a leak —
# it is in the reflog, in any fetched copy, and in GitHub's API. Only a
# per-commit walk sees it. self-hygiene asks whether the change AS IT WILL
# LAND keeps SKILL.md in step with internal/cli, which is a question about
# the end state; see that script's header for why a per-commit walk would
# make it fail on correct work.
#
# KNOWN LIMIT: base-ref mode diffs against the passed ref directly rather than
# an explicit `git merge-base`. `git log A..B` already anchors at the
# merge-base for an ordinary PR (A is an ancestor of B), so this only widens
# if the checked-out ref diverges from the PR's own history — which is
# exactly what a stale `base.sha` on a rebased base branch would do (see
# ci.yaml's `gates` job comment for the caller-side half of this same
# limit). INFERRED risk, not reproduced against a real GitHub Actions run.

set -euo pipefail

# Root resolution respects the caller's cwd: the engine spawns gates with Dir
# set to the step's worktree, and trust entries invoke this script by absolute
# path into the shared checkout — a script-relative cd would re-point the gate
# at the shared tree. Outside any git repo, fall back to the script's own root.
cd "$(git rev-parse --show-toplevel 2>/dev/null || echo "$(dirname "${BASH_SOURCE[0]}")/../..")"

if [ "$#" -gt 0 ]; then
  BASE_REF="$1"
  if [ -z "$BASE_REF" ]; then
    echo "secret-scan FAILED: a base-ref argument was passed but empty; refusing" >&2
    echo "to fall back to the working-tree scan, which would pass a clean CI" >&2
    echo "checkout having scanned nothing." >&2
    exit 1
  fi
else
  BASE_REF=""
fi

# FAIL CLOSED if the tree cannot be read. Every line below is derived from git,
# so a git that refuses leaves `added` empty — and an empty scan reports
# "clean", which is a security gate saying PASS precisely when it inspected
# nothing. Refusing here costs a clear error; the alternative costs silence.
if ! git rev-parse --git-dir >/dev/null 2>&1; then
  echo "secret-scan FAILED: not a git repository, so nothing can be scanned." >&2
  exit 1
fi

# High-signal shapes only. A pattern that fires on ordinary code trains people
# to route around the gate, so the bar is that a true positive is far likelier
# than a false one.
# `github_pat_` is separate from `gh[pousr]_`: fine-grained PATs (2022 onward)
# use a different prefix entirely, so a list claiming GitHub coverage without it
# has a gap in its OWN stated scope rather than a deliberate scope choice.
PATTERNS='AKIA[0-9A-Z]{16}
gh[pousr]_[A-Za-z0-9]{36,}
github_pat_[A-Za-z0-9_]{40,}
xox[abprs]-[0-9A-Za-z-]{10,}
sk-ant-[A-Za-z0-9_-]{20,}
AIza[0-9A-Za-z_-]{35}
-----BEGIN [A-Z ]*PRIVATE KEY-----'

# Added lines, tracked and untracked. Untracked matters because a brand-new
# file is exactly where a credential lands and `git diff` cannot see one.
#
# `-z` and `read -d ''` are load-bearing, not style. Plain `git ls-files`
# QUOTES a name containing a tab, newline, or non-ASCII byte, and the quoted
# form does not name a real file — so `xargs -I{} sed` fails on it and that
# file is NEVER SCANNED. A credential in a file named with a tab would have
# passed this gate silently. Null-delimited output is unambiguous and needs no
# unquoting. (Measured 2026-08-06: a `weird<TAB>tab.txt` fixture skipped the
# scan entirely under the previous xargs form.)
collect() {
  # `|| true` here is scoped to GREP ALONE, which exits 1 on "no matching
  # lines" — an ordinary outcome, not an error. A git failure still propagates.
  if [ -n "$BASE_REF" ]; then
    # CI mode: the change under review is the committed diff, not the working
    # tree — a checked-out PR has nothing staged or unstaged to find.
    #
    # `git log -p A..B`, NOT `git diff A...B`. A two-tree diff compares only
    # the base and head SNAPSHOTS, so a credential added in one commit and
    # removed by a later commit of the same PR is invisible to it — the two
    # trees never differ on that line. Walking each commit's own patch catches
    # it: the ADD is a line in that commit's diff regardless of what a later
    # commit does to it. `..`, not `...`: for `git log` (unlike `git diff`),
    # `A...B` means the SYMMETRIC difference (commits reachable from either
    # side but not both) — `A..B` is "commits new to this branch", which is
    # the question here.
    #
    # `--diff-merges=first-parent` IS LOAD-BEARING. `git log -p`
    # defaults to `--diff-merges=off`, so a MERGE COMMIT contributes no patch
    # at all — and a credential written during conflict resolution exists only
    # in the merge commit, in neither parent. It is in the merged tree and the
    # scan never saw it. This matters most on `pull_request`, where the
    # checked-out HEAD IS GitHub's synthetic merge commit.
    #
    # Measured before the flag: a fixture whose merge resolution wrote a
    # credential reported `secret-scan: clean`, exit 0, with the credential
    # present in the merged tree and absent from both parents.
    #
    # `first-parent`, not `remerge` or `separate`: it shows everything the
    # merge brought INTO the branch under review, which is exactly the set
    # that ends up in the merged tree. `remerge` shows only what the resolver
    # changed relative to an automatic merge, so a credential arriving
    # untouched from a side branch would slip past; `separate` emits one patch
    # per parent, double-counting ordinary merges into noise. This choice
    # deliberately errs toward scanning more than strictly necessary — a
    # security gate's false positive costs a look, a false negative costs a
    # leak.
    git log --unified=0 -p --diff-merges=first-parent "$BASE_REF"..HEAD -- . |
      { grep '^+' | grep -v '^+++' || true; }
    return
  fi
  # THREE sources, because a change hides in any of them. STAGED IS THE ONE
  # THAT MATTERS MOST: this gate runs on `implement`, before any commit, so an
  # executor that stages its work as it goes is ordinary — and a staged file
  # is absent from BOTH a bare working-tree diff and the untracked
  # enumeration. The secret would fall straight through the gap between them.
  git diff --cached --unified=0 -- . | { grep '^+' | grep -v '^+++' || true; }
  git diff --unified=0 -- .          | { grep '^+' | grep -v '^+++' || true; }
  while IFS= read -r -d '' f; do
    # `./` rather than `--`: BSD sed (macOS) does not accept `--` as an
    # end-of-options marker. The prefix keeps a name beginning with `-` from
    # parsing as a flag.
    [ -f "$f" ] && sed 's/^/+/' "./$f"
  done < <(git ls-files --others --exclude-standard -z)
}

# NO BLANKET `|| true`. That was the root defect behind three separate
# false-negative reports: it turned every collection failure — an aborted
# enumeration, a wrong tree state, a git that refused — into an empty result,
# which the emptiness check below then reported as "clean". A scanner that
# cannot collect its input must FAIL, not pass.
if ! added=$(collect); then
  echo "secret-scan FAILED: could not collect the change set; nothing was scanned." >&2
  exit 1
fi

if [ -z "$added" ]; then
  echo "secret-scan: no added lines to scan"
  exit 0
fi

# The matching line is never printed: it would land in the gate's recorded
# verdict, the event log, and every transcript quoting them — the gate would
# become the leak. The count is enough to go looking.
if hits=$(printf '%s\n' "$added" | grep -cE "$PATTERNS") && [ "$hits" -gt 0 ]; then
  echo "secret-scan FAILED: $hits added line(s) match a credential pattern." >&2
  echo "Remove it and re-run. If it is a fixture, make it unmistakably fake." >&2
  exit 1
fi

echo "secret-scan: clean"
