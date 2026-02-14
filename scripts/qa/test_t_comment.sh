#!/usr/bin/env bash
# Section T: Comment Command

test_t_comment() {
  printf "Section T: Comment Command\n"

  # T1: add comment with -m flag (JSON)
  run comment 1 --json -m "QA inline comment"
  assert_exit "T" "T1" 0
  assert_json "T" "T1" ".ok" "true"

  # T2: JSON contract — id is a number, issue_id is DKT-prefixed, body/author/created_at present
  assert_json "T" "T2_body" ".data.body" "QA inline comment"
  assert_json "T" "T2_issue" ".data.issue_id" "DKT-1"
  assert_json_exists "T" "T2_author" ".data.author"
  assert_json_exists "T" "T2_time" ".data.created_at"
  # comment id should be a plain integer (not DKT-prefixed)
  local COMMENT_ID_RAW
  COMMENT_ID_RAW=$(echo "$CMD_STDOUT" | jq -r '.data.id' 2>/dev/null)
  if echo "$COMMENT_ID_RAW" | grep -qE '^[0-9]+$'; then
    check "T" "T2_id_int" "PASS"
  else
    check "T" "T2_id_int" "FAIL" "comment id '$COMMENT_ID_RAW' is not a plain integer"
  fi

  # T3: add comment human mode
  run comment 1 -m "Human mode comment"
  assert_exit "T" "T3" 0
  assert_stdout_contains "T" "T3" "Comment added to DKT-1"

  # T4: JSON mode without -m → validation error (exit 3)
  run comment 1 --json
  assert_exit "T" "T4" 3

  # T5: comment via stdin pipe
  run_stdin "piped comment body" comment 1 --json
  assert_exit "T" "T5" 0
  assert_json "T" "T5_body" ".data.body" "piped comment body"

  # T6: comment on non-existent issue → not found (exit 2)
  run comment 9999 --json -m "ghost"
  assert_exit "T" "T6" 2

  # T7: comment with no args → error
  run comment
  assert_exit_nonzero "T" "T7"

  # T8: verify activity log records comment_added
  run show 1 --json
  assert_exit "T" "T8" 0
  local COMMENT_ACTIVITY
  COMMENT_ACTIVITY=$(echo "$CMD_STDOUT" | jq '[.data.activity[] | select(.field_changed == "comment_added")] | length' 2>/dev/null)
  if [ "$COMMENT_ACTIVITY" -ge 3 ]; then
    check "T" "T8_activity" "PASS"
  else
    check "T" "T8_activity" "FAIL" "expected >= 3 comment_added entries, got $COMMENT_ACTIVITY"
  fi

  # T9: DKT-prefix accepted as issue ID
  run comment DKT-1 --json -m "prefix test"
  assert_exit "T" "T9" 0
  assert_json "T" "T9" ".data.issue_id" "DKT-1"
}
