#!/usr/bin/env bash
# Section I: Move Command

test_i_move() {
  printf "Section I: Move Command"

  run issue move 1 todo --json
  assert_exit "I" "I1" 0
  assert_json "I" "I1" ".data.status" "todo"

  run issue move DKT-1 todo --json
  assert_exit "I" "I2" 0

  run issue move 1 in-progress --json
  assert_exit "I" "I3" 0
  assert_json "I" "I3" ".data.status" "in-progress"

  run issue move 1 review --json
  assert_exit "I" "I4" 0
  assert_json "I" "I4" ".data.status" "review"

  run issue move 1 done --json
  assert_exit "I" "I5" 0
  assert_json "I" "I5" ".data.status" "done"

  run issue move 1 backlog --json
  assert_exit "I" "I6" 0
  assert_json "I" "I6" ".data.status" "backlog"

  run issue move 9999 todo --json
  assert_exit "I" "I7" 2

  run issue move 1 invalid --json
  assert_exit "I" "I8" 3

  run issue move 1
  assert_exit_nonzero "I" "I9"

  run issue move
  assert_exit_nonzero "I" "I10"

  run issue move 1 todo
  assert_exit "I" "I11" 0
  assert_stdout_contains "I" "I11" "Moved"
}
