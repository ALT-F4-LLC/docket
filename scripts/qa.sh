#!/usr/bin/env bash
#
# Docket CLI QA Test Suite
#
# Usage:
#   ./scripts/qa.sh [path/to/docket-binary]
#
# If no binary path is given, builds from source with `go build`.
# Runs all functional checks and prints a summary report.

set -euo pipefail

# --- Configuration -----------------------------------------------------------

DOCKET="${1:-}"
QA_DIR=""
PASS_COUNT=0
FAIL_COUNT=0
RESULTS=()

# --- Helpers -----------------------------------------------------------------

setup() {
  QA_DIR=$(mktemp -d)
  export DOCKET_PATH="$QA_DIR/db"
  mkdir -p "$DOCKET_PATH"
}

cleanup() {
  rm -rf "$QA_DIR" /tmp/docket-qa-alt /tmp/docket-qa-bin 2>/dev/null || true
}
trap cleanup EXIT

# Run a command, capture stdout, stderr, and exit code.
# Sets: CMD_STDOUT, CMD_STDERR, CMD_EXIT
run() {
  CMD_STDOUT="" CMD_STDERR="" CMD_EXIT=0
  local tmpout tmperr
  tmpout=$(mktemp)
  tmperr=$(mktemp)
  set +e
  "$DOCKET" "$@" >"$tmpout" 2>"$tmperr"
  CMD_EXIT=$?
  set -e
  CMD_STDOUT=$(cat "$tmpout")
  CMD_STDERR=$(cat "$tmperr")
  rm -f "$tmpout" "$tmperr"
}

# Run with piped stdin.
run_stdin() {
  local input="$1"; shift
  CMD_STDOUT="" CMD_STDERR="" CMD_EXIT=0
  local tmpout tmperr
  tmpout=$(mktemp)
  tmperr=$(mktemp)
  set +e
  echo "$input" | "$DOCKET" "$@" >"$tmpout" 2>"$tmperr"
  CMD_EXIT=$?
  set -e
  CMD_STDOUT=$(cat "$tmpout")
  CMD_STDERR=$(cat "$tmperr")
  rm -f "$tmpout" "$tmperr"
}

# Run with a custom DOCKET_PATH.
run_env() {
  local dp="$1"; shift
  CMD_STDOUT="" CMD_STDERR="" CMD_EXIT=0
  local tmpout tmperr
  tmpout=$(mktemp)
  tmperr=$(mktemp)
  set +e
  DOCKET_PATH="$dp" "$DOCKET" "$@" >"$tmpout" 2>"$tmperr"
  CMD_EXIT=$?
  set -e
  CMD_STDOUT=$(cat "$tmpout")
  CMD_STDERR=$(cat "$tmperr")
  rm -f "$tmpout" "$tmperr"
}

# Record a check result. Usage: check SECTION ID PASS|FAIL [details]
check() {
  local section="$1" id="$2" result="$3" details="${4:-}"
  if [ "$result" = "PASS" ]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
  fi
  RESULTS+=("$section|$id|$result|$details")
  if [ "$result" = "FAIL" ]; then
    printf "  FAIL %s: %s\n" "$id" "$details"
  fi
}

# Assert exit code equals expected.
assert_exit() {
  local section="$1" id="$2" expected="$3"
  if [ "$CMD_EXIT" -eq "$expected" ]; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "expected exit $expected, got $CMD_EXIT. stderr: $(echo "$CMD_STDERR" | head -1)"
  fi
}

# Assert exit code is non-zero.
assert_exit_nonzero() {
  local section="$1" id="$2"
  if [ "$CMD_EXIT" -ne 0 ]; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "expected non-zero exit, got 0"
  fi
}

# Assert stdout contains a literal string.
assert_stdout_contains() {
  local section="$1" id="$2" needle="$3"
  if echo "$CMD_STDOUT" | grep -qF "$needle"; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "stdout missing '$needle'"
  fi
}

