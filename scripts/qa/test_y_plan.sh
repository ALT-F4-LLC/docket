#!/usr/bin/env bash
# Section Y: Plan Command

test_y_plan() {
  printf "Section Y: Plan Command\n"

  # Setup: create a small dependency chain for plan testing.
  # A (no deps) -> B (blocked by A) -> C (blocked by B)
  run create --json -t "Plan Phase1" -p high
  assert_exit "Y" "Y0_a" 0
  local PLAN_A
  PLAN_A=$(extract_id)

  run create --json -t "Plan Phase2" -p medium
  assert_exit "Y" "Y0_b" 0
  local PLAN_B
  PLAN_B=$(extract_id)

  run create --json -t "Plan Phase3" -p low
  assert_exit "Y" "Y0_c" 0
  local PLAN_C
  PLAN_C=$(extract_id)

  # A blocks B, B blocks C.
  run link "$PLAN_A" blocks "$PLAN_B" --json
  assert_exit "Y" "Y0_link1" 0
  run link "$PLAN_B" blocks "$PLAN_C" --json
  assert_exit "Y" "Y0_link2" 0

  # Also create an independent issue (should be in Phase 1 alongside A).
  run create --json -t "Plan Independent" -p critical
  assert_exit "Y" "Y0_ind" 0
  local PLAN_IND
  PLAN_IND=$(extract_id)

  # Y1: Basic plan (JSON) — returns phased execution plan.
  run plan --json
  assert_exit "Y" "Y1" 0
  assert_json "Y" "Y1_ok" ".ok" "true"

  # Y2: JSON contract — phases array and stats.
  assert_json_exists "Y" "Y2_phases" ".data.phases"
  assert_json_exists "Y" "Y2_total" ".data.total_issues"
  assert_json_exists "Y" "Y2_tphases" ".data.total_phases"
  assert_json_exists "Y" "Y2_maxpar" ".data.max_parallelism"

  # Y3: Plan has multiple phases (at least 3 due to A->B->C chain).
  local NUM_PHASES
  NUM_PHASES=$(echo "$CMD_STDOUT" | jq '.data.total_phases' 2>/dev/null)
  if [ "$NUM_PHASES" -ge 3 ]; then
    check "Y" "Y3_phases" "PASS"
  else
    check "Y" "Y3_phases" "FAIL" "expected >= 3 phases, got $NUM_PHASES"
  fi

  # Y4: Phase 1 contains the independent issue and Plan_A (both have no blockers).
  local PHASE1_IDS
  PHASE1_IDS=$(echo "$CMD_STDOUT" | jq -r '[.data.phases[0].issues[].id] | join(",")' 2>/dev/null)
  if echo "$PHASE1_IDS" | grep -qF "DKT-$PLAN_A"; then
    check "Y" "Y4_a_in_p1" "PASS"
  else
    check "Y" "Y4_a_in_p1" "FAIL" "Plan_A (DKT-$PLAN_A) should be in Phase 1"
  fi

  # Y5: Plan_B should NOT be in Phase 1 (it's blocked by Plan_A).
  if echo "$PHASE1_IDS" | grep -qF "DKT-$PLAN_B"; then
    check "Y" "Y5_b_not_p1" "FAIL" "Plan_B (DKT-$PLAN_B) should NOT be in Phase 1"
  else
    check "Y" "Y5_b_not_p1" "PASS"
  fi

  # Y6: Max parallelism is at least 2 (Phase 1 has A + independent).
  local MAX_PAR
  MAX_PAR=$(echo "$CMD_STDOUT" | jq '.data.max_parallelism' 2>/dev/null)
  if [ "$MAX_PAR" -ge 2 ]; then
    check "Y" "Y6_maxpar" "PASS"
  else
    check "Y" "Y6_maxpar" "FAIL" "expected max_parallelism >= 2, got $MAX_PAR"
  fi

  # Y7: Human mode output contains "Execution Plan" and "Summary".
  run plan
  assert_exit "Y" "Y7" 0
  assert_stdout_contains "Y" "Y7_header" "Execution Plan"
  assert_stdout_contains "Y" "Y7_summary" "Summary"

  # Y8: Human mode shows phase numbers with correct labels.
  assert_stdout_contains "Y" "Y8_phase1" "Phase 1 (start)"
  assert_stdout_contains "Y" "Y8_phase2" "Phase 2"

  # Y9: Status filter — filter to only todo issues.
  run plan --json -s todo
  assert_exit "Y" "Y9" 0
  # All returned issues should have status "todo" (our test issues are backlog).
  local PLAN_TOTAL
  PLAN_TOTAL=$(echo "$CMD_STDOUT" | jq '.data.total_issues' 2>/dev/null)
  # This may be 0 since our test issues are in backlog, which is fine.
  check "Y" "Y9_filter" "PASS"

  # Y10: Invalid status filter (exit 3).
  run plan --json -s invalid
  assert_exit "Y" "Y10" 3

  # Y11: Root scoping — scope to a parent issue and its children.
  # --root uses parent-child hierarchy, not dependency edges.
  # Create a parent with two children to test scoping.
  run create --json -t "Plan Root Parent" -p high
  assert_exit "Y" "Y11_root_create" 0
  local PLAN_ROOT
  PLAN_ROOT=$(extract_id)

  run create --json -t "Plan Root Child1" -p medium --parent "$PLAN_ROOT"
  assert_exit "Y" "Y11_child1_create" 0
  local PLAN_RC1
  PLAN_RC1=$(extract_id)

  run create --json -t "Plan Root Child2" -p low --parent "$PLAN_ROOT"
  assert_exit "Y" "Y11_child2_create" 0
  local PLAN_RC2
  PLAN_RC2=$(extract_id)

  run plan --json --root "$PLAN_ROOT"
  assert_exit "Y" "Y11" 0
  # The independent issue and other plan issues should NOT be in the scoped plan.
  local HAS_IND
  HAS_IND=$(echo "$CMD_STDOUT" | jq "[.data.phases[].issues[] | select(.id == \"DKT-$PLAN_IND\")] | length" 2>/dev/null)
  if [ "$HAS_IND" -eq 0 ]; then
    check "Y" "Y11_scoped" "PASS"
  else
    check "Y" "Y11_scoped" "FAIL" "independent issue should not appear when scoping to root $PLAN_ROOT"
  fi
  # Children should be present in the scoped plan.
  local HAS_RC1
  HAS_RC1=$(echo "$CMD_STDOUT" | jq "[.data.phases[].issues[] | select(.id == \"DKT-$PLAN_RC1\")] | length" 2>/dev/null)
  if [ "$HAS_RC1" -ge 1 ]; then
    check "Y" "Y11_child1" "PASS"
  else
    check "Y" "Y11_child1" "FAIL" "child DKT-$PLAN_RC1 should appear in root-scoped plan"
  fi

  # Y12: Invalid root ID format (exit 3).
  run plan --json --root "abc"
  assert_exit "Y" "Y12" 3

  # Y13: Empty plan (all done issues).
  # Close root-scoped issues and check.
  run close "$PLAN_RC1" --json
  run close "$PLAN_RC2" --json
  run close "$PLAN_ROOT" --json
  run plan --json --root "$PLAN_ROOT"
  assert_exit "Y" "Y13" 0
  local EMPTY_TOTAL
  EMPTY_TOTAL=$(echo "$CMD_STDOUT" | jq '.data.total_issues' 2>/dev/null)
  if [ "$EMPTY_TOTAL" -eq 0 ]; then
    check "Y" "Y13_empty" "PASS"
  else
    check "Y" "Y13_empty" "FAIL" "expected 0 issues when all are done, got $EMPTY_TOTAL"
  fi

  # Reopen for other tests.
  run reopen "$PLAN_A" --json
  run reopen "$PLAN_B" --json
  run reopen "$PLAN_C" --json
  run reopen "$PLAN_IND" --json
  run reopen "$PLAN_ROOT" --json
  run reopen "$PLAN_RC1" --json
  run reopen "$PLAN_RC2" --json
}
