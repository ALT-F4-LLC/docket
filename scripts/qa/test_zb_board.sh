#!/usr/bin/env bash
# Section ZB: Board Command

test_zb_board() {
  printf "Section ZB: Board Command"

  # ZB1: Board JSON mode — basic contract.
  run board --json
  assert_exit "ZB" "ZB1" 0
  assert_json "ZB" "ZB1_ok" ".ok" "true"

  # ZB2: JSON has columns array.
  assert_json_exists "ZB" "ZB2_columns" ".data.columns"

  # ZB3: Exactly 5 columns (one per status).
  local COL_COUNT
  COL_COUNT=$(echo "$CMD_STDOUT" | jq '.data.columns | length' 2>/dev/null)
  if [ "$COL_COUNT" -eq 5 ]; then
    check "ZB" "ZB3_count" "PASS"
  else
    check "ZB" "ZB3_count" "FAIL" "expected 5 columns, got $COL_COUNT"
  fi

  # ZB4: Each column has status, count, and issues fields.
  assert_json_all "ZB" "ZB4_status" ".data.columns" '.status != null'
  assert_json_all "ZB" "ZB4_count" ".data.columns" '.count != null'
  assert_json_all "ZB" "ZB4_issues" ".data.columns" '.issues != null'

  # ZB5: Column statuses match the expected Kanban order.
  assert_json "ZB" "ZB5_col0" ".data.columns[0].status" "backlog"
  assert_json "ZB" "ZB5_col1" ".data.columns[1].status" "todo"
  assert_json "ZB" "ZB5_col2" ".data.columns[2].status" "in-progress"
  assert_json "ZB" "ZB5_col3" ".data.columns[3].status" "review"
  assert_json "ZB" "ZB5_col4" ".data.columns[4].status" "done"

  # ZB6: At least one column has issues (from prior test sections).
  local TOTAL_ISSUES
  TOTAL_ISSUES=$(echo "$CMD_STDOUT" | jq '[.data.columns[].count] | add' 2>/dev/null)
  if [ "$TOTAL_ISSUES" -gt 0 ]; then
    check "ZB" "ZB6_nonempty" "PASS"
  else
    check "ZB" "ZB6_nonempty" "FAIL" "expected at least one issue on the board, got 0"
  fi

  # ZB7: Each column's count matches its issues array length.
  local MISMATCHED
  MISMATCHED=$(echo "$CMD_STDOUT" | jq '[.data.columns[] | select(.count != (.issues | length))] | length' 2>/dev/null)
  if [ "$MISMATCHED" -eq 0 ]; then
    check "ZB" "ZB7_consistency" "PASS"
  else
    check "ZB" "ZB7_consistency" "FAIL" "$MISMATCHED columns have count/issues mismatch"
  fi

  # ZB8: Default mode excludes sub-issues (only root issues shown).
  # Save the default (rolled-up) total for comparison with --expand.
  local DEFAULT_TOTAL="$TOTAL_ISSUES"

  # ZB9: --expand flag — JSON mode shows sub-issues individually.
  run board --json --expand
  assert_exit "ZB" "ZB9" 0
  assert_json "ZB" "ZB9_ok" ".ok" "true"
  local EXPAND_TOTAL
  EXPAND_TOTAL=$(echo "$CMD_STDOUT" | jq '[.data.columns[].count] | add' 2>/dev/null)
  if [ "$EXPAND_TOTAL" -ge "$DEFAULT_TOTAL" ]; then
    check "ZB" "ZB9_expand" "PASS"
  else
    check "ZB" "ZB9_expand" "FAIL" "expanded total ($EXPAND_TOTAL) < default total ($DEFAULT_TOTAL)"
  fi

  # ZB10: Human mode runs without error.
  run board
  assert_exit "ZB" "ZB10" 0

  # ZB11: --label filter returns subset of issues.
  run board --json -l bug
  assert_exit "ZB" "ZB11" 0
  assert_json "ZB" "ZB11_ok" ".ok" "true"

  # ZB12: --priority filter does not error.
  run board --json --priority high
  assert_exit "ZB" "ZB12" 0
  assert_json "ZB" "ZB12_ok" ".ok" "true"

  # ZB13: --assignee filter does not error.
  run board --json --assignee nobody
  assert_exit "ZB" "ZB13" 0
  assert_json "ZB" "ZB13_ok" ".ok" "true"

  # ZB14: Invalid priority filter returns validation error.
  run board --json --priority invalid-priority
  assert_exit "ZB" "ZB14" 3
  assert_json "ZB" "ZB14_ok" ".ok" "false"
}
