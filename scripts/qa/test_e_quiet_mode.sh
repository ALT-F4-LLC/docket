#!/usr/bin/env bash
# Section E: Quiet Mode

test_e_quiet_mode() {
  printf "Section E: Quiet Mode\n"
  local QUIET_DIR
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
}
