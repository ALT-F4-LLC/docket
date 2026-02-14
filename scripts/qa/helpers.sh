#!/usr/bin/env bash
# Shared helper functions for QA test suite

setup() {
  QA_DIR=$(mktemp -d)
  export DOCKET_PATH="$QA_DIR/db"
  mkdir -p "$DOCKET_PATH"
}

cleanup() {
  rm -rf "$QA_DIR" /tmp/docket-qa-alt /tmp/docket-qa-bin 2>/dev/null || true
}

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