# Assert stderr contains a literal string.
assert_stderr_contains() {
  local section="$1" id="$2" needle="$3"
  if echo "$CMD_STDERR" | grep -qF "$needle"; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "stderr missing '$needle'"
  fi
}

# Assert JSON field equals value. Uses jq.
assert_json() {
  local section="$1" id="$2" path="$3" expected="$4"
  local actual
  actual=$(echo "$CMD_STDOUT" | jq -r "$path" 2>/dev/null || echo "__JQ_ERROR__")
  if [ "$actual" = "$expected" ]; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "JSON $path: expected '$expected', got '$actual'"
  fi
}

# Assert JSON field is non-empty / non-null.
assert_json_exists() {
  local section="$1" id="$2" path="$3"
  local actual
  actual=$(echo "$CMD_STDOUT" | jq -r "$path" 2>/dev/null || echo "null")
  if [ -n "$actual" ] && [ "$actual" != "null" ]; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "JSON $path is null or missing"
  fi
}

# Assert JSON field is null or absent.
assert_json_null() {
  local section="$1" id="$2" path="$3"
  local actual
  actual=$(echo "$CMD_STDOUT" | jq -r "$path" 2>/dev/null || echo "null")
  if [ "$actual" = "null" ] || [ -z "$actual" ]; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "JSON $path expected null, got '$actual'"
  fi
}

# Assert JSON array length is >= N.
assert_json_array_min() {
  local section="$1" id="$2" path="$3" min="$4"
  local len
  len=$(echo "$CMD_STDOUT" | jq "$path | length" 2>/dev/null || echo "0")
  if [ "$len" -ge "$min" ]; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "JSON $path length $len < $min"
  fi
}

# Assert JSON array length is <= N.
assert_json_array_max() {
  local section="$1" id="$2" path="$3" max="$4"
  local len
  len=$(echo "$CMD_STDOUT" | jq "$path | length" 2>/dev/null || echo "0")
  if [ "$len" -le "$max" ]; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "JSON $path length $len > $max"
  fi
}

# Assert all items in a JSON array match a jq filter.
assert_json_all() {
  local section="$1" id="$2" array_path="$3" filter="$4"
  local bad
  bad=$(echo "$CMD_STDOUT" | jq "[$array_path[] | select($filter | not)] | length" 2>/dev/null || echo "999")
  if [ "$bad" -eq 0 ]; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "$bad items in $array_path failed filter: $filter"
  fi
}

# Extract numeric ID from JSON data.id (strips "DKT-" prefix).
extract_id() {
  echo "$CMD_STDOUT" | jq -r '.data.id' 2>/dev/null | sed 's/^DKT-//'
}

# --- Build -------------------------------------------------------------------

printf "=== Docket QA Test Suite ===\n\n"

if [ -z "$DOCKET" ]; then
  printf "Building docket...\n"
  if ! go build -o /tmp/docket-qa-bin ./cmd/docket; then
    printf "FATAL: build failed\n"
    exit 1
  fi
  DOCKET="/tmp/docket-qa-bin"
  printf "Built: %s\n\n" "$DOCKET"
else
  printf "Using binary: %s\n\n" "$DOCKET"
fi

# Verify jq is available.
if ! command -v jq &>/dev/null; then
  printf "FATAL: jq is required but not found in PATH\n"
  exit 1
fi

# --- Section A: No-DB Commands -----------------------------------------------

printf "Section A: No-DB Commands\n"
# Use a path with no DB.
NO_DB_DIR=$(mktemp -d)
mkdir -p "$NO_DB_DIR"

run_env "$NO_DB_DIR" version
assert_exit "A" "A1" 0

run_env "$NO_DB_DIR" version --json
assert_exit "A" "A2" 0
assert_json "A" "A2" ".ok" "true"
assert_json_exists "A" "A2" ".data.version"

run_env "$NO_DB_DIR" --help
assert_exit "A" "A3" 0

