#!/usr/bin/env bash
# Section J: Close Command

test_j_close() {
  printf "Section J: Close Command"

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
}
