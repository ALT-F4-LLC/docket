#!/usr/bin/env bash
#
# gate-coverage-check — the GATE COVERAGE comment block at the top of
# .github/workflows/ci.yaml names, for every script under
# scripts/qa/, whether CI runs it and why not when it doesn't. That block is
# hand-maintained prose; nothing checked it against the directory it
# describes. This script does: every non-test script under
# scripts/qa/ must be named somewhere in the block, and every script named in
# the block must still exist. A script added to scripts/qa/ and forgotten
# here now fails CI instead of silently going unaccounted for.
#
# It checks TWO different claims, because the block makes two:
#
#   ACCOUNTING (the original check) — the set of script NAMES in the block
#   matches the set of files on disk. Answers "is every script spoken for?"
#
#   INVOCATION — where the block says a script "RUNS HERE, by name,
#   in `<job>`", that job must exist in the `jobs:` mapping and must actually
#   invoke that script. Answers "is the claim true?"
#
# Accounting alone was not enough, and the gap was not theoretical: with the
# entire `repo-gates` job deleted from ci.yaml, this script still printed
# `ok (18 scripts accounted for)` and exited 0, while the block went on
# asserting "RUNS HERE, by name, in `repo-gates`: secret-scan.sh,
# self-hygiene.sh". Only the names were pinned, never the invocations — so
# ci.yaml:12-13's promise that the listing "cannot drift silently" held for
# half of what the listing says.
#
# The invocation half derives BOTH of its inputs from the file: the job name
# and the script names come from parsing the block's own RUNS HERE clause,
# and the truth comes from the `jobs:` mapping. Neither is a hand-maintained
# list here, which is AC3 — a second list would just be a third thing
# to drift.
#
# Usage:
#   ./scripts/qa/gate-coverage-check.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CI_YAML="$REPO_ROOT/.github/workflows/ci.yaml"
FAIL=0

fail() {
  echo "gate-coverage-check FAIL: $1" >&2
  FAIL=1
}

if [ ! -f "$CI_YAML" ]; then
  fail "$CI_YAML does not exist"
  exit 1
fi

# The block runs from the `name: ci` line to the `on:` trigger block — the
# comment lines in between are the block's own boundary, not a hand-picked
# line range that would silently stop tracking it if the file grew above it.
BLOCK=$(awk '/^on:/{exit} {print}' "$CI_YAML")

# Every non-test script actually on disk.
DISK_SCRIPTS=$(cd "$REPO_ROOT/scripts/qa" && ls *.sh | grep -v '^test_' | sort)

# Every `.sh` filename token the block itself names. `qa.sh` (singular,
# `scripts/qa.sh`) is the runner the block mentions as the thing INVOKING
# several gates — it lives one directory above scripts/qa/ and is not itself
# a member of the directory this check accounts for.
#
# THE `.sh` MUST END THE TOKEN. Without a trailing boundary, `[A-Za-z0-9_-]+\.sh`
# matches the PREFIX of any longer word: the prose `base.sha` in this block
# scraped as a phantom `base.sh`, and the check then reported "the block names
# a script that no longer exists" for a file nobody had ever written. Any
# `.sh?????` word in the surrounding explanation would do the same. `[^A-Za-z0-9_.-]`
# after the extension (or end of line) confines the match to a real filename
# token.
#
# Comment lines only, additionally: the extent above runs to the `on:` key, so
# any YAML between `name: ci` and `on:` is inside the block too, and the
# listing this check verifies lives entirely in the prose.
BLOCK_SCRIPTS=$(printf '%s\n' "$BLOCK" | grep '^[[:space:]]*#' |
  grep -oE '[A-Za-z0-9_-]+\.sh([^A-Za-z0-9_.-]|$)' |
  sed 's/[^A-Za-z0-9_.-]$//' | grep -v '^qa\.sh$' | sort -u)

