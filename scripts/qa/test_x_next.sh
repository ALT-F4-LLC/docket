#!/usr/bin/env bash
# Section X: Next Command

test_x_next() {
  printf "Section X: Next Command"

  # Setup: create issues with various states and relations for next-readiness testing.
  # We need: independent issues, blocked issues, parent/child issues, done issues.

  run create --json -t "Next Independent" -p high
  assert_exit "X" "X0_ind" 0
  local NEXT_IND
  NEXT_IND=$(extract_id)

  run create --json -t "Next Blocker" -p critical
  assert_exit "X" "X0_blocker" 0
  local NEXT_BLOCKER
  NEXT_BLOCKER=$(extract_id)

  run create --json -t "Next Blocked" -p medium
  assert_exit "X" "X0_blocked" 0
  local NEXT_BLOCKED
  NEXT_BLOCKED=$(extract_id)

  # Create a blocks relation: BLOCKER blocks BLOCKED.
  run link "$NEXT_BLOCKER" blocks "$NEXT_BLOCKED" --json
  assert_exit "X" "X0_link" 0

  # Create a parent with a child (parent should not appear in next since it's not a leaf).
  run create --json -t "Next Parent" -p high
  assert_exit "X" "X0_parent" 0
  local NEXT_PARENT
  NEXT_PARENT=$(extract_id)

  run create --json -t "Next Child" -p medium --parent "$NEXT_PARENT"
  assert_exit "X" "X0_child" 0
  local NEXT_CHILD
  NEXT_CHILD=$(extract_id)

  # Create a done issue (should never appear in next).
  run create --json -t "Next Done Issue"
  assert_exit "X" "X0_done" 0
  local NEXT_DONE
  NEXT_DONE=$(extract_id)
  run close "$NEXT_DONE" --json
  assert_exit "X" "X0_close" 0

  # X1: Basic next (JSON) — returns ready issues.
  run next --json
  assert_exit "X" "X1" 0
  assert_json "X" "X1_ok" ".ok" "true"
  assert_json_exists "X" "X1_total" ".data.total"

  # X2: Done issues are excluded.
  local HAS_DONE
  HAS_DONE=$(echo "$CMD_STDOUT" | jq "[.data.issues[] | select(.id == \"DKT-$NEXT_DONE\")] | length" 2>/dev/null)
  if [ "$HAS_DONE" -eq 0 ]; then
    check "X" "X2_no_done" "PASS"
  else
    check "X" "X2_no_done" "FAIL" "done issue DKT-$NEXT_DONE should not appear in next"
  fi

  # X3: Blocked issues are excluded (NEXT_BLOCKED is blocked by NEXT_BLOCKER which is not done).
  local HAS_BLOCKED
  HAS_BLOCKED=$(echo "$CMD_STDOUT" | jq "[.data.issues[] | select(.id == \"DKT-$NEXT_BLOCKED\")] | length" 2>/dev/null)
  if [ "$HAS_BLOCKED" -eq 0 ]; then
    check "X" "X3_no_blocked" "PASS"
  else
    check "X" "X3_no_blocked" "FAIL" "blocked issue DKT-$NEXT_BLOCKED should not appear in next"
  fi

  # X4: Parent issues with children are excluded (not leaf tasks).
  local HAS_PARENT
  HAS_PARENT=$(echo "$CMD_STDOUT" | jq "[.data.issues[] | select(.id == \"DKT-$NEXT_PARENT\")] | length" 2>/dev/null)
  if [ "$HAS_PARENT" -eq 0 ]; then
    check "X" "X4_no_parent" "PASS"
  else
    check "X" "X4_no_parent" "FAIL" "parent issue DKT-$NEXT_PARENT should not appear in next (not a leaf)"
  fi

  # X5: Independent leaf issues DO appear (NEXT_IND, NEXT_BLOCKER, NEXT_CHILD).
  local HAS_IND
  HAS_IND=$(echo "$CMD_STDOUT" | jq "[.data.issues[] | select(.id == \"DKT-$NEXT_IND\")] | length" 2>/dev/null)
  if [ "$HAS_IND" -ge 1 ]; then
    check "X" "X5_has_ind" "PASS"
  else
    check "X" "X5_has_ind" "FAIL" "independent issue DKT-$NEXT_IND should appear in next"
  fi

  # X6: Limit flag works.
  run next --json --limit 1
  assert_exit "X" "X6" 0
  assert_json_array_max "X" "X6_limit" ".data.issues" 1

  # X7: Status filter works (filter to only todo issues; our issues are backlog).
  run next --json -s todo
  assert_exit "X" "X7" 0
  # All returned issues should have status "todo".
  local TODO_ONLY
  TODO_ONLY=$(echo "$CMD_STDOUT" | jq '[.data.issues[] | select(.status != "todo")] | length' 2>/dev/null || echo "0")
  if [ "$TODO_ONLY" -eq 0 ]; then
    check "X" "X7_status" "PASS"
  else
    check "X" "X7_status" "FAIL" "expected only todo issues when filtering by status"
  fi

  # X8: Priority filter works.
  run next --json -p critical
  assert_exit "X" "X8" 0
  local CRIT_ONLY
  CRIT_ONLY=$(echo "$CMD_STDOUT" | jq '[.data.issues[] | select(.priority != "critical")] | length' 2>/dev/null || echo "0")
  if [ "$CRIT_ONLY" -eq 0 ]; then
    check "X" "X8_priority" "PASS"
  else
    check "X" "X8_priority" "FAIL" "expected only critical issues when filtering by priority"
  fi

  # X9: Invalid status filter (exit 3).
  run next --json -s invalid
  assert_exit "X" "X9" 3

  # X10: Invalid priority filter (exit 3).
  run next --json -p invalid
  assert_exit "X" "X10" 3

  # X11: Invalid type filter (exit 3).
  run next --json -T invalid
  assert_exit "X" "X11" 3

  # X12: Human mode outputs something (table rendering).
  run next
  assert_exit "X" "X12" 0

  # X13: JSON contract — issues array and total field.
  run next --json
  assert_exit "X" "X13" 0
  assert_json_exists "X" "X13_issues" ".data.issues"
  assert_json_exists "X" "X13_total" ".data.total"

  # X14: After closing the blocker, the previously blocked issue becomes ready.
  run close "$NEXT_BLOCKER" --json
  assert_exit "X" "X14_close" 0
  run next --json
  assert_exit "X" "X14" 0
  local NOW_READY
  NOW_READY=$(echo "$CMD_STDOUT" | jq "[.data.issues[] | select(.id == \"DKT-$NEXT_BLOCKED\")] | length" 2>/dev/null)
  if [ "$NOW_READY" -ge 1 ]; then
    check "X" "X14_unblocked" "PASS"
  else
    check "X" "X14_unblocked" "FAIL" "issue DKT-$NEXT_BLOCKED should be ready after blocker was closed"
  fi
}