run_env "$NO_DB_DIR" config
assert_exit "A" "A4" 0

run_env "$NO_DB_DIR" config --json
assert_exit "A" "A5" 0
assert_json "A" "A5" ".ok" "true"
assert_json "A" "A5" ".data.db_size_bytes" "0"
assert_json "A" "A5" ".data.schema_version" "0"

rm -rf "$NO_DB_DIR"

# --- Section B: Init Lifecycle -----------------------------------------------

printf "Section B: Init Lifecycle\n"
setup

run init
assert_exit "B" "B1" 0

if [ -f "$DOCKET_PATH/issues.db" ] && [ -s "$DOCKET_PATH/issues.db" ]; then
  check "B" "B2" "PASS"
else
  check "B" "B2" "FAIL" "DB file missing or empty"
fi

run init
assert_exit "B" "B3" 0

run init --json
assert_exit "B" "B4" 0
assert_json "B" "B4" ".ok" "true"
assert_json "B" "B4" ".data.created" "false"

# --- Section C: Config After Init --------------------------------------------

printf "Section C: Config After Init\n"

run config
assert_exit "C" "C1" 0

run config --json
assert_exit "C" "C2" 0
assert_json "C" "C2" ".data.schema_version" "1"
assert_json "C" "C2" ".data.issue_prefix" "DKT"

# --- Section D: DOCKET_PATH Override -----------------------------------------

printf "Section D: DOCKET_PATH Override\n"
mkdir -p /tmp/docket-qa-alt

run_env /tmp/docket-qa-alt init
assert_exit "D" "D1" 0

run_env /tmp/docket-qa-alt config --json
assert_exit "D" "D2" 0
assert_stdout_contains "D" "D2" "/tmp/docket-qa-alt"

rm -rf /tmp/docket-qa-alt

if [ ! -d /tmp/docket-qa-alt ]; then
  check "D" "D3" "PASS"
else
  check "D" "D3" "FAIL" "directory /tmp/docket-qa-alt still exists after cleanup"
fi

# --- Section E: Quiet Mode ---------------------------------------------------

printf "Section E: Quiet Mode\n"
QUIET_DIR=$(mktemp -d)
mkdir -p "$QUIET_DIR"

run_env "$QUIET_DIR" init --quiet
assert_exit "E" "E1" 0
if [ -z "$CMD_STDERR" ]; then
  check "E" "E1_stderr" "PASS"
else
  check "E" "E1_stderr" "FAIL" "stderr not suppressed: $CMD_STDERR"
fi

run_env "$QUIET_DIR" config --quiet
assert_exit "E" "E2" 0

rm -rf "$QUIET_DIR"

# --- Section F: Create Command -----------------------------------------------

printf "Section F: Create Command\n"

run create --json -t "QA Test Issue"
assert_exit "F" "F1" 0
assert_json "F" "F1" ".ok" "true"
assert_json "F" "F1" ".data.title" "QA Test Issue"
assert_json "F" "F1" ".data.status" "backlog"
assert_json "F" "F1" ".data.priority" "none"
assert_json "F" "F1" ".data.kind" "task"

run create --json -t "High Priority Bug" -p high -T bug -s todo
assert_exit "F" "F2" 0
assert_json "F" "F2" ".data.priority" "high"
assert_json "F" "F2" ".data.kind" "bug"
assert_json "F" "F2" ".data.status" "todo"

run create --json -t "With Labels" -l "frontend" -l "urgent"
assert_exit "F" "F3" 0
assert_json "F" "F3" ".ok" "true"

run create --json -t "With Assignee" -a "alice"
assert_exit "F" "F4" 0
assert_json "F" "F4" ".data.assignee" "alice"

run create --json
assert_exit "F" "F5" 3

run create --json -t "Sub-issue" --parent DKT-1
assert_exit "F" "F6" 0
assert_json "F" "F6" ".data.parent_id" "DKT-1"

