#!/usr/bin/env bash
# Section P: Activity Log

test_p_activity() {
  printf "Section P: Activity Log"

  run issue create --json -t "Activity Test"
  assert_exit "P" "P1" 0
  local ACT_ID
  ACT_ID=$(extract_id)

  run issue move "$ACT_ID" todo --json
  assert_exit "P" "P2" 0

  run issue edit "$ACT_ID" --json -t "Renamed" -p high
  assert_exit "P" "P3" 0

  run issue close "$ACT_ID" --json
  assert_exit "P" "P4" 0

  run issue reopen "$ACT_ID" --json
  assert_exit "P" "P5" 0

  run issue show "$ACT_ID" --json
  assert_exit "P" "P6" 0
  assert_json_array_min "P" "P6" ".data.activity" 5

  # P7: log command returns success
  run issue log "$ACT_ID" --json
  assert_exit "P" "P7" 0

  # P8: JSON entries array has >= 5 entries
  assert_json_array_min "P" "P8" ".data.entries" 5

  # P9: JSON issue_id matches the formatted ID
  local FORMATTED_ID
  FORMATTED_ID="DKT-${ACT_ID}"
  assert_json "P" "P9" ".data.issue_id" "$FORMATTED_ID"

  # P10: log with --limit 2 returns success
  run issue log "$ACT_ID" --limit 2 --json
  assert_exit "P" "P10" 0

  # P11: limit works (at most 2 entries)
  assert_json_array_max "P" "P11" ".data.entries" 2

  # P12: log for non-existent issue returns exit code 2
  run issue log 99999 --json
  assert_exit "P" "P12" 2

  # P13: human mode exits 0
  run issue log "$ACT_ID"
  assert_exit "P" "P13" 0

  # P14: human output contains "Activity for"
  assert_stdout_contains "P" "P14" "Activity for"

  # P15: log with no args returns error.
  run issue log
  assert_exit_nonzero "P" "P15"

  # P16: log with invalid ID format returns validation error.
  run issue log abc --json
  assert_exit "P" "P16" 3
}
