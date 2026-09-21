#!/usr/bin/env bash
#
# Docket CLI QA Test Suite
#
# Usage:
#   ./scripts/qa.sh [--verbose] [path/to/docket-binary] [section-letter]
#
# If no binary path is given, builds from source with `go build`.
# Runs all functional checks and prints a summary report.
# By default only failures are shown. Pass --verbose to see all results.
# Optional section letter (A-Z) runs only that section.
# Note: Sections B-U share a single DB and run sequentially. Later sections
# depend on state created by earlier ones (e.g., G uses issues from F).
# Only section A is fully self-contained. When running a single section,
# all prerequisite sections (B through the target) are executed automatically.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- Configuration -----------------------------------------------------------

VERBOSE=false
if [ "${1:-}" = "--verbose" ]; then
  VERBOSE=true
  shift
fi

DOCKET="${1:-}"
SECTION="${2:-}"
QA_DIR=""
PASS_COUNT=0
FAIL_COUNT=0
RESULTS=()

# Millisecond-precision timer (portable — uses perl Time::HiRes).
_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
_elapsed() { local e=$(( $(_ms) - $1 )); printf " (%d.%03ds)\n" "$((e / 1000))" "$((e % 1000))"; }
SUITE_START=$(_ms)

# --- Source helpers and test files -------------------------------------------

source "$SCRIPT_DIR/qa/helpers.sh"

for f in $(LC_ALL=C ls "$SCRIPT_DIR"/qa/test_*.sh); do
  source "$f"
done

trap cleanup EXIT

# --- Build -------------------------------------------------------------------

printf "=== Docket QA Test Suite ===\n\n"

if [ -z "$DOCKET" ]; then
  printf "Building docket...\n"
  # Under TMPDIR, not a hardcoded /tmp: a TMPDIR-confined environment denies
  # writes to /tmp, and this is the suite's FIRST write — the whole run aborts
  # here rather than failing one check. Same reason as qa_mktemp in helpers.sh.
  QA_BIN="${TMPDIR:-/tmp}/docket-qa-bin"
  if ! go build -o "$QA_BIN" ./cmd/docket; then
    printf "FATAL: build failed\n"
    exit 1
  fi
  DOCKET="$QA_BIN"
  printf "Built: %s\n\n" "$DOCKET"
else
  printf "Using binary: %s\n\n" "$DOCKET"
fi

# Verify jq is available.
if ! command -v jq &>/dev/null; then
  printf "FATAL: jq is required but not found in PATH\n"
  exit 1
fi

# Section ZE inspects the schema of the v4 fixture directly.
if ! command -v sqlite3 &>/dev/null; then
  printf "FATAL: sqlite3 is required but not found in PATH\n"
  exit 1
fi

# --- Run sections ------------------------------------------------------------

# Ordered list of sections and their test functions.
# Sections B-ZC share a DB and depend on earlier sections' state.
SECTIONS=(
  A:test_a_no_db
  B:test_b_init
  C:test_c_config
  D:test_d_path_override
  E:test_e_quiet_mode
  F:test_f_create
  G:test_g_list
  H:test_h_show
  I:test_i_move
  J:test_j_close
  K:test_k_reopen
  L:test_l_edit
  M:test_m_edit_reparent
  N:test_n_delete_simple
  O:test_o_delete_cascade
  P:test_p_activity
  Q:test_q_json_contracts
  R:test_r_exit_codes
  S:test_s_error_paths
  T:test_t_comment
  U:test_u_comments
  V:test_v_label
  W:test_w_link
  X:test_x_next
  Y:test_y_plan
  Z:test_z_graph
  ZA:test_za_stats
  ZB:test_zb_board
  ZC:test_zc_export_import
  ZD:test_zd_jsonv2
  ZE:test_ze_cas_migration
  ZF:test_zf_claims
  ZG:test_zg_workflow
  ZH:test_zh_stranger
  ZI:test_zi_budget
  ZJ:test_zj_dispatch
  ZK:test_zk_zerotouch
  ZL:test_zl_tail
  ZM:test_zm_metadata
  ZN:test_zn_packet
  ZO:test_zo_backfill
  ZP:test_zp_fail_metadata
  ZQ:test_zd_ui
)

REACHED_TARGET=false

for entry in "${SECTIONS[@]}"; do
  letter="${entry%%:*}"
  func="${entry##*:}"

  # Section A is self-contained and runs before DB setup.
  if [ "$letter" = "A" ]; then
    if [ -z "$SECTION" ] || [ "$SECTION" = "A" ]; then
      sec_start=$(_ms)
      "$func"
      _elapsed "$sec_start"
    fi
    if [ "$SECTION" = "A" ]; then
      break
    fi
    setup
    continue
  fi

  sec_start=$(_ms)
  if [ -z "$SECTION" ]; then
    # No filter — run everything.
    "$func"
  else
    # Run all sections up to and including the target so prerequisites are met.
    "$func"
  fi
  _elapsed "$sec_start"

  if [ -n "$SECTION" ] && [ "$letter" = "$SECTION" ]; then
    REACHED_TARGET=true
    break
  fi