MISSING_FROM_BLOCK=$(comm -23 <(printf '%s\n' "$DISK_SCRIPTS") <(printf '%s\n' "$BLOCK_SCRIPTS"))
STALE_IN_BLOCK=$(comm -13 <(printf '%s\n' "$DISK_SCRIPTS") <(printf '%s\n' "$BLOCK_SCRIPTS"))

if [ -n "$MISSING_FROM_BLOCK" ]; then
  fail "scripts/qa/ has a script the GATE COVERAGE block never names: $(printf '%s ' $MISSING_FROM_BLOCK)"
fi

if [ -n "$STALE_IN_BLOCK" ]; then
  fail "the GATE COVERAGE block names a script that no longer exists: $(printf '%s ' $STALE_IN_BLOCK)"
fi

# --- INVOCATION -------------------------------------------------------------
#
# The block's RUNS HERE clause names a job and the scripts it claims that job
# invokes. Both halves are read out of the file rather than restated here.
#
# The clause spans several lines: a `RUNS HERE, by name, in \`<job>\`` opener,
# then parenthetical prose, then an indented list of script names. So the job
# name is taken from the opener and the script names from every line between
# that opener and the next `RUNS`/`EXCLUDED` heading — the block's own
# structure, not a line count.

RUNS_HERE_JOB=$(printf '%s\n' "$BLOCK" |
  sed -n 's/.*RUNS HERE, by name, in `\([A-Za-z0-9_-]*\)`.*/\1/p' | head -1)

if [ -z "$RUNS_HERE_JOB" ]; then
  # The clause is the thing this half verifies. If it is gone or reworded,
  # this check has silently stopped checking anything — which is the exact
  # failure mode this check is about, so it fails rather than passing vacuously.
  fail "the GATE COVERAGE block has no \`RUNS HERE, by name, in \`<job>\`\` clause; the invocation check cannot run"
else
  # The scripts the clause claims that job runs: the lines from the opener up
  # to the next section heading.
  RUNS_HERE_SCRIPTS=$(printf '%s\n' "$BLOCK" |
    awk '/RUNS HERE, by name, in/{inblock=1}
         inblock && /RUNS ELSEWHERE|EXCLUDED/{inblock=0}
         inblock{print}' |
    grep -oE '[A-Za-z0-9_-]+\.sh' | sort -u)

  # The job's own text, from the `jobs:` mapping. A job body runs from its
  # `  <name>:` key to the next key at the same indent — the YAML's own
  # structure. This is a targeted extract, not a YAML parse: the question is
  # only "does this job exist, and does its text invoke this script", and
  # both are answerable from the raw lines without adding a yq dependency to
  # a gate that has none.
  JOB_BODY=$(awk -v job="  $RUNS_HERE_JOB:" '
    $0 == job {injob=1; next}
    injob && /^  [A-Za-z0-9_-]+:/ {exit}
    injob {print}
  ' "$CI_YAML")

  if [ -z "$JOB_BODY" ]; then
    fail "the GATE COVERAGE block says scripts run in the \`$RUNS_HERE_JOB\` job, but ci.yaml's jobs: mapping has no such job"
  else
    # A job body that is entirely comments invokes nothing. Steps are what
    # run, so the check is against the step text, with comment lines dropped
    # — otherwise the `repo-gates` job comment naming the scripts would
    # satisfy the check on its own, and a job stripped to its comments would
    # still pass.
    JOB_STEPS=$(printf '%s\n' "$JOB_BODY" | sed 's/[[:space:]]*#.*//')

    for script in $RUNS_HERE_SCRIPTS; do
      if ! printf '%s\n' "$JOB_STEPS" | grep -qF "$script"; then
        fail "the GATE COVERAGE block says \`$RUNS_HERE_JOB\` runs $script, but that job never invokes it"
      fi
    done
  fi
fi

if [ "$FAIL" -eq 0 ]; then
  echo "gate-coverage-check: ok ($(printf '%s\n' "$DISK_SCRIPTS" | wc -l | tr -d ' ') scripts accounted for)"
fi

exit "$FAIL"
