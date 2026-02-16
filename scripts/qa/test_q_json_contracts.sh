#!/usr/bin/env bash
# Section Q: JSON Contracts

test_q_json_contracts() {
  printf "Section Q: JSON Contracts"

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

  run issue create --json -t "Contract Test"
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

  run issue list --json
  assert_exit "Q" "Q5" 0
  assert_json_exists "Q" "Q5" ".data.issues"
  assert_json_exists "Q" "Q5" ".data.total"

  run issue show "$Q_ISSUE_ID" --json
  assert_exit "Q" "Q6" 0
  assert_json_exists "Q" "Q6" ".data.sub_issues"
  assert_json_exists "Q" "Q6" ".data.relations"
  assert_json_exists "Q" "Q6" ".data.comments"
  assert_json_exists "Q" "Q6" ".data.activity"

  run issue move "$Q_ISSUE_ID" backlog --json
  assert_exit "Q" "Q7" 0
  assert_json "Q" "Q7" ".data.status" "backlog"
  assert_json_exists "Q" "Q7" ".data.id"

  run issue edit "$Q_ISSUE_ID" --json -t "Contract Edit"
  assert_exit "Q" "Q8" 0
  assert_json "Q" "Q8" ".data.title" "Contract Edit"
  assert_json_exists "Q" "Q8" ".data.id"

  run issue close "$Q_ISSUE_ID" --json
  assert_exit "Q" "Q9" 0
  assert_json "Q" "Q9" ".data.status" "done"

  run issue reopen "$Q_ISSUE_ID" --json
  assert_exit "Q" "Q10" 0
  assert_json "Q" "Q10" ".data.status" "backlog"

  # Q11: delete contract
  local Q11_PARENT
  run issue create --json -t "Q11 Parent"
  Q11_PARENT=$(extract_id)
  run issue create --json -t "Q11 Child" --parent "$Q11_PARENT"
  run issue delete "$Q11_PARENT" --json --force
  assert_exit "Q" "Q11" 0
  assert_json "Q" "Q11" ".ok" "true"
  assert_json_exists "Q" "Q11" ".data.id"

  # Q12: comment contract
  run issue comment add "$Q_ISSUE_ID" --json -m "Q12 contract"
  assert_exit "Q" "Q12" 0
  assert_json_exists "Q" "Q12" ".data.id"
  assert_json_exists "Q" "Q12" ".data.body"
  assert_json_exists "Q" "Q12" ".data.issue_id"
  assert_json_exists "Q" "Q12" ".data.created_at"

  # Q13: comments contract
  run issue comment list "$Q_ISSUE_ID" --json
  assert_exit "Q" "Q13" 0
  assert_json "Q" "Q13" ".ok" "true"
  assert_json_array_min "Q" "Q13" ".data" 1

  # Q14: log contract
  run issue log "$Q_ISSUE_ID" --json
  assert_exit "Q" "Q14" 0
  assert_json_exists "Q" "Q14" ".data.issue_id"
  assert_json_exists "Q" "Q14" ".data.entries"
  assert_json_exists "Q" "Q14" ".data.total"

  # Q15: stats contract
  run stats --json
  assert_exit "Q" "Q15" 0
  assert_json_exists "Q" "Q15" ".data.total"
  assert_json_exists "Q" "Q15" ".data.root_issues"
  assert_json_exists "Q" "Q15" ".data.sub_issues"
  assert_json_exists "Q" "Q15" ".data.by_status"
  assert_json_exists "Q" "Q15" ".data.by_priority"
  assert_json_exists "Q" "Q15" ".data.labels"

  # Q16: board contract
  run board --json
  assert_exit "Q" "Q16" 0
  assert_json_exists "Q" "Q16" ".data.columns"
  assert_json_all "Q" "Q16_col" ".data.columns" '.status != null and .count != null and .issues != null'

  # Q17: next contract
  run next --json
  assert_exit "Q" "Q17" 0
  assert_json_exists "Q" "Q17" ".data.issues"
  assert_json_exists "Q" "Q17" ".data.total"

  # Q18: plan contract
  run plan --json
  assert_exit "Q" "Q18" 0
  assert_json_exists "Q" "Q18" ".data.phases"
  assert_json_exists "Q" "Q18" ".data.total_issues"
  assert_json_exists "Q" "Q18" ".data.total_phases"
  assert_json_exists "Q" "Q18" ".data.max_parallelism"
}
