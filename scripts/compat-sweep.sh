#!/usr/bin/env bash
#
# Byte-compatibility sweep against a baseline binary.
#
# Usage:
#   ./scripts/compat-sweep.sh <baseline-binary> [current-binary]
#
# Proves engine-spec.md §9 item 8 for a repo that never uses the new stage's
# features: "all existing verbs byte-compatible without --json=v2; a
# workflow-free repo shows no behavioral change on any existing verb."
#
# Method: build one fixture database, copy it, run every existing read verb
# under BOTH binaries against IDENTICAL database state, and diff byte-for-byte.
# Non-empty output is asserted alongside the zero diff — two empty strings
# compare equal and would prove nothing.
#
# Each stage re-runs this against the previous stage's tag. The sweep lives in
# a script rather than inline in one stage's notes precisely so later stages
# inherit the obligation rather than reinventing it.

set -euo pipefail

BASELINE="${1:?usage: compat-sweep.sh <baseline-binary> [current-binary]}"
CURRENT="${2:-}"

if [ ! -x "$BASELINE" ]; then
  printf "FATAL: baseline binary %s is not executable\n" "$BASELINE"
  exit 1
fi

if [ -z "$CURRENT" ]; then
  CURRENT=$(mktemp -u /tmp/docket-compat-current.XXXXXX)
  go build -o "$CURRENT" ./cmd/docket
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0

# --- Build the fixture with the BASELINE binary --------------------------
#
# Seeded by the old binary specifically: this is a database as it existed
# before the new stage, which is the state a real upgrading user has.

SEED="$WORK/seed"
mkdir -p "$SEED"
export DOCKET_PATH="$SEED"

"$BASELINE" init >/dev/null 2>&1

"$BASELINE" issue create -t "Compat root" -d "root issue" -p high -T feature >/dev/null
"$BASELINE" issue create -t "Compat child" -d "child issue" --parent DKT-1 >/dev/null
"$BASELINE" issue create -t "Compat third" -p low -T bug >/dev/null
"$BASELINE" issue label add DKT-1 backend >/dev/null
"$BASELINE" issue label add DKT-2 frontend >/dev/null
"$BASELINE" issue link add DKT-2 blocks DKT-3 >/dev/null
"$BASELINE" issue comment add DKT-1 -m "a comment" >/dev/null
"$BASELINE" issue file add DKT-1 src/main.go >/dev/null
"$BASELINE" issue move DKT-3 in-progress >/dev/null
"$BASELINE" doc create -t "Compat doc" -T tdd -d "doc body" >/dev/null
"$BASELINE" doc link add DOC-1 --issue DKT-1 >/dev/null
"$BASELINE" vote create -d "Compat proposal" -n 2 >/dev/null

# --- The verb matrix -----------------------------------------------------
#
# Every existing READ verb, in both human and v1-JSON mode. Read verbs only:
# a mutating verb would diverge the two databases and make later comparisons
# meaningless. Their output shapes are covered by the QA suite's golden checks.

VERBS=(
  "issue list"
  "issue list --all"
  "issue list --json"
  "issue list --all --json"
  "issue list --status todo --json"
  "issue list --priority high --json"
  "issue list --label backend --json"
  "issue list --type bug --json"
  "issue show DKT-1"
  "issue show DKT-1 --json"
  "issue show DKT-2 --json"
  "issue show DKT-3 --json"
  "issue log DKT-1 --json"
  "issue log DKT-3 --json"
  "issue comment list DKT-1 --json"
  "issue label list --json"
  "issue link list DKT-2 --json"
  "issue file list DKT-1 --json"
  "issue graph DKT-1 --json"
  "doc list --json"
  "doc show DOC-1 --json"
  "doc list"
  "vote list --json"
  "vote show DKT-V1 --json"
  "next --json"
  "next"
  "plan --json"
  "board --json"
  "board"
  "stats --json"
  "stats"
  "export --json"
  "config --json"
  "version"
)

record() {
  local id="$1" result="$2" detail="${3:-}"
  if [ "$result" = "PASS" ]; then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
    printf "  FAIL %s: %s\n" "$id" "$detail"
  fi
}

# Human-mode tables render to terminal width and colorize by TTY detection.
# Both are pinned so a width or color difference between two runs cannot be
# mistaken for an output-format change.
export COLUMNS=100
export NO_COLOR=1

printf "=== Byte-compatibility sweep ===\n"
printf "baseline: %s\n" "$BASELINE"
printf "current:  %s\n\n" "$CURRENT"

RUN_DIR="$WORK/run"

