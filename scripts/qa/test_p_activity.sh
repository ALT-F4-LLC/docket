#!/usr/bin/env bash
# Section P: Activity Log

test_p_activity() {
  printf "Section P: Activity Log\n"

  run create --json -t "Activity Test"
  assert_exit "P" "P1" 0
  local ACT_ID
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
}