run create --json -t "Bad Status" -s invalid
assert_exit "F" "F7" 3

run create --json -t "Bad Priority" -p invalid
assert_exit "F" "F8" 3

run create --json -t "Bad Type" -T invalid
assert_exit "F" "F9" 3

run create --json -t "Bad Parent" --parent 9999
assert_exit "F" "F10" 2

run_stdin "stdin desc" create --json -t "Stdin Test" -d -
assert_exit "F" "F11" 0
assert_stdout_contains "F" "F11" "stdin desc"

# --- Section G: List Command -------------------------------------------------

printf "Section G: List Command\n"

run list --json
assert_exit "G" "G1" 0
assert_json "G" "G1" ".ok" "true"
assert_json_array_min "G" "G1" ".data.issues" 1

run ls --json
assert_exit "G" "G2" 0
assert_json "G" "G2" ".ok" "true"

run list --json -s todo
assert_exit "G" "G3" 0
assert_json_all "G" "G3" ".data.issues" '.status == "todo"'

run list --json -p high
assert_exit "G" "G4" 0
assert_json_all "G" "G4" ".data.issues" '.priority == "high"'

run list --json -T bug
assert_exit "G" "G5" 0
assert_json_all "G" "G5" ".data.issues" '.kind == "bug"'

run list --json -a alice
assert_exit "G" "G6" 0
assert_json_all "G" "G6" ".data.issues" '.assignee == "alice"'

run list --json --roots
assert_exit "G" "G7" 0
assert_json_all "G" "G7" ".data.issues" '.parent_id == null'

run list --json --parent DKT-1
assert_exit "G" "G8" 0
assert_json_all "G" "G8" ".data.issues" '.parent_id == "DKT-1"'

run list --json --sort created_at:asc
assert_exit "G" "G9" 0

run list --json --limit 2
assert_exit "G" "G10" 0
assert_json_array_max "G" "G10" ".data.issues" 2

run list
assert_exit "G" "G11" 0

run list --tree
assert_exit "G" "G12" 0

# --- Section H: Show Command -------------------------------------------------

printf "Section H: Show Command\n"

run show 1 --json
assert_exit "H" "H1" 0
assert_json "H" "H1" ".ok" "true"
assert_json "H" "H1" ".data.id" "DKT-1"
assert_json_exists "H" "H1" ".data.title"

run show DKT-1 --json
assert_exit "H" "H2" 0
assert_json "H" "H2" ".data.id" "DKT-1"

run show 1
assert_exit "H" "H3" 0

run show 9999 --json
assert_exit "H" "H4" 2
assert_json "H" "H4" ".code" "NOT_FOUND"

run show DKT-1 --json
assert_exit "H" "H5" 0
assert_json_array_min "H" "H5" ".data.activity" 1

run show --json
assert_exit_nonzero "H" "H6"

# --- Section I: Move Command -------------------------------------------------

printf "Section I: Move Command\n"

run move 1 todo --json
assert_exit "I" "I1" 0
assert_json "I" "I1" ".data.status" "todo"

run move DKT-1 todo --json
assert_exit "I" "I2" 0

run move 1 in-progress --json
assert_exit "I" "I3" 0
assert_json "I" "I3" ".data.status" "in-progress"

run move 1 review --json
assert_exit "I" "I4" 0
assert_json "I" "I4" ".data.status" "review"

run move 1 done --json
assert_exit "I" "I5" 0
assert_json "I" "I5" ".data.status" "done"

run move 1 backlog --json
assert_exit "I" "I6" 0
assert_json "I" "I6" ".data.status" "backlog"

run move 9999 todo --json
assert_exit "I" "I7" 2

run move 1 invalid --json
assert_exit "I" "I8" 3

run move 1
assert_exit_nonzero "I" "I9"

run move
assert_exit_nonzero "I" "I10"

run move 1 todo
assert_exit "I" "I11" 0
assert_stdout_contains "I" "I11" "Moved"

