#!/usr/bin/env bash
# Shared helper functions for QA test suite

# CURRENT_SCHEMA_VERSION is read from the source of truth rather than
# hand-maintained here, so a stage that bumps the schema does not leave the
# fixture proof asserting a stale number (or, worse, pinned to a version the
# migration no longer stops at).
CURRENT_SCHEMA_VERSION=$(
  sed -n 's/^const currentSchemaVersion = \([0-9]*\).*/\1/p' \
    "$SCRIPT_DIR/../internal/db/schema.go"
)

# qa_mktemp / qa_mktemp_d — TMPDIR-honoring temp creation for the harness.
#
# Template form, deliberately, for the same reason 86c5633 fixed genericity.sh
# and a later fix landed in gate-baseref-regression.sh: bare `mktemp` on macOS ignores
# TMPDIR and uses the confstr per-user temp dir, which a sandboxed environment
# may deny. In the harness that denial is total — `setup()` runs before section
# A, so the whole suite aborts on its first line rather than failing one check.
qa_mktemp()   { mktemp   "${TMPDIR:-/tmp}/docket-qa.XXXXXX"; }
qa_mktemp_d() { mktemp -d "${TMPDIR:-/tmp}/docket-qa.XXXXXX"; }

setup() {
  QA_DIR=$(qa_mktemp_d)
  export DOCKET_PATH="$QA_DIR/db"
  mkdir -p "$DOCKET_PATH"

  # THE SANDBOX RULE (gates-trust §9.5 SB2). Every QA section inherits a
  # throwaway XDG_CONFIG_HOME, whether or not it touches the trust store, so a
  # section added later CANNOT FORGET. Without it, `docket trust add` in any
  # section would write entries into the operator's own
  # ~/.config/docket/trust.toml — a QA run that modifies the machine it is
  # auditing, which is the failure SB5 aborts on rather than skips.
  export XDG_CONFIG_HOME="$QA_DIR/xdg"
  mkdir -p "$XDG_CONFIG_HOME"
}

