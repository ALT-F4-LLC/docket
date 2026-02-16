#!/usr/bin/env bash
# Section L: Edit Command

test_l_edit() {
  printf "Section L: Edit Command"

  run issue edit 1 --json -t "Updated Title"
  assert_exit "L" "L1" 0
  assert_json "L" "L1" ".data.title" "Updated Title"

  run issue edit 1 --json -s in-progress
  assert_exit "L" "L2" 0
  assert_json "L" "L2" ".data.status" "in-progress"

  run issue edit 1 --json -p high
  assert_exit "L" "L3" 0
  assert_json "L" "L3" ".data.priority" "high"

  run issue edit 1 --json -T bug
  assert_exit "L" "L4" 0
  assert_json "L" "L4" ".data.kind" "bug"

  run issue edit 1 --json -a "bob"
  assert_exit "L" "L5" 0
  assert_json "L" "L5" ".data.assignee" "bob"

  run issue edit 1 --json -t "Multi Edit" -p critical -s todo
  assert_exit "L" "L6" 0
  assert_json "L" "L6" ".data.title" "Multi Edit"
  assert_json "L" "L6" ".data.priority" "critical"
  assert_json "L" "L6" ".data.status" "todo"

  run issue show 1 --json
  assert_exit "L" "L7" 0
  assert_json_array_min "L" "L7" ".data.activity" 5

  run issue edit 1 --json
  assert_exit "L" "L8" 0

  run issue edit 9999 --json -t "X"
  assert_exit "L" "L9" 2

  run issue edit 1 --json -s invalid
  assert_exit "L" "L10" 3

  run issue edit 1 --json -p invalid
  assert_exit "L" "L11" 3

  run issue edit 1 --json -T invalid
  assert_exit "L" "L12" 3

  run_stdin "new desc" issue edit 1 --json -d -
  assert_exit "L" "L13" 0
  assert_stdout_contains "L" "L13" "new desc"

  run issue edit 1 --json -d "direct desc"
  assert_exit "L" "L14" 0
  assert_json "L" "L14" ".data.description" "direct desc"

  run issue edit
  assert_exit_nonzero "L" "L15"
}
