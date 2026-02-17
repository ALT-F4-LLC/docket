#!/usr/bin/env bash
# Section R: Exit Codes

test_r_exit_codes() {
  printf "Section R: Exit Codes"

  run version
  assert_exit "R" "R1" 0

  run config
  assert_exit "R" "R2" 0

  run init
  assert_exit "R" "R3" 0

  run issue create --json -t "Exit Code Test"
  assert_exit "R" "R4" 0
  local R_ID
  R_ID=$(extract_id)

  run issue create --json
  assert_exit "R" "R5" 3

  run issue list --json
  assert_exit "R" "R6" 0

  run issue show "$R_ID" --json
  assert_exit "R" "R7" 0

  run issue show 9999 --json
  assert_exit "R" "R8" 2

  run issue move "$R_ID" todo --json
  assert_exit "R" "R9" 0

  run issue move 9999 todo --json
  assert_exit "R" "R10" 2

  run issue move "$R_ID" invalid --json
  assert_exit "R" "R11" 3

  run issue close "$R_ID" --json
  assert_exit "R" "R12" 0

  run issue close 9999 --json
  assert_exit "R" "R13" 2

  run issue reopen "$R_ID" --json
  assert_exit "R" "R14" 0

  run issue reopen 9999 --json
  assert_exit "R" "R15" 2

  run issue edit "$R_ID" --json -t "X"
  assert_exit "R" "R16" 0

  run issue edit 9999 --json -t "X"
  assert_exit "R" "R17" 2

  run issue edit "$R_ID" --json -s invalid
  assert_exit "R" "R18" 3

  # R19: cycle detection
  local R19_P R19_C
  run issue create --json -t "R19 Parent"
  R19_P=$(extract_id)
  run issue create --json -t "R19 Child" --parent "$R19_P"
  R19_C=$(extract_id)
  run issue edit "$R19_P" --json --parent "$R19_C"
  assert_exit "R" "R19" 4

  # R20: delete no children
  local R20_ID
  run issue create --json -t "R20 Delete"
  R20_ID=$(extract_id)
  run issue delete "$R20_ID" --json
  assert_exit "R" "R20" 0

  run issue delete 9999 --json
  assert_exit "R" "R21" 2

  # R22-R23: delete with children
  local R22_P
  run issue create --json -t "R22 Parent"
  R22_P=$(extract_id)
  run issue create --json -t "R22 Child" --parent "$R22_P"

  run issue delete "$R22_P" --json
  assert_exit "R" "R22" 3

  run issue delete "$R22_P" --json --force --orphan
  assert_exit "R" "R23" 3

  # clean up R22
  run issue delete "$R22_P" --json --force
}
