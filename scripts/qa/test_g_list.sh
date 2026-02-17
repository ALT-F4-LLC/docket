#!/usr/bin/env bash
# Section G: List Command

test_g_list() {
  printf "Section G: List Command"

  run issue list --json
  assert_exit "G" "G1" 0
  assert_json "G" "G1" ".ok" "true"
  assert_json_array_min "G" "G1" ".data.issues" 1

  run issue ls --json
  assert_exit "G" "G2" 0
  assert_json "G" "G2" ".ok" "true"

  run issue list --json -s todo
  assert_exit "G" "G3" 0
  assert_json_all "G" "G3" ".data.issues" '.status == "todo"'

  run issue list --json -p high
  assert_exit "G" "G4" 0
  assert_json_all "G" "G4" ".data.issues" '.priority == "high"'

  run issue list --json -T bug
  assert_exit "G" "G5" 0
  assert_json_all "G" "G5" ".data.issues" '.kind == "bug"'

  run issue list --json -a alice
  assert_exit "G" "G6" 0
  assert_json_all "G" "G6" ".data.issues" '.assignee == "alice"'

  run issue list --json --roots
  assert_exit "G" "G7" 0
  assert_json_all "G" "G7" ".data.issues" '.parent_id == null'

  run issue list --json --parent DKT-1
  assert_exit "G" "G8" 0
  assert_json_all "G" "G8" ".data.issues" '.parent_id == "DKT-1"'

  run issue list --json --sort created_at:asc
  assert_exit "G" "G9" 0

  run issue list --json --limit 2
  assert_exit "G" "G10" 0
  assert_json_array_max "G" "G10" ".data.issues" 2

  run issue list
  assert_exit "G" "G11" 0

  run issue list --tree
  assert_exit "G" "G12" 0

  # G13: label filter — issue with "frontend" label was created in F3.
  run issue list --json -l frontend
  assert_exit "G" "G13" 0
  assert_json_all "G" "G13" ".data.issues" '(.labels | index("frontend")) != null'

  # G14: --all flag includes done issues.
  run issue create --json -t "G14 Done Issue"
  local G14_ID
  G14_ID=$(extract_id)
  run issue close "$G14_ID" --json
  assert_exit "G" "G14_close" 0
  # Without --all, the done issue should not appear.
  run issue list --json
  assert_exit "G" "G14_default" 0
  local G14_HAS_DONE
  G14_HAS_DONE=$(echo "$CMD_STDOUT" | jq "[.data.issues[] | select(.id == \"DKT-$G14_ID\")] | length" 2>/dev/null)
  if [ "$G14_HAS_DONE" -eq 0 ]; then
    check "G" "G14_excluded" "PASS"
  else
    check "G" "G14_excluded" "FAIL" "done issue DKT-$G14_ID should not appear without --all"
  fi
  # With --all, the done issue should appear.
  run issue list --json --all
  assert_exit "G" "G14_all" 0
  local G14_HAS_ALL
  G14_HAS_ALL=$(echo "$CMD_STDOUT" | jq "[.data.issues[] | select(.id == \"DKT-$G14_ID\")] | length" 2>/dev/null)
  if [ "$G14_HAS_ALL" -ge 1 ]; then
    check "G" "G14_included" "PASS"
  else
    check "G" "G14_included" "FAIL" "done issue DKT-$G14_ID should appear with --all"
  fi
  # Reopen to avoid polluting later sections.
  run issue reopen "$G14_ID" --json

  # G15: invalid status filter returns validation error.
  run issue list --json -s invalid
  assert_exit "G" "G15" 3

  # G16: invalid priority filter returns validation error.
  run issue list --json -p invalid
  assert_exit "G" "G16" 3

  # G17: invalid type filter returns validation error.
  run issue list --json -T invalid
  assert_exit "G" "G17" 3
}