# --- Section J: Close Command ------------------------------------------------

printf "Section J: Close Command\n"

run close 1 --json
assert_exit "J" "J1" 0
assert_json "J" "J1" ".data.status" "done"

run close 1 --json
assert_exit "J" "J2" 0

run close DKT-2 --json
assert_exit "J" "J3" 0

run close 9999 --json
assert_exit "J" "J4" 2

run close
assert_exit_nonzero "J" "J5"

run close 1
assert_exit "J" "J6" 0

# --- Section K: Reopen Command -----------------------------------------------

printf "Section K: Reopen Command\n"

run reopen 1 --json
assert_exit "K" "K1" 0
assert_json "K" "K1" ".data.status" "backlog"

run reopen 1 --json
assert_exit "K" "K2" 0

run reopen DKT-2 --json
assert_exit "K" "K3" 0
assert_json "K" "K3" ".data.status" "backlog"

run reopen 9999 --json
assert_exit "K" "K4" 2

run reopen
assert_exit_nonzero "K" "K5"

run reopen 1
assert_exit "K" "K6" 0

# --- Section L: Edit Command -------------------------------------------------

printf "Section L: Edit Command\n"

run edit 1 --json -t "Updated Title"
assert_exit "L" "L1" 0
assert_json "L" "L1" ".data.title" "Updated Title"

run edit 1 --json -s in-progress
assert_exit "L" "L2" 0
assert_json "L" "L2" ".data.status" "in-progress"

run edit 1 --json -p high
assert_exit "L" "L3" 0
assert_json "L" "L3" ".data.priority" "high"

run edit 1 --json -T bug
assert_exit "L" "L4" 0
assert_json "L" "L4" ".data.kind" "bug"

run edit 1 --json -a "bob"
assert_exit "L" "L5" 0
assert_json "L" "L5" ".data.assignee" "bob"

run edit 1 --json -t "Multi Edit" -p critical -s todo
assert_exit "L" "L6" 0
assert_json "L" "L6" ".data.title" "Multi Edit"
assert_json "L" "L6" ".data.priority" "critical"
assert_json "L" "L6" ".data.status" "todo"

run show 1 --json
assert_exit "L" "L7" 0
assert_json_array_min "L" "L7" ".data.activity" 5

run edit 1 --json
assert_exit "L" "L8" 0

run edit 9999 --json -t "X"
assert_exit "L" "L9" 2

run edit 1 --json -s invalid
assert_exit "L" "L10" 3

run edit 1 --json -p invalid
assert_exit "L" "L11" 3

run edit 1 --json -T invalid
assert_exit "L" "L12" 3

run_stdin "new desc" edit 1 --json -d -
assert_exit "L" "L13" 0
assert_stdout_contains "L" "L13" "new desc"

run edit 1 --json -d "direct desc"
assert_exit "L" "L14" 0
assert_json "L" "L14" ".data.description" "direct desc"

run edit
assert_exit_nonzero "L" "L15"

# --- Section M: Edit Reparenting ---------------------------------------------

printf "Section M: Edit Reparenting\n"

run create --json -t "Parent Issue"
assert_exit "M" "M1" 0
PARENT_ID=$(extract_id)

run create --json -t "Child Issue" --parent "$PARENT_ID"
assert_exit "M" "M2" 0
CHILD_ID=$(extract_id)

run create --json -t "Grandchild" --parent "$CHILD_ID"
assert_exit "M" "M3" 0
GRANDCHILD_ID=$(extract_id)

run edit "$CHILD_ID" --json --parent "$PARENT_ID"
assert_exit "M" "M4" 0

run edit "$CHILD_ID" --json --parent none
assert_exit "M" "M5" 0
assert_json_null "M" "M5" ".data.parent_id"

run edit "$CHILD_ID" --json --parent "$PARENT_ID"
assert_exit "M" "M6" 0

