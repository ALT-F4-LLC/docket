#!/usr/bin/env bash
# Section M: Edit Reparenting

test_m_edit_reparent() {
  printf "Section M: Edit Reparenting\n"

  run create --json -t "Parent Issue"
  assert_exit "M" "M1" 0
  local PARENT_ID
  PARENT_ID=$(extract_id)

  run create --json -t "Child Issue" --parent "$PARENT_ID"
  assert_exit "M" "M2" 0
  local CHILD_ID
  CHILD_ID=$(extract_id)

  run create --json -t "Grandchild" --parent "$CHILD_ID"
  assert_exit "M" "M3" 0
  local GRANDCHILD_ID
  GRANDCHILD_ID=$(extract_id)

  run edit "$CHILD_ID" --json --parent "$PARENT_ID"
  assert_exit "M" "M4" 0

  run edit "$CHILD_ID" --json --parent none
  assert_exit "M" "M5" 0
  assert_json_null "M" "M5" ".data.parent_id"

  run edit "$CHILD_ID" --json --parent "$PARENT_ID"
  assert_exit "M" "M6" 0

  run edit "$PARENT_ID" --json --parent "$GRANDCHILD_ID"
  assert_exit "M" "M7" 4

  run edit "$PARENT_ID" --json --parent "$CHILD_ID"
  assert_exit "M" "M8" 4

  run edit "$CHILD_ID" --json --parent "$CHILD_ID"
  assert_exit "M" "M9" 3

  run edit "$CHILD_ID" --json --parent 9999
  assert_exit "M" "M10" 2

  run edit "$CHILD_ID" --json --parent 0
  assert_exit "M" "M11" 0
  assert_json_null "M" "M11" ".data.parent_id"
}
