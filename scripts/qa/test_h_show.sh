#!/usr/bin/env bash
# Section H: Show Command

test_h_show() {
  printf "Section H: Show Command"

  run issue show 1 --json
  assert_exit "H" "H1" 0
  assert_json "H" "H1" ".ok" "true"
  assert_json "H" "H1" ".data.id" "DKT-1"
  assert_json_exists "H" "H1" ".data.title"

  run issue show DKT-1 --json
  assert_exit "H" "H2" 0
  assert_json "H" "H2" ".data.id" "DKT-1"

  run issue show 1
  assert_exit "H" "H3" 0

  run issue show 9999 --json
  assert_exit "H" "H4" 2
  assert_json "H" "H4" ".code" "NOT_FOUND"

  run issue show DKT-1 --json
  assert_exit "H" "H5" 0
  assert_json_array_min "H" "H5" ".data.activity" 1

  run issue show --json
  assert_exit_nonzero "H" "H6"
}