run edit "$PARENT_ID" --json --parent "$GRANDCHILD_ID"
assert_exit "M" "M7" 4

run edit "$PARENT_ID" --json --parent "$CHILD_ID"
assert_exit "M" "M8" 4

run edit "$CHILD_ID" --json --parent "$CHILD_ID"
assert_exit "M" "M9" 3

run edit "$CHILD_ID" --json --parent 9999
assert_exit "M" "M10" 2

run edit "$CHILD_ID" --json --parent 0
assert_exit "M" "M11" 0
assert_json_null "M" "M11" ".data.parent_id"

# --- Section N: Delete — Simple ----------------------------------------------

printf "Section N: Delete (Simple)\n"

run create --json -t "Delete Me"
assert_exit "N" "N1" 0
DEL_ID=$(extract_id)

run delete "$DEL_ID" --json
assert_exit "N" "N2" 0
assert_json "N" "N2" ".ok" "true"

run show "$DEL_ID" --json
assert_exit "N" "N3" 2

run delete 9999 --json
assert_exit "N" "N4" 2

run delete
assert_exit_nonzero "N" "N5"

# --- Section O: Delete — Cascade and Orphan ----------------------------------

printf "Section O: Delete (Cascade & Orphan)\n"

run create --json -t "Cascade Parent"
assert_exit "O" "O1" 0
CASCADE_PARENT=$(extract_id)

run create --json -t "Cascade Child 1" --parent "$CASCADE_PARENT"
assert_exit "O" "O2" 0
CASCADE_CHILD1=$(extract_id)

run create --json -t "Cascade Child 2" --parent "$CASCADE_PARENT"
assert_exit "O" "O3" 0
CASCADE_CHILD2=$(extract_id)

run delete "$CASCADE_PARENT" --json
assert_exit "O" "O4" 3

run delete "$CASCADE_PARENT" --json --force --orphan
assert_exit "O" "O5" 3

run delete "$CASCADE_PARENT" --json --force
assert_exit "O" "O6" 0
assert_json "O" "O6" ".ok" "true"

run show "$CASCADE_PARENT" --json
assert_exit "O" "O7" 2

run show "$CASCADE_CHILD1" --json
assert_exit "O" "O8" 2

run show "$CASCADE_CHILD2" --json
assert_exit "O" "O9" 2

run create --json -t "Orphan Parent"
assert_exit "O" "O10" 0
ORPHAN_PARENT=$(extract_id)

run create --json -t "Orphan Child 1" --parent "$ORPHAN_PARENT"
assert_exit "O" "O11" 0
ORPHAN_CHILD1=$(extract_id)

run create --json -t "Orphan Child 2" --parent "$ORPHAN_PARENT"
assert_exit "O" "O12" 0
ORPHAN_CHILD2=$(extract_id)

run delete "$ORPHAN_PARENT" --json --orphan
assert_exit "O" "O13" 0
assert_json "O" "O13" ".ok" "true"

run show "$ORPHAN_PARENT" --json
assert_exit "O" "O14" 2

run show "$ORPHAN_CHILD1" --json
assert_exit "O" "O15" 0
assert_json_null "O" "O15" ".data.parent_id"

run show "$ORPHAN_CHILD2" --json
assert_exit "O" "O16" 0
assert_json_null "O" "O16" ".data.parent_id"

# --- Section P: Activity Log Verification ------------------------------------

printf "Section P: Activity Log\n"

run create --json -t "Activity Test"
assert_exit "P" "P1" 0
ACT_ID=$(extract_id)

run move "$ACT_ID" todo --json
assert_exit "P" "P2" 0

run edit "$ACT_ID" --json -t "Renamed" -p high
assert_exit "P" "P3" 0

run close "$ACT_ID" --json
assert_exit "P" "P4" 0

run reopen "$ACT_ID" --json
assert_exit "P" "P5" 0