done

if [ -n "$SECTION" ] && [ "$SECTION" != "A" ] && [ "$REACHED_TARGET" = false ]; then
  printf "FATAL: unknown section '%s'\n" "$SECTION"
  exit 1
fi

# --- Standing gates ----------------------------------------------------------
#
# The genericity rule (docs/design/genericity.md, CLAUDE.md's PR bar) runs on
# every push rather than depending on a reviewer's memory — the same discipline
# as the --token-flag guard test (docs/tdd/engine-spine.md §9). It scans core
# surface only, and is skipped when a single section was requested, since it is
# a whole-repo check rather than a section's.
#
# copy-verify, render-verify, and the CI-wiring gate-baseref-regression /
# gate-coverage-check run here for the same reason: each is a whole-repo,
# diff-independent check, not a section's.
#
# ONE BLOCK SHAPE, defined once as `run_gate` in qa/helpers.sh. This
# section used to hold five copies of the same twelve lines, which had already
# drifted in three dimensions — the failure anchor, the temp-file idiom, and
# whether the output file honored TMPDIR at all. That drift was not cosmetic:
# it is what made a real render-verify failure report a benign `no test` line
# as its reason. Anything that varies legitimately between gates is a
# PARAMETER below, so a sixth gate cannot reintroduce the same divergence.

if [ -z "$SECTION" ]; then
  # The failure anchor is per-gate because the gates genuinely print failures
  # differently; everything else about the block is shared.
  run_gate "GEN" "GEN_core_surface" "Genericity gate" \
    "genericity.sh" '^FAIL'

  # copy-verify and render-verify (design-qa's gates) are also whole-repo,
  # diff-independent checks — same shape as genericity, so they run here too
  # rather than depending on a ui-change run to exercise them: neither needs
  # run/step identity or a specific workflow step, and both already run
  # standalone.
  #
  # copy-verify's own diagnostics are the indented lines that follow the
  # offending grep hit; its trailing `copy-verify FAILED.` banner is a summary
  # that names nothing. The anchor targets the diagnostics so the report says
  # WHICH discipline broke, not merely that the gate failed.
  run_gate "CV" "CV_copy_discipline" "copy-verify gate" \
    "copy-verify.sh" 'blame the reader|placeholder|differs from|MISSING'

  # render-verify prints benign `  no test ...` observations BEFORE any
  # `  MISSING`, so its anchor must name the failure marker rather than "the
  # first indented line that is not ok" — the extraction that reported the
  # wrong file and the wrong reason.
  run_gate "RV" "RV_render_coverage" "render-verify gate" \
    "render-verify.sh" 'MISSING|raw ANSI escape'

  # gate-baseref-regression and gate-coverage-check pin the CI wiring
  # itself (findings C1/C2/C5 and C3): the former runs the base-ref mode of
  # secret-scan.sh/self-hygiene.sh through their real entry points, the
  # latter checks ci.yaml's GATE COVERAGE block against the actual directory.
  run_gate "BR" "BR_baseref_mode" "gate-baseref-regression" \
    "gate-baseref-regression.sh" 'FAIL'

  run_gate "GC" "GC_ci_coverage_block" "gate-coverage-check" \
    "gate-coverage-check.sh" 'FAIL'
fi

# --- Report ------------------------------------------------------------------

TOTAL=$((PASS_COUNT + FAIL_COUNT))
SUITE_ELAPSED=$(( $(_ms) - SUITE_START ))

if [ "$VERBOSE" = true ]; then
  printf "\n=== QA Report ===\n\n"
  printf "%-8s | %-8s | %-6s | %s\n" "Section" "Check" "Result" "Details"
  printf "%-8s-+-%-8s-+-%-6s-+-%s\n" "--------" "--------" "------" "-------"

  for r in "${RESULTS[@]}"; do
    IFS='|' read -r sec id res det <<< "$r"
    printf "%-8s | %-8s | %-6s | %s\n" "$sec" "$id" "$res" "$det"
  done
fi

if [ "$FAIL_COUNT" -gt 0 ]; then
  printf "\nFailed checks:\n"
  for r in "${RESULTS[@]}"; do
    IFS='|' read -r sec id res det <<< "$r"
    if [ "$res" = "FAIL" ]; then
      printf "  %s %s: %s\n" "$sec" "$id" "$det"
    fi
  done
fi

printf "\nQA Result: %d/%d checks passed in %d.%03ds\n" \
  "$PASS_COUNT" "$TOTAL" "$((SUITE_ELAPSED / 1000))" "$((SUITE_ELAPSED % 1000))"

if [ "$FAIL_COUNT" -gt 0 ]; then
  exit 1
fi

exit 0
