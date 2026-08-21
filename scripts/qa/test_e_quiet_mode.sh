#!/usr/bin/env bash
# Section E: Quiet Mode

test_e_quiet_mode() {
  printf "Section E: Quiet Mode"
  local QUIET_DIR
  QUIET_DIR=$(qa_mktemp_d)
  mkdir -p "$QUIET_DIR"

  run_env "$QUIET_DIR" init --quiet
  assert_exit "E" "E1" 0
  check_cond "E" "E1_stderr" "stderr not suppressed: $CMD_STDERR" [ -z "$CMD_STDERR" ]

  run_env "$QUIET_DIR" config --quiet
  assert_exit "E" "E2" 0

  rm -rf "$QUIET_DIR"
}
