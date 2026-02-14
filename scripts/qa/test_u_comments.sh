#!/usr/bin/env bash
# Section U: Comments Command

test_u_comments() {
  printf "Section U: Comments Command\n"

  # U1: list comments (JSON) — should have comments from Section T
  run comments 1 --json
  assert_exit "U" "U1" 0
  assert_json "U" "U1" ".ok" "true"
  assert_json_array_min "U" "U1_count" ".data" 3

  # U2: each comment in the array has the right shape
  assert_json_all "U" "U2_body" ".data" '.body != null and .body != ""'
  assert_json_all "U" "U2_issue" ".data" '.issue_id == "DKT-1"'

  # U3: list comments human mode
  run comments 1
  assert_exit "U" "U3" 0
  assert_stdout_contains "U" "U3" "QA inline comment"

  # U4: comments on non-existent issue → not found (exit 2)
  run comments 9999 --json
  assert_exit "U" "U4" 2

  # U5: comments with no args → error
  run comments
  assert_exit_nonzero "U" "U5"

  # U6: DKT-prefix accepted
  run comments DKT-1 --json
  assert_exit "U" "U6" 0
  assert_json_array_min "U" "U6" ".data" 1

  # U7: comments on issue with no comments returns empty array
  run create --json -t "No Comments Issue"
  assert_exit "U" "U7_create" 0
  local NO_COMMENT_ID
  NO_COMMENT_ID=$(extract_id)
  run comments "$NO_COMMENT_ID" --json
  assert_exit "U" "U7" 0
  assert_json "U" "U7_empty" ".data | length" "0"

  # U8: show command includes real comments from DB
  run show 1 --json
  assert_exit "U" "U8" 0
  assert_json_array_min "U" "U8_comments" ".data.comments" 3
}