# assert_trust_sandbox is SB5: a HARD ABORT, not a skip, when the trust sandbox
# is not in force.
#
# It is an abort because the failure mode is not a missing test result — it is a
# QA run that writes into the operator's real trust store. A skip would report
# "fine" while doing exactly the damage the rule exists to prevent.
assert_trust_sandbox() {
  if [ -z "${XDG_CONFIG_HOME:-}" ]; then
    printf "FATAL: XDG_CONFIG_HOME is unset; refusing to run a trust section\n" >&2
    printf "       against the operator's real trust store (§9.5 SB5).\n" >&2
    exit 1
  fi
  case "$XDG_CONFIG_HOME" in
    "$QA_DIR"/*) ;;
    *)
      printf "FATAL: XDG_CONFIG_HOME (%s) is not under QA_DIR (%s);\n" \
        "$XDG_CONFIG_HOME" "$QA_DIR" >&2
      printf "       refusing to run a trust section against it (§9.5 SB5).\n" >&2
      exit 1
      ;;
  esac
}

cleanup() {
  # The two named paths follow TMPDIR for the same reason their creators do;
  # the bare /tmp spellings stay so a run that predates this change, or one
  # made with TMPDIR unset, still gets its artifacts removed.
  rm -rf "$QA_DIR" \
    "${TMPDIR:-/tmp}/docket-qa-alt" "${TMPDIR:-/tmp}/docket-qa-bin" \
    /tmp/docket-qa-alt /tmp/docket-qa-bin 2>/dev/null || true
}

# Run a command, capture stdout, stderr, and exit code.
# Sets: CMD_STDOUT, CMD_STDERR, CMD_EXIT
run() {
  CMD_STDOUT="" CMD_STDERR="" CMD_EXIT=0
  local tmpout tmperr
  tmpout=$(qa_mktemp)
  tmperr=$(qa_mktemp)
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
  tmpout=$(qa_mktemp)
  tmperr=$(qa_mktemp)
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
  tmpout=$(qa_mktemp)
  tmperr=$(qa_mktemp)
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

# check_cond collapses the 340-copy shape (DKT-51):
#
#   if COND; then
#     check "SEC" "ID" "PASS"
#   else
#     check "SEC" "ID" "FAIL" "msg"
#   fi
#
# into one call: check_cond SEC ID msg COND...
#
# COND is passed as TRAILING ARGS and run DIRECTLY as a command — never
# through `eval` and never inside a subshell. That is what makes this safe
# rather than merely shorter:
#
#   - No eval: the condition is `[ "$x" -eq 0 ]` (i.e. the `test`/`[` builtin
#     invoked with its own argv), never a string re-parsed by the shell, so
#     there is no second layer of quoting to get wrong and no injection risk
#     from a value that happens to contain shell metacharacters.
#   - No subshell: `if "$@"; then` runs the condition as a plain command
#     substitution-free invocation inside THIS function's own shell, so it
#     inherits `set -e`'s suspension exactly the way `if COND; then` always
#     has — a condition's failure here never aborts the script, matching the
#     inline form byte for byte. (This is the hazard the issue named: `set -e`
#     does not survive into a subshell the way it reads, and wrapping a
#     conditional in a function is exactly the kind of change that can
#     silently alter when a non-zero exit aborts. Calling `"$@"` directly,
#     with no `$(...)`, no `bash -c`, and no pipeline, keeps this function in
#     the same shell as its caller, so nothing changes.)
#
# It exists ONLY for conditions expressible as a single command — `[ ... ]`,
# `grep -q ...`, a function call. A COMPOUND condition (`[ a ] && [ b ]`) is
# not convertible this way without eval or `[[ ... ]]`'s special parse-time
# syntax (which cannot be invoked via "$@": `[[` is shell grammar, not a
# runtime command) — those sites are deliberately left as `if`/`fi`.
check_cond() {
  local section="$1" id="$2" msg="$3"; shift 3
  if "$@"; then
    check "$section" "$id" "PASS"
  else
    check "$section" "$id" "FAIL" "$msg"
  fi
}

# Run a standing gate script and record one PASS/FAIL for it.
#
# THE ONE DEFINITION of the block shape qa.sh's standing-gate section used to
# repeat five times. Those copies had already drifted in two dimensions —
# three different failure anchors (`'^FAIL'`, `'^  '`, `'FAIL'`) and two
# temp-file idioms — and that drift is what produced the wrong-reason bug this
# consolidation fixes.
#
# Usage: run_gate <SECTION> <CHECK_ID> <label> <script> [failure-anchor]
#
# The FAILURE ANCHOR is the one thing that legitimately varies, because the
# gates genuinely print their failures differently: genericity.sh emits
# `FAIL  <word> appears in...` at column 0, render-verify.sh emits
# `  MISSING  <file>...` indented, and the CI gate scripts emit
# `<gate> FAIL: <reason>`. So it is a PARAMETER with a conservative default,
# not a fifth hand-copied grep.
#
# Why the default excludes `  ok` AND is anchored on real failure markers:
# a bare `grep -m1 '^  '` picks the FIRST indented line, and render-verify.sh
# prints benign `  no test ...` observations BEFORE any `  MISSING`. Filtering
# only `  ok` was not enough — `  no test` survives it, so the operator was
# told the wrong file and the wrong reason for the failure. Measured against a
# forced failure: reported `no test internal/render/markdown.go has exported
# Render* but no markdown_test.go` (a standing backlog note) when the actual
# failure was `MISSING internal/render/markdown.go was changed by this step
# and has no test`.
#
# The fallback chain matters as much as the anchor: an anchored match first,
# then any non-ok indented line, then a fixed string. A gate that fails in a
# shape nobody anticipated still reports SOMETHING rather than an empty
# reason.
run_gate() {
  local section="$1" id="$2" label="$3" script="$4"
  local anchor="${5:-MISSING|FAIL|FAILED}"

  local start out
  start=$(_ms)
  printf "%s" "$label"

  out=$(qa_mktemp)

  if bash "$SCRIPT_DIR/qa/$script" >"$out" 2>&1; then
    check "$section" "$id" "PASS"
    # A gate that exits 0 having declined to run half of itself is not the
    # same as one that verified everything. copy-verify.sh says so in its own
    # output; surface it rather than letting a bare PASS imply full coverage.
    if grep -q "reported gap, not a pass" "$out"; then
      printf "  NOTE %s\n" "$(grep -m1 'not run' "$out")"
    fi
  else
    check "$section" "$id" "FAIL" "$(gate_failure_detail "$out" "$anchor" "$script")"
    if [ "$VERBOSE" = true ]; then
      sed 's/^/  /' "$out"
    fi
  fi

  rm -f "$out"
  _elapsed "$start"
}

# gate_failure_detail extracts the line that explains why a gate failed.
# Split out from run_gate so the extraction rule is testable on its own and
# stated once.
gate_failure_detail() {
  local out="$1" anchor="$2" script="$3"
  local detail

  # 1. The gate's own failure marker, which is the line that names the real
  #    reason.
  detail=$(grep -m1 -E "$anchor" "$out" 2>/dev/null || true)

  # 2. Failing that, any indented line that is not a passing sub-check.
  if [ -z "$detail" ]; then
    detail=$(grep -v '^  ok' "$out" 2>/dev/null | grep -m1 '^  ' || true)
  fi

  # 3. Failing that, say plainly that the gate failed without a parseable
  #    reason — never an empty detail, which reads as no failure at all.
  if [ -z "$detail" ]; then
    detail="$script failed"
  fi

  # Collapse leading whitespace so the report column stays aligned across
  # gates that indent their failures differently.
  printf '%s\n' "$detail" | sed 's/^[[:space:]]*//'
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
  check_cond "$section" "$id" "expected non-zero exit, got 0" [ "$CMD_EXIT" -ne 0 ]
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
  check_cond "$section" "$id" "JSON $path: expected '$expected', got '$actual'" [ "$actual" = "$expected" ]
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
  check_cond "$section" "$id" "JSON $path length $len < $min" [ "$len" -ge "$min" ]
}

# Assert JSON array length is <= N.
assert_json_array_max() {
  local section="$1" id="$2" path="$3" max="$4"
  local len
  len=$(echo "$CMD_STDOUT" | jq "$path | length" 2>/dev/null || echo "0")
  check_cond "$section" "$id" "JSON $path length $len > $max" [ "$len" -le "$max" ]
}

# Assert all items in a JSON array match a jq filter.
assert_json_all() {
  local section="$1" id="$2" array_path="$3" filter="$4"
  local bad
  bad=$(echo "$CMD_STDOUT" | jq "[$array_path[] | select($filter | not)] | length" 2>/dev/null || echo "999")
  check_cond "$section" "$id" "$bad items in $array_path failed filter: $filter" [ "$bad" -eq 0 ]
}

# Extract numeric ID from JSON data.id (strips "DKT-" prefix).
extract_id() {
  echo "$CMD_STDOUT" | jq -r '.data.id' 2>/dev/null | sed 's/^DKT-//'
}
