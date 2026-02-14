#!/usr/bin/env bash
# Section B: Init Lifecycle

test_b_init() {
  printf "Section B: Init Lifecycle\n"

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
}