run show "$ACT_ID" --json
assert_exit "P" "P6" 0
assert_json_array_min "P" "P6" ".data.activity" 5

# --- Section Q: JSON Contract Validation -------------------------------------

printf "Section Q: JSON Contracts\n"

run version --json
assert_exit "Q" "Q1" 0
assert_json_exists "Q" "Q1" ".data.version"
assert_json_exists "Q" "Q1" ".data.commit"
assert_json_exists "Q" "Q1" ".data.build_date"

run config --json
assert_exit "Q" "Q2" 0
assert_json_exists "Q" "Q2" ".data.db_path"
assert_json_exists "Q" "Q2" ".data.db_size_bytes"
assert_json_exists "Q" "Q2" ".data.schema_version"
assert_json_exists "Q" "Q2" ".data.issue_prefix"

run init --json
assert_exit "Q" "Q3" 0
assert_json_exists "Q" "Q3" ".data.path"
assert_json_exists "Q" "Q3" ".data.db_path"
assert_json_exists "Q" "Q3" ".data.schema_version"

run create --json -t "Contract Test"
assert_exit "Q" "Q4" 0
assert_json_exists "Q" "Q4" ".data.id"
assert_json_exists "Q" "Q4" ".data.title"
assert_json_exists "Q" "Q4" ".data.status"
assert_json_exists "Q" "Q4" ".data.priority"
assert_json_exists "Q" "Q4" ".data.kind"
assert_json_exists "Q" "Q4" ".data.created_at"
assert_json_exists "Q" "Q4" ".data.updated_at"
Q_ISSUE_ID=$(extract_id)

run list --json
assert_exit "Q" "Q5" 0
assert_json_exists "Q" "Q5" ".data.issues"
assert_json_exists "Q" "Q5" ".data.total"

run show "$Q_ISSUE_ID" --json
assert_exit "Q" "Q6" 0
assert_json_exists "Q" "Q6" ".data.sub_issues"
assert_json_exists "Q" "Q6" ".data.relations"
assert_json_exists "Q" "Q6" ".data.comments"
assert_json_exists "Q" "Q6" ".data.activity"

run move "$Q_ISSUE_ID" backlog --json
assert_exit "Q" "Q7" 0
assert_json "Q" "Q7" ".data.status" "backlog"
assert_json_exists "Q" "Q7" ".data.id"

run edit "$Q_ISSUE_ID" --json -t "Contract Edit"
assert_exit "Q" "Q8" 0
assert_json "Q" "Q8" ".data.title" "Contract Edit"
assert_json_exists "Q" "Q8" ".data.id"

run close "$Q_ISSUE_ID" --json
assert_exit "Q" "Q9" 0
assert_json "Q" "Q9" ".data.status" "done"

run reopen "$Q_ISSUE_ID" --json
assert_exit "Q" "Q10" 0
assert_json "Q" "Q10" ".data.status" "backlog"

# Q11: delete contract
run create --json -t "Q11 Parent"
Q11_PARENT=$(extract_id)
run create --json -t "Q11 Child" --parent "$Q11_PARENT"
run delete "$Q11_PARENT" --json --force
assert_exit "Q" "Q11" 0
assert_json "Q" "Q11" ".ok" "true"
assert_json_exists "Q" "Q11" ".data.id"

# --- Section R: Exit Codes ---------------------------------------------------

printf "Section R: Exit Codes\n"

run version
assert_exit "R" "R1" 0

run config
assert_exit "R" "R2" 0

run init
assert_exit "R" "R3" 0

run create --json -t "Exit Code Test"
assert_exit "R" "R4" 0
R_ID=$(extract_id)

run create --json
assert_exit "R" "R5" 3

run list --json
assert_exit "R" "R6" 0

run show "$R_ID" --json
assert_exit "R" "R7" 0

run show 9999 --json
assert_exit "R" "R8" 2

run move "$R_ID" todo --json
assert_exit "R" "R9" 0

