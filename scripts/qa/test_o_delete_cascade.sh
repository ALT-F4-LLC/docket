#!/usr/bin/env bash
# Section O: Delete (Cascade & Orphan)

test_o_delete_cascade() {
  printf "Section O: Delete (Cascade & Orphan)"

  run issue create --json -t "Cascade Parent"
  assert_exit "O" "O1" 0
  local CASCADE_PARENT
  CASCADE_PARENT=$(extract_id)

  run issue create --json -t "Cascade Child 1" --parent "$CASCADE_PARENT"
  assert_exit "O" "O2" 0
  local CASCADE_CHILD1
  CASCADE_CHILD1=$(extract_id)

  run issue create --json -t "Cascade Child 2" --parent "$CASCADE_PARENT"
  assert_exit "O" "O3" 0
  local CASCADE_CHILD2
  CASCADE_CHILD2=$(extract_id)

  run issue delete "$CASCADE_PARENT" --json
  assert_exit "O" "O4" 3

  run issue delete "$CASCADE_PARENT" --json --force --orphan
  assert_exit "O" "O5" 3

  run issue delete "$CASCADE_PARENT" --json --force
  assert_exit "O" "O6" 0
  assert_json "O" "O6" ".ok" "true"

  run issue show "$CASCADE_PARENT" --json
  assert_exit "O" "O7" 2

  run issue show "$CASCADE_CHILD1" --json
  assert_exit "O" "O8" 2

  run issue show "$CASCADE_CHILD2" --json
  assert_exit "O" "O9" 2

  run issue create --json -t "Orphan Parent"
  assert_exit "O" "O10" 0
  local ORPHAN_PARENT
  ORPHAN_PARENT=$(extract_id)

  run issue create --json -t "Orphan Child 1" --parent "$ORPHAN_PARENT"
  assert_exit "O" "O11" 0
  local ORPHAN_CHILD1
  ORPHAN_CHILD1=$(extract_id)

  run issue create --json -t "Orphan Child 2" --parent "$ORPHAN_PARENT"
  assert_exit "O" "O12" 0
  local ORPHAN_CHILD2
  ORPHAN_CHILD2=$(extract_id)

  run issue delete "$ORPHAN_PARENT" --json --orphan
  assert_exit "O" "O13" 0
  assert_json "O" "O13" ".ok" "true"

  run issue show "$ORPHAN_PARENT" --json
  assert_exit "O" "O14" 2

  run issue show "$ORPHAN_CHILD1" --json
  assert_exit "O" "O15" 0
  assert_json_null "O" "O15" ".data.parent_id"

  run issue show "$ORPHAN_CHILD2" --json
  assert_exit "O" "O16" 0
  assert_json_null "O" "O16" ".data.parent_id"
}
