#!/usr/bin/env bash
# Section ZA: Stats Command

test_za_stats() {
  printf "Section ZA: Stats Command"

  # ZA1: Stats JSON mode — basic contract.
  run stats --json
  assert_exit "ZA" "ZA1" 0
  assert_json "ZA" "ZA1_ok" ".ok" "true"

  # ZA2: JSON has all top-level fields.
  assert_json_exists "ZA" "ZA2_total" ".data.total"
  assert_json_exists "ZA" "ZA2_root" ".data.root_issues"
  assert_json_exists "ZA" "ZA2_sub" ".data.sub_issues"
  assert_json_exists "ZA" "ZA2_status" ".data.by_status"
  assert_json_exists "ZA" "ZA2_priority" ".data.by_priority"
  assert_json_exists "ZA" "ZA2_labels" ".data.labels"

  # ZA3: Total > 0 (we have issues from previous sections).
  local TOTAL
  TOTAL=$(echo "$CMD_STDOUT" | jq '.data.total' 2>/dev/null)
  check_cond "ZA" "ZA3_total" "expected total > 0, got $TOTAL" [ "$TOTAL" -gt 0 ]

  # ZA4: root_issues + sub_issues = total.
  local ROOT SUB
  ROOT=$(echo "$CMD_STDOUT" | jq '.data.root_issues' 2>/dev/null)
  SUB=$(echo "$CMD_STDOUT" | jq '.data.sub_issues' 2>/dev/null)
  local SUM=$((ROOT + SUB))
  check_cond "ZA" "ZA4_sum" "root ($ROOT) + sub ($SUB) = $SUM, expected $TOTAL" [ "$SUM" -eq "$TOTAL" ]

  # ZA5: by_status has at least one entry.
  local STATUS_KEYS
  STATUS_KEYS=$(echo "$CMD_STDOUT" | jq '.data.by_status | length' 2>/dev/null)
  check_cond "ZA" "ZA5_status" "by_status has no entries" [ "$STATUS_KEYS" -gt 0 ]

  # ZA6: by_priority has at least one entry.
  local PRIO_KEYS
  PRIO_KEYS=$(echo "$CMD_STDOUT" | jq '.data.by_priority | length' 2>/dev/null)
  check_cond "ZA" "ZA6_priority" "by_priority has no entries" [ "$PRIO_KEYS" -gt 0 ]

  # ZA7: Human mode output works.
  run stats
  assert_exit "ZA" "ZA7" 0

  # ZA8: Human output contains expected section headers.
  assert_stdout_contains "ZA" "ZA8_overview" "Total issues"
  assert_stdout_contains "ZA" "ZA8_status" "Status"
  assert_stdout_contains "ZA" "ZA8_priority" "Priority"
  assert_stdout_contains "ZA" "ZA8_labels" "Labels"

  # ZA9: Empty database — stats shows zeros.
  local EMPTY_DIR
  EMPTY_DIR=$(qa_mktemp_d)
  run_env "$EMPTY_DIR" init --json
  assert_exit "ZA" "ZA9_init" 0
  run_env "$EMPTY_DIR" stats --json
  assert_exit "ZA" "ZA9" 0
  assert_json "ZA" "ZA9_ok" ".ok" "true"
  assert_json "ZA" "ZA9_total" ".data.total" "0"
  assert_json "ZA" "ZA9_root" ".data.root_issues" "0"
  assert_json "ZA" "ZA9_sub" ".data.sub_issues" "0"
  rm -rf "$EMPTY_DIR"
}