run move 9999 todo --json
assert_exit "R" "R10" 2

run move "$R_ID" invalid --json
assert_exit "R" "R11" 3

run close "$R_ID" --json
assert_exit "R" "R12" 0

run close 9999 --json
assert_exit "R" "R13" 2

run reopen "$R_ID" --json
assert_exit "R" "R14" 0

run reopen 9999 --json
assert_exit "R" "R15" 2

run edit "$R_ID" --json -t "X"
assert_exit "R" "R16" 0

run edit 9999 --json -t "X"
assert_exit "R" "R17" 2

run edit "$R_ID" --json -s invalid
assert_exit "R" "R18" 3

# R19: cycle detection
run create --json -t "R19 Parent"
R19_P=$(extract_id)
run create --json -t "R19 Child" --parent "$R19_P"
R19_C=$(extract_id)
run edit "$R19_P" --json --parent "$R19_C"
assert_exit "R" "R19" 4

# R20: delete no children
run create --json -t "R20 Delete"
R20_ID=$(extract_id)
run delete "$R20_ID" --json
assert_exit "R" "R20" 0

run delete 9999 --json
assert_exit "R" "R21" 2

# R22-R23: delete with children
run create --json -t "R22 Parent"
R22_P=$(extract_id)
run create --json -t "R22 Child" --parent "$R22_P"

run delete "$R22_P" --json
assert_exit "R" "R22" 3

run delete "$R22_P" --json --force --orphan
assert_exit "R" "R23" 3

# clean up R22
run delete "$R22_P" --json --force

# --- Section S: Error Paths (No DB) -----------------------------------------

printf "Section S: Error Paths (No DB)\n"
NO_DB_DIR2=$(mktemp -d)
mkdir -p "$NO_DB_DIR2"

run_env "$NO_DB_DIR2" config
assert_exit "S" "S1" 0

run_env "$NO_DB_DIR2" --help
assert_exit "S" "S2" 0

run_env "$NO_DB_DIR2" create --json -t "No DB"
assert_exit_nonzero "S" "S3"

run_env "$NO_DB_DIR2" list --json
assert_exit_nonzero "S" "S4"

run_env "$NO_DB_DIR2" show 1 --json
assert_exit_nonzero "S" "S5"

run_env "$NO_DB_DIR2" move 1 todo --json
assert_exit_nonzero "S" "S6"

run_env "$NO_DB_DIR2" close 1 --json
assert_exit_nonzero "S" "S7"

run_env "$NO_DB_DIR2" reopen 1 --json
assert_exit_nonzero "S" "S8"

run_env "$NO_DB_DIR2" edit 1 --json -t "X"
assert_exit_nonzero "S" "S9"

run_env "$NO_DB_DIR2" delete 1 --json
assert_exit_nonzero "S" "S10"

rm -rf "$NO_DB_DIR2"

# --- Report ------------------------------------------------------------------

printf "\n=== QA Report ===\n\n"
printf "%-8s | %-8s | %-6s | %s\n" "Section" "Check" "Result" "Details"
printf "%-8s-+-%-8s-+-%-6s-+-%s\n" "--------" "--------" "------" "-------"

for r in "${RESULTS[@]}"; do
  IFS='|' read -r sec id res det <<< "$r"
  printf "%-8s | %-8s | %-6s | %s\n" "$sec" "$id" "$res" "$det"
done

TOTAL=$((PASS_COUNT + FAIL_COUNT))
printf "\nQA Result: %d/%d checks passed\n" "$PASS_COUNT" "$TOTAL"

if [ "$FAIL_COUNT" -gt 0 ]; then
  printf "\nFailed checks:\n"
  for r in "${RESULTS[@]}"; do
    IFS='|' read -r sec id res det <<< "$r"
    if [ "$res" = "FAIL" ]; then
      printf "  %s %s: %s\n" "$sec" "$id" "$det"
    fi
  done
  exit 1
fi

exit 0
