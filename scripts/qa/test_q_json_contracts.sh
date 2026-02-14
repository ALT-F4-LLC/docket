#!/usr/bin/env bash
# Section Q: JSON Contracts

test_q_json_contracts() {
  printf "Section Q: JSON Contracts\n"

  run version --json
  assert_exit "Q" "Q1" 0
  assert_json_exists "Q" "Q1" ".data.version"
  assert_json_exists "Q" "Q1" ".data.commit"
  assert_json_exists "Q" "Q1" ".data.build_date"

  run config --json
  assert_exit "Q" "Q2" 0
  assert_json_exists "Q" "Q2" ".data.db_path"
  assert_json_exists "Q" "Q2" ".data.db_size_bytes"
  assert_json_exists "Q" "Q2" ".data.schema_version"
  assert_json_exists "Q" "Q2" ".data.issue_prefix"

  run init --json
  assert_exit "Q" "Q3" 0
  assert_json_exists "Q" "Q3" ".data.path"
  assert_json_exists "Q" "Q3" ".data.db_path"
  assert_json_exists "Q" "Q3" ".data.schema_version"

  run create --json -t "Contract Test"
  assert_exit "Q" "Q4" 0
  assert_json_exists "Q" "Q4" ".data.id"
  assert_json_exists "Q" "Q4" ".data.title"
  assert_json_exists "Q" "Q4" ".data.status"
  assert_json_exists "Q" "Q4" ".data.priority"
  assert_json_exists "Q" "Q4" ".data.kind"
  assert_json_exists "Q" "Q4" ".data.created_at"
  assert_json_exists "Q" "Q4" ".data.updated_at"
  local Q_ISSUE_ID
  Q_ISSUE_ID=$(extract_id)

  run list --json
  assert_exit "Q" "Q5" 0
  assert_json_exists "Q" "Q5" ".data.issues"
  assert_json_exists "Q" "Q5" ".data.total"

  run show "$Q_ISSUE_ID" --json
  assert_exit "Q" "Q6" 0
  assert_json_exists "Q" "Q6" ".data.sub_issues"
  assert_json_exists "Q" "Q6" ".data.relations"
  assert_json_exists "Q" "Q6" ".data.comments"
  assert_json_exists "Q" "Q6" ".data.activity"

  run move "$Q_ISSUE_ID" backlog --json
  assert_exit "Q" "Q7" 0
  assert_json "Q" "Q7" ".data.status" "backlog"
  assert_json_exists "Q" "Q7" ".data.id"

  run edit "$Q_ISSUE_ID" --json -t "Contract Edit"
  assert_exit "Q" "Q8" 0
  assert_json "Q" "Q8" ".data.title" "Contract Edit"
  assert_json_exists "Q" "Q8" ".data.id"

  run close "$Q_ISSUE_ID" --json
  assert_exit "Q" "Q9" 0
  assert_json "Q" "Q9" ".data.status" "done"

  run reopen "$Q_ISSUE_ID" --json
  assert_exit "Q" "Q10" 0
  assert_json "Q" "Q10" ".data.status" "backlog"

  # Q11: delete contract
  local Q11_PARENT
  run create --json -t "Q11 Parent"
  Q11_PARENT=$(extract_id)
  run create --json -t "Q11 Child" --parent "$Q11_PARENT"
  run delete "$Q11_PARENT" --json --force
  assert_exit "Q" "Q11" 0
  assert_json "Q" "Q11" ".ok" "true"
  assert_json_exists "Q" "Q11" ".data.id"
}
