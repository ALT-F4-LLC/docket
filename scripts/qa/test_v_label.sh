#!/usr/bin/env bash
# Section V: Label Commands

test_v_label() {
  printf "Section V: Label Commands\n"

  # Create a dedicated issue for label tests.
  run create --json -t "Label Test Issue"
  assert_exit "V" "V0_setup" 0
  local LABEL_ISSUE_ID
  LABEL_ISSUE_ID=$(extract_id)

  # V1: label add (JSON) — add a single label to an issue
  run label add "$LABEL_ISSUE_ID" "bug" --json
  assert_exit "V" "V1" 0
  assert_json "V" "V1_ok" ".ok" "true"

  # V2: JSON contract — data is an array of label objects with id, name, color(optional)
  assert_json_all "V" "V2_name" ".data" '.name != null and .name != ""'
  assert_json_all "V" "V2_id" ".data" '.id != null'
  assert_json_array_min "V" "V2_len" ".data" 1

  # V3: label add multiple labels at once
  run label add "$LABEL_ISSUE_ID" "frontend" "urgent" --json
  assert_exit "V" "V3" 0
  assert_json_array_min "V" "V3_len" ".data" 3

  # V4: label add with --color flag (creates new label with color)
  run label add "$LABEL_ISSUE_ID" "critical" --color "#ff0000" --json
  assert_exit "V" "V4" 0
  local HAS_COLOR
  HAS_COLOR=$(echo "$CMD_STDOUT" | jq '[.data[] | select(.name == "critical" and .color == "#ff0000")] | length' 2>/dev/null)
  if [ "$HAS_COLOR" -ge 1 ]; then
    check "V" "V4_color" "PASS"
  else
    check "V" "V4_color" "FAIL" "expected label 'critical' with color '#ff0000'"
  fi

  # V5: label add human mode output
  run label add "$LABEL_ISSUE_ID" "docs"
  assert_exit "V" "V5" 0
  assert_stdout_contains "V" "V5" "Added label(s) to DKT-$LABEL_ISSUE_ID"

  # V6: label add to non-existent issue -> exit 2
  run label add 9999 "bug" --json
  assert_exit "V" "V6" 2

  # V7: label add with invalid issue ID -> exit 3
  run label add "abc" "bug" --json
  assert_exit "V" "V7" 3

  # V8: label add with no args -> error
  run label add
  assert_exit_nonzero "V" "V8"

  # V9: label rm (JSON) — remove a label
  run label rm "$LABEL_ISSUE_ID" "docs" --json
  assert_exit "V" "V9" 0
  assert_json "V" "V9_ok" ".ok" "true"
  # Verify "docs" is no longer in the returned labels
  local DOCS_REMAINING
  DOCS_REMAINING=$(echo "$CMD_STDOUT" | jq '[.data[] | select(.name == "docs")] | length' 2>/dev/null)
  if [ "$DOCS_REMAINING" -eq 0 ]; then
    check "V" "V9_removed" "PASS"
  else
    check "V" "V9_removed" "FAIL" "label 'docs' still present after rm"
  fi

  # V10: label rm non-existent label -> exit 2
  run label rm "$LABEL_ISSUE_ID" "nonexistent" --json
  assert_exit "V" "V10" 2

  # V11: label rm label not attached to issue -> exit 3
  # "docs" was removed from the issue in V9 but still exists as a global label.
  # Verify it still appears in label list before testing the not-attached error.
  run label list --json
  assert_exit "V" "V11_pre" 0
  local DOCS_EXISTS
  DOCS_EXISTS=$(echo "$CMD_STDOUT" | jq '[.data[] | select(.name == "docs")] | length' 2>/dev/null)
  if [ "$DOCS_EXISTS" -ge 1 ]; then
    check "V" "V11_exists" "PASS"
  else
    check "V" "V11_exists" "FAIL" "label 'docs' should still exist after rm from issue"
  fi
  run label rm "$LABEL_ISSUE_ID" "docs" --json
  assert_exit "V" "V11" 3

  # V12: label rm human mode
  run label add "$LABEL_ISSUE_ID" "temp-label" --json
  assert_exit "V" "V12_setup" 0
  run label rm "$LABEL_ISSUE_ID" "temp-label"
  assert_exit "V" "V12" 0
  assert_stdout_contains "V" "V12" "Removed label(s) from DKT-$LABEL_ISSUE_ID"

  # V13: label list (JSON) — shows labels with issue counts
  run label list --json
  assert_exit "V" "V13" 0
  assert_json "V" "V13_ok" ".ok" "true"
  assert_json_array_min "V" "V13_len" ".data" 1
  assert_json_all "V" "V13_name" ".data" '.name != null and .name != ""'
  assert_json_all "V" "V13_count" ".data" '.issue_count != null'

  # V14: label list human mode — table format
  run label list
  assert_exit "V" "V14" 0
  assert_stdout_contains "V" "V14_hdr" "NAME"
  assert_stdout_contains "V" "V14_hdr2" "ISSUES"

  # V15: label delete with --force (JSON)
  # First add a disposable label
  run label add "$LABEL_ISSUE_ID" "disposable" --json
  assert_exit "V" "V15_setup" 0
  run label delete "disposable" --force --json
  assert_exit "V" "V15" 0
  assert_json "V" "V15_ok" ".ok" "true"
  assert_json "V" "V15_name" ".data.name" "disposable"

  # V16: label delete non-existent label -> exit 2
  run label delete "ghost-label" --force --json
  assert_exit "V" "V16" 2

  # V17: label delete without --force in JSON mode -> exit 3
  run label add "$LABEL_ISSUE_ID" "no-force-test" --json
  assert_exit "V" "V17_setup" 0
  run label delete "no-force-test" --json
  assert_exit "V" "V17" 3

  # Clean up the label left from V17
  run label delete "no-force-test" --force --json
  assert_exit "V" "V17_cleanup" 0

  # V18: label add idempotent (adding same label twice doesn't error)
  run label add "$LABEL_ISSUE_ID" "bug" --json
  assert_exit "V" "V18" 0

  # V19: DKT-prefix accepted for label add/rm
  run label add "DKT-$LABEL_ISSUE_ID" "dkt-prefix-test" --json
  assert_exit "V" "V19_add" 0
  run label rm "DKT-$LABEL_ISSUE_ID" "dkt-prefix-test" --json
  assert_exit "V" "V19_rm" 0

  # V20: activity log records label_added and label_removed
  run show "$LABEL_ISSUE_ID" --json
  assert_exit "V" "V20" 0
  local LABEL_ADDED_COUNT
  LABEL_ADDED_COUNT=$(echo "$CMD_STDOUT" | jq '[.data.activity[] | select(.field_changed == "label_added")] | length' 2>/dev/null)
  if [ "$LABEL_ADDED_COUNT" -ge 1 ]; then
    check "V" "V20_added" "PASS"
  else
    check "V" "V20_added" "FAIL" "expected >= 1 label_added activity entries, got $LABEL_ADDED_COUNT"
  fi
  local LABEL_REMOVED_COUNT
  LABEL_REMOVED_COUNT=$(echo "$CMD_STDOUT" | jq '[.data.activity[] | select(.field_changed == "label_removed")] | length' 2>/dev/null)
  if [ "$LABEL_REMOVED_COUNT" -ge 1 ]; then
    check "V" "V20_removed" "PASS"
  else
    check "V" "V20_removed" "FAIL" "expected >= 1 label_removed activity entries, got $LABEL_REMOVED_COUNT"
  fi

  # V21: label list after deletions shows updated counts
  # "disposable" was deleted in V15; verify it no longer appears
  run label list --json
  assert_exit "V" "V21" 0
  local DISPOSABLE_COUNT
  DISPOSABLE_COUNT=$(echo "$CMD_STDOUT" | jq '[.data[] | select(.name == "disposable")] | length' 2>/dev/null)
  if [ "$DISPOSABLE_COUNT" -eq 0 ]; then
    check "V" "V21_gone" "PASS"
  else
    check "V" "V21_gone" "FAIL" "label 'disposable' still appears after delete"
  fi

  # V22: label add with --color on existing label should fail
  # "bug" already exists from V1
  run label add "$LABEL_ISSUE_ID" "bug" --color "#00ff00" --json
  assert_exit_nonzero "V" "V22"
}
