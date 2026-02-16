#!/usr/bin/env bash
# Section W: Link Commands (link, unlink, links)

test_w_link() {
  printf "Section W: Link Commands"

  # Setup: create 4 issues for relation tests.
  run issue create --json -t "Link Source"
  assert_exit "W" "W0_src" 0
  local LINK_SRC
  LINK_SRC=$(extract_id)

  run issue create --json -t "Link Target"
  assert_exit "W" "W0_tgt" 0
  local LINK_TGT
  LINK_TGT=$(extract_id)

  run issue create --json -t "Link Third"
  assert_exit "W" "W0_third" 0
  local LINK_THIRD
  LINK_THIRD=$(extract_id)

  run issue create --json -t "Link Fourth"
  assert_exit "W" "W0_fourth" 0
  local LINK_FOURTH
  LINK_FOURTH=$(extract_id)

  # W1: docket link basic (JSON) — create a blocks relation
  run issue link add "$LINK_SRC" blocks "$LINK_TGT" --json
  assert_exit "W" "W1" 0
  assert_json "W" "W1_ok" ".ok" "true"

  # W2: JSON contract — data contains source_issue_id, target_issue_id, relation_type
  assert_json_exists "W" "W2_source" ".data.source_issue_id"
  assert_json_exists "W" "W2_target" ".data.target_issue_id"
  assert_json "W" "W2_type" ".data.relation_type" "blocks"

  # W3: Hyphenated relation type normalizes (depends-on -> depends_on)
  run issue link add "$LINK_THIRD" depends-on "$LINK_SRC" --json
  assert_exit "W" "W3" 0
  assert_json "W" "W3_type" ".data.relation_type" "depends_on"

  # W4: Human mode output contains "Linked"
  run issue link add "$LINK_SRC" relates-to "$LINK_THIRD"
  assert_exit "W" "W4" 0
  assert_stdout_contains "W" "W4" "Linked"

  # W5: Self-referential rejection (exit 3)
  run issue link add "$LINK_SRC" blocks "$LINK_SRC" --json
  assert_exit "W" "W5" 3

  # W6: Duplicate relation rejection (exit 4)
  run issue link add "$LINK_SRC" blocks "$LINK_TGT" --json
  assert_exit "W" "W6" 4

  # W7: Inverse duplicate rejection (exit 4)
  # A blocks B exists from W1, now try B blocks A
  run issue link add "$LINK_TGT" blocks "$LINK_SRC" --json
  assert_exit "W" "W7" 4

  # W8: Cycle detection for blocks (exit 4)
  # SRC blocks TGT from W1. Create TGT blocks FOURTH, then FOURTH blocks SRC.
  run issue link add "$LINK_TGT" blocks "$LINK_FOURTH" --json
  assert_exit "W" "W8_setup" 0
  run issue link add "$LINK_FOURTH" blocks "$LINK_SRC" --json
  assert_exit "W" "W8" 4

  # W9: Cycle detection for depends_on (exit 4)
  # THIRD depends_on SRC from W3. Trying SRC depends_on THIRD creates a cycle.
  run issue link add "$LINK_SRC" depends-on "$LINK_THIRD" --json
  assert_exit "W" "W9" 4

  # W10: DKT-prefix accepted
  run issue link add "DKT-$LINK_SRC" duplicates "DKT-$LINK_FOURTH" --json
  assert_exit "W" "W10" 0

  # W11: Invalid relation type (exit 3)
  run issue link add "$LINK_SRC" invalid-type "$LINK_TGT" --json
  assert_exit "W" "W11" 3

  # W12: Link to non-existent issue (exit 2)
  run issue link add "$LINK_SRC" blocks 9999 --json
  assert_exit "W" "W12" 2

  # W13: Invalid issue ID (exit 3)
  run issue link add "abc" blocks "$LINK_TGT" --json
  assert_exit "W" "W13" 3

  # W14: docket links (JSON) — shows all relations
  run issue link list "$LINK_SRC" --json
  assert_exit "W" "W14" 0
  assert_json "W" "W14_ok" ".ok" "true"
  assert_json_array_min "W" "W14_len" ".data" 1

  # W15: JSON contract — each item has relation_type and issue_id
  assert_json_all "W" "W15_type" ".data" '.relation_type != null and .relation_type != ""'
  assert_json_all "W" "W15_issue" ".data" '.issue_id != null and .issue_id != ""'

  # W16: Computed inverses (blocked_by appears for target)
  # TGT has "blocked_by SRC" since SRC blocks TGT
  run issue link list "$LINK_TGT" --json
  assert_exit "W" "W16" 0
  local HAS_BLOCKED_BY
  HAS_BLOCKED_BY=$(echo "$CMD_STDOUT" | jq '[.data[] | select(.relation_type == "blocked_by")] | length' 2>/dev/null)
  if [ "$HAS_BLOCKED_BY" -ge 1 ]; then
    check "W" "W16_inverse" "PASS"
  else
    check "W" "W16_inverse" "FAIL" "expected 'blocked_by' in links for target issue"
  fi

  # W17: Human mode shows "blocks"
  run issue link list "$LINK_SRC"
  assert_exit "W" "W17" 0
  assert_stdout_contains "W" "W17" "blocks"

  # W18: Empty relations for isolated issue
  run issue create --json -t "Isolated Issue"
  assert_exit "W" "W18_setup" 0
  local ISOLATED_ID
  ISOLATED_ID=$(extract_id)
  run issue link list "$ISOLATED_ID" --json
  assert_exit "W" "W18" 0
  assert_json "W" "W18_ok" ".ok" "true"
  assert_json "W" "W18_msg" ".message" "No relations found for DKT-$ISOLATED_ID"

  # W18b: Links for non-existent issue returns exit 2
  run issue link list 9999 --json
  assert_exit "W" "W18b" 2

  # W19: docket unlink (JSON) — remove a relation
  # Remove the duplicates relation created in W10
  run issue link remove "$LINK_SRC" duplicates "$LINK_FOURTH" --json
  assert_exit "W" "W19" 0
  assert_json "W" "W19_ok" ".ok" "true"

  # W20: Unlink human mode contains "Unlinked"
  # First re-create the relation to remove it in human mode
  run issue link add "$LINK_SRC" duplicates "$LINK_FOURTH" --json
  assert_exit "W" "W20_setup" 0
  run issue link remove "$LINK_SRC" duplicates "$LINK_FOURTH"
  assert_exit "W" "W20" 0
  assert_stdout_contains "W" "W20" "Unlinked"

  # W21: Unlink non-existent relation (exit 2)
  run issue link remove "$LINK_SRC" duplicates "$LINK_FOURTH" --json
  assert_exit "W" "W21" 2

  # W22: Activity recorded on both issues
  run issue show "$LINK_SRC" --json
  assert_exit "W" "W22" 0
  local REL_ADDED
  REL_ADDED=$(echo "$CMD_STDOUT" | jq '[.data.activity[] | select(.field_changed == "relation_added")] | length' 2>/dev/null)
  if [ "$REL_ADDED" -ge 1 ]; then
    check "W" "W22_activity" "PASS"
  else
    check "W" "W22_activity" "FAIL" "expected relation_added activity on source issue"
  fi

  # W23: Relations visible in show command
  run issue show "$LINK_SRC" --json
  assert_exit "W" "W23" 0
  local REL_COUNT
  REL_COUNT=$(echo "$CMD_STDOUT" | jq '.data.relations | length' 2>/dev/null)
  if [ "$REL_COUNT" -ge 1 ]; then
    check "W" "W23_show" "PASS"
  else
    check "W" "W23_show" "FAIL" "expected relations in show output"
  fi
}