for verb in "${VERBS[@]}"; do
  # Both binaries run against the SAME directory path, sequentially, each on a
  # fresh copy of the seeded database. Using one path matters: `config` echoes
  # its database path, so two directories would diff on the path itself and
  # report a difference that is an artifact of the harness rather than a real
  # incompatibility.
  #
  # The baseline reads the v5 database; the current binary then migrates its
  # own fresh copy to v6 on open and must still read identically. That is
  # exactly the scenario under test: a user's existing database, upgraded.
  rm -rf "$RUN_DIR"
  mkdir -p "$RUN_DIR"
  cp "$SEED/issues.db" "$RUN_DIR/issues.db"

  set +e
  OLD_OUT=$(DOCKET_PATH="$RUN_DIR" $BASELINE $verb 2>/dev/null)
  OLD_RC=$?
  set -e

  rm -rf "$RUN_DIR"
  mkdir -p "$RUN_DIR"
  cp "$SEED/issues.db" "$RUN_DIR/issues.db"

  set +e
  NEW_OUT=$(DOCKET_PATH="$RUN_DIR" $CURRENT $verb 2>/dev/null)
  NEW_RC=$?
  set -e

  # Non-empty guard: a diff of two empty strings passes vacuously.
  if [ -z "$OLD_OUT" ]; then
    record "$verb" "FAIL" "baseline produced no output (the comparison would be vacuous)"
    continue
  fi

  if [ "$OLD_RC" != "$NEW_RC" ]; then
    record "$verb" "FAIL" "exit code $OLD_RC -> $NEW_RC"
    continue
  fi

  # Human-mode output renders relative ages ("now", "1 second ago"), which
  # differ between two runs a second apart. That is elapsed time, not an
  # output-format change, so relative ages are normalized before comparison.
  # Absolute timestamps in JSON output are NOT normalized — those come from
  # the database and must match exactly.
  OLD_CMP=$(printf '%s' "$OLD_OUT" | sed -E 's/(now|[0-9]+ (second|minute|hour|day|week|month|year)s? ago)/<AGE>/g')
  NEW_CMP=$(printf '%s' "$NEW_OUT" | sed -E 's/(now|[0-9]+ (second|minute|hour|day|week|month|year)s? ago)/<AGE>/g')

  # Age substitution changes string width, so column padding shifts with it.
  # Collapse runs of spaces in HUMAN output only — JSON has no such padding
  # and must stay byte-exact.
  case " $verb " in
    *--json*) ;;
    *) OLD_CMP=$(printf '%s' "$OLD_CMP" | tr -s ' ')
       NEW_CMP=$(printf '%s' "$NEW_CMP" | tr -s ' ') ;;
  esac

  if [ "$OLD_CMP" != "$NEW_CMP" ]; then
    record "$verb" "FAIL" "output differs"
    diff <(printf '%s' "$OLD_CMP") <(printf '%s' "$NEW_CMP") | head -10 | sed 's/^/      /'
    continue
  fi

  record "$verb" "PASS"
done

# --- Dormancy: the new schema is present but empty -----------------------

if command -v sqlite3 >/dev/null 2>&1; then
  # Migrate a fresh copy explicitly. The last verb in the matrix may be
  # skipDB-annotated (`version`), in which case its copy was never opened and
  # never migrated — querying that one would report a false failure.
  rm -rf "$RUN_DIR"
  mkdir -p "$RUN_DIR"
  cp "$SEED/issues.db" "$RUN_DIR/issues.db"
  DOCKET_PATH="$RUN_DIR" $CURRENT issue list --all >/dev/null 2>&1

  SCHEMA=$(sqlite3 "$RUN_DIR/issues.db" \
    "SELECT value FROM meta WHERE key='schema_version';" 2>/dev/null || echo "ERR")
  if [ "$SCHEMA" != "ERR" ] && [ "$SCHEMA" -gt 5 ] 2>/dev/null; then
    record "dormancy:migrated" "PASS"
  else
    record "dormancy:migrated" "FAIL" "database did not migrate (schema_version=$SCHEMA)"
  fi

  CLAIMED=$(sqlite3 "$RUN_DIR/issues.db" \
    "SELECT COUNT(*) FROM issues WHERE owner IS NOT NULL OR attempt != 0;" 2>/dev/null || echo "ERR")
  if [ "$CLAIMED" = "0" ]; then
    record "dormancy:no-lease-state" "PASS"
  else
    record "dormancy:no-lease-state" "FAIL" "$CLAIMED rows carry lease state in a repo that never claimed"
  fi
fi

printf "\nSweep: %d/%d verbs byte-identical\n" "$PASS" "$((PASS + FAIL))"

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
