#!/usr/bin/env bash
#
# Docket CLI QA Test Suite
#
# Usage:
#   ./scripts/qa.sh [path/to/docket-binary] [section-letter]
#
# If no binary path is given, builds from source with `go build`.
# Runs all functional checks and prints a summary report.
# Optional section letter (A-V) runs only that section.
# Note: Sections B-U share a single DB and run sequentially. Later sections
# depend on state created by earlier ones (e.g., G uses issues from F).
# Only section A is fully self-contained. When running a single section,
# all prerequisite sections (B through the target) are executed automatically.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- Configuration -----------------------------------------------------------

DOCKET="${1:-}"
SECTION="${2:-}"
QA_DIR=""
PASS_COUNT=0
FAIL_COUNT=0
RESULTS=()

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

# --- Run sections ------------------------------------------------------------

# Ordered list of sections and their test functions.
# Sections B-V share a DB and depend on earlier sections' state.
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
)

REACHED_TARGET=false

for entry in "${SECTIONS[@]}"; do
  letter="${entry%%:*}"
  func="${entry##*:}"

  # Section A is self-contained and runs before DB setup.
  if [ "$letter" = "A" ]; then
    if [ -z "$SECTION" ] || [ "$SECTION" = "A" ]; then
      "$func"
    fi
    if [ "$SECTION" = "A" ]; then
      break
    fi
    setup
    continue
  fi

  if [ -z "$SECTION" ]; then
    # No filter — run everything.
    "$func"
  else
    # Run all sections up to and including the target so prerequisites are met.
    "$func"
    if [ "$letter" = "$SECTION" ]; then
      REACHED_TARGET=true
      break
    fi
  fi
done

if [ -n "$SECTION" ] && [ "$SECTION" != "A" ] && [ "$REACHED_TARGET" = false ]; then
  printf "FATAL: unknown section '%s'\n" "$SECTION"
  exit 1
fi

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
