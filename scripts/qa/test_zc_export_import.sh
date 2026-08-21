#!/usr/bin/env bash
# Section ZC: Export and Import Commands

test_zc_export_import() {
  printf "Section ZC: Export and Import Commands"

  # ---------------------------------------------------------------------------
  # Export tests (using shared DB which has data from previous sections)
  # ---------------------------------------------------------------------------

  # ZC1: Export JSON to stdout succeeds.
  run export
  assert_exit "ZC" "ZC1" 0

  # ZC2: JSON output is valid and contains required top-level keys.
  if echo "$CMD_STDOUT" | jq -e '.version' >/dev/null 2>&1; then
    check "ZC" "ZC2_valid" "PASS"
  else
    check "ZC" "ZC2_valid" "FAIL" "export output is not valid JSON"
  fi

  # Verify all required top-level keys exist.
  local KEY
  for KEY in version exported_at issues comments relations labels issue_label_mappings; do
    if echo "$CMD_STDOUT" | jq -e ".$KEY" >/dev/null 2>&1; then
      check "ZC" "ZC2_key_$KEY" "PASS"
    else
      check "ZC" "ZC2_key_$KEY" "FAIL" "missing top-level key: $KEY"
    fi
  done

  # ZC3: Export JSON has version = 1.
  local VERSION
  VERSION=$(echo "$CMD_STDOUT" | jq '.version' 2>/dev/null)
  check_cond "ZC" "ZC3_version" "expected version 1, got $VERSION" [ "$VERSION" = "1" ]

  # ZC4: Export JSON has non-empty issues array.
  local ISSUE_COUNT
  ISSUE_COUNT=$(echo "$CMD_STDOUT" | jq '.issues | length' 2>/dev/null)
  check_cond "ZC" "ZC4_issues" "expected issues array to be non-empty" [ "$ISSUE_COUNT" -gt 0 ]

  # Save JSON export for later import tests.
  local EXPORT_JSON="$CMD_STDOUT"

  # ZC5: Export CSV format.
  run export -o csv
  assert_exit "ZC" "ZC5" 0
  assert_stdout_contains "ZC" "ZC5_header" "id,parent_id,title,description,status,priority,type,assignee,labels,files,created_at,updated_at"

  # ZC6: Export Markdown format.
  run export -o markdown
  assert_exit "ZC" "ZC6" 0
  assert_stdout_contains "ZC" "ZC6_header" "# Docket Export"

  # ZC7: Export to file with --file flag.
  local EXPORT_FILE
  EXPORT_FILE=$(qa_mktemp)
  run export -f "$EXPORT_FILE"
  assert_exit "ZC" "ZC7" 0
  # Verify file was written and contains valid JSON.
  if [ -s "$EXPORT_FILE" ]; then
    if jq -e '.version' "$EXPORT_FILE" >/dev/null 2>&1; then
      check "ZC" "ZC7_file" "PASS"
    else
      check "ZC" "ZC7_file" "FAIL" "exported file is not valid JSON"
    fi
  else
    check "ZC" "ZC7_file" "FAIL" "exported file is empty or missing"
  fi
  rm -f "$EXPORT_FILE"

  # ZC8: Export with --status filter reduces output.
  run export -s done
  assert_exit "ZC" "ZC8" 0
  local DONE_COUNT
  DONE_COUNT=$(echo "$CMD_STDOUT" | jq '.issues | length' 2>/dev/null)
  # Filtered count should be <= total count.
  check_cond "ZC" "ZC8_filter" "filtered count ($DONE_COUNT) > total count ($ISSUE_COUNT)" [ "$DONE_COUNT" -le "$ISSUE_COUNT" ]
  # All filtered issues should have status "done".
  local BAD_STATUS
  BAD_STATUS=$(echo "$CMD_STDOUT" | jq '[.issues[] | select(.status != "done")] | length' 2>/dev/null)
  check_cond "ZC" "ZC8_status" "$BAD_STATUS issues with non-done status in filtered export" [ "$BAD_STATUS" = "0" ]

  # ZC9: Invalid format flag returns error.
  run export -o xml
  assert_exit_nonzero "ZC" "ZC9"

  # ---------------------------------------------------------------------------
  # Import tests (using fresh temp directories for isolated DBs)
  # ---------------------------------------------------------------------------

  # Write the saved JSON export to a temp file for import tests.
  local IMPORT_FILE
  IMPORT_FILE=$(qa_mktemp)
  echo "$EXPORT_JSON" > "$IMPORT_FILE"

  # ZC10: Import into empty DB succeeds (round-trip).
  local IMPORT_DIR
  IMPORT_DIR=$(qa_mktemp_d)
  run_env "$IMPORT_DIR" init --json
  assert_exit "ZC" "ZC10_init" 0
  run_env "$IMPORT_DIR" import --json "$IMPORT_FILE"
  assert_exit "ZC" "ZC10" 0
  assert_json "ZC" "ZC10_ok" ".ok" "true"
  assert_json_exists "ZC" "ZC10_imported" ".data.imported"
  # Imported count should be > 0.
  local IMPORTED
  IMPORTED=$(echo "$CMD_STDOUT" | jq '.data.imported' 2>/dev/null)
  check_cond "ZC" "ZC10_count" "expected imported > 0, got $IMPORTED" [ "$IMPORTED" -gt 0 ]

  # ZC11: Verify round-trip — exported issue count matches imported DB.
  run_env "$IMPORT_DIR" stats --json
  assert_exit "ZC" "ZC11" 0
  local RT_TOTAL
  RT_TOTAL=$(echo "$CMD_STDOUT" | jq '.data.total' 2>/dev/null)
  check_cond "ZC" "ZC11_roundtrip" "round-trip total $RT_TOTAL != original $ISSUE_COUNT" [ "$RT_TOTAL" = "$ISSUE_COUNT" ]
  rm -rf "$IMPORT_DIR"

  # ZC12: Import into non-empty DB without flags fails.
  local NONEMPTY_DIR
  NONEMPTY_DIR=$(qa_mktemp_d)
  run_env "$NONEMPTY_DIR" init --json
  assert_exit "ZC" "ZC12_init" 0
  run_env "$NONEMPTY_DIR" issue create --json -t "Blocker issue"
  assert_exit "ZC" "ZC12_create" 0
  run_env "$NONEMPTY_DIR" import --json "$IMPORT_FILE"
  assert_exit_nonzero "ZC" "ZC12"
  rm -rf "$NONEMPTY_DIR"

  # ZC13: Import with --merge flag succeeds on non-empty DB.
  local MERGE_DIR
  MERGE_DIR=$(qa_mktemp_d)
  run_env "$MERGE_DIR" init --json
  assert_exit "ZC" "ZC13_init" 0
  run_env "$MERGE_DIR" issue create --json -t "Pre-existing issue"
  assert_exit "ZC" "ZC13_create" 0
  run_env "$MERGE_DIR" import --json --merge "$IMPORT_FILE"
  assert_exit "ZC" "ZC13" 0
  assert_json "ZC" "ZC13_ok" ".ok" "true"
  assert_json_exists "ZC" "ZC13_imported" ".data.imported"
  rm -rf "$MERGE_DIR"

  # ZC14: Import with --replace --json --yes flag succeeds.
  local REPLACE_DIR
  REPLACE_DIR=$(qa_mktemp_d)
  run_env "$REPLACE_DIR" init --json
  assert_exit "ZC" "ZC14_init" 0
  run_env "$REPLACE_DIR" issue create --json -t "Will be replaced"
  assert_exit "ZC" "ZC14_create" 0

  # ZC14a: --replace --json without --yes is refused (DKT-15): --json must
  # never double as consent for a destructive replace.
  run_env "$REPLACE_DIR" import --json --replace "$IMPORT_FILE"
  assert_exit "ZC" "ZC14a" 3
  assert_json "ZC" "ZC14a_code" ".code" "VALIDATION_ERROR"

  run_env "$REPLACE_DIR" import --json --replace --yes "$IMPORT_FILE"
  assert_exit "ZC" "ZC14" 0
  assert_json "ZC" "ZC14_ok" ".ok" "true"
  # After replace, issue count should match the export, not include the pre-existing one.
  run_env "$REPLACE_DIR" stats --json
  assert_exit "ZC" "ZC14_stats" 0
  local REPLACE_TOTAL
  REPLACE_TOTAL=$(echo "$CMD_STDOUT" | jq '.data.total' 2>/dev/null)
  check_cond "ZC" "ZC14_replaced" "after replace, total $REPLACE_TOTAL != export $ISSUE_COUNT" [ "$REPLACE_TOTAL" = "$ISSUE_COUNT" ]
  rm -rf "$REPLACE_DIR"

  # ZC15: --merge and --replace together fails.
  local CONFLICT_DIR
  CONFLICT_DIR=$(qa_mktemp_d)
  run_env "$CONFLICT_DIR" init --json
  assert_exit "ZC" "ZC15_init" 0
  run_env "$CONFLICT_DIR" import --json --merge --replace "$IMPORT_FILE"
  assert_exit_nonzero "ZC" "ZC15"
  rm -rf "$CONFLICT_DIR"

  # ZC16: Import with invalid JSON fails.
  local BAD_FILE
  BAD_FILE=$(qa_mktemp)
  echo "this is not json" > "$BAD_FILE"
  local BAD_DIR
  BAD_DIR=$(qa_mktemp_d)
  run_env "$BAD_DIR" init --json
  assert_exit "ZC" "ZC16_init" 0
  run_env "$BAD_DIR" import --json "$BAD_FILE"
  assert_exit_nonzero "ZC" "ZC16"
  rm -f "$BAD_FILE"
  rm -rf "$BAD_DIR"

  # ZC17: Import with no arguments fails.
  run import --json
  assert_exit_nonzero "ZC" "ZC17"

  # ZC18: Export CSV to file works.
  local CSV_FILE
  CSV_FILE=$(qa_mktemp)
  run export -o csv -f "$CSV_FILE"
  assert_exit "ZC" "ZC18" 0
  if [ -s "$CSV_FILE" ]; then
    if head -1 "$CSV_FILE" | grep -qF "id,parent_id,title"; then
      check "ZC" "ZC18_csv_file" "PASS"
    else
      check "ZC" "ZC18_csv_file" "FAIL" "CSV file missing header row"
    fi
  else
    check "ZC" "ZC18_csv_file" "FAIL" "CSV file is empty or missing"
  fi
  rm -f "$CSV_FILE"

  # ZC19: Export with --label filter.
  run export -l frontend
  assert_exit "ZC" "ZC19" 0
  local LABEL_ISSUE_COUNT
  LABEL_ISSUE_COUNT=$(echo "$CMD_STDOUT" | jq '.issues | length' 2>/dev/null)
  # Filtered count should be <= total unfiltered count.
  check_cond "ZC" "ZC19_filter" "label-filtered count ($LABEL_ISSUE_COUNT) > total ($ISSUE_COUNT)" [ "$LABEL_ISSUE_COUNT" -le "$ISSUE_COUNT" ]

  # ZC20: Import with non-existent file fails.
  run import --json /tmp/docket-qa-nonexistent-file.json
  assert_exit_nonzero "ZC" "ZC20"

  # ZC21: Export with invalid status filter returns validation error.
  run export -s invalid
  assert_exit_nonzero "ZC" "ZC21"

  # Cleanup temp import file.
  rm -f "$IMPORT_FILE"
}
