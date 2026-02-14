#!/usr/bin/env bash
# Section F: Create Command

test_f_create() {
  printf "Section F: Create Command\n"

  run create --json -t "QA Test Issue"
  assert_exit "F" "F1" 0
  assert_json "F" "F1" ".ok" "true"
  assert_json "F" "F1" ".data.title" "QA Test Issue"
  assert_json "F" "F1" ".data.status" "backlog"
  assert_json "F" "F1" ".data.priority" "none"
  assert_json "F" "F1" ".data.kind" "task"

  run create --json -t "High Priority Bug" -p high -T bug -s todo
  assert_exit "F" "F2" 0
  assert_json "F" "F2" ".data.priority" "high"
  assert_json "F" "F2" ".data.kind" "bug"
  assert_json "F" "F2" ".data.status" "todo"

  run create --json -t "With Labels" -l "frontend" -l "urgent"
  assert_exit "F" "F3" 0
  assert_json "F" "F3" ".ok" "true"

  run create --json -t "With Assignee" -a "alice"
  assert_exit "F" "F4" 0
  assert_json "F" "F4" ".data.assignee" "alice"

  run create --json
  assert_exit "F" "F5" 3

  run create --json -t "Sub-issue" --parent DKT-1
  assert_exit "F" "F6" 0
  assert_json "F" "F6" ".data.parent_id" "DKT-1"

  run create --json -t "Bad Status" -s invalid
  assert_exit "F" "F7" 3

  run create --json -t "Bad Priority" -p invalid
  assert_exit "F" "F8" 3

  run create --json -t "Bad Type" -T invalid
  assert_exit "F" "F9" 3

  run create --json -t "Bad Parent" --parent 9999
  assert_exit "F" "F10" 2

  run_stdin "stdin desc" create --json -t "Stdin Test" -d -
  assert_exit "F" "F11" 0
  assert_stdout_contains "F" "F11" "stdin desc"
}
