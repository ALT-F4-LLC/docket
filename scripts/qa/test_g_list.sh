#!/usr/bin/env bash
# Section G: List Command

test_g_list() {
  printf "Section G: List Command\n"

  run list --json
  assert_exit "G" "G1" 0
  assert_json "G" "G1" ".ok" "true"
  assert_json_array_min "G" "G1" ".data.issues" 1

  run ls --json
  assert_exit "G" "G2" 0
  assert_json "G" "G2" ".ok" "true"

  run list --json -s todo
  assert_exit "G" "G3" 0
  assert_json_all "G" "G3" ".data.issues" '.status == "todo"'

  run list --json -p high
  assert_exit "G" "G4" 0
  assert_json_all "G" "G4" ".data.issues" '.priority == "high"'

  run list --json -T bug
  assert_exit "G" "G5" 0
  assert_json_all "G" "G5" ".data.issues" '.kind == "bug"'

  run list --json -a alice
  assert_exit "G" "G6" 0
  assert_json_all "G" "G6" ".data.issues" '.assignee == "alice"'

  run list --json --roots
  assert_exit "G" "G7" 0
  assert_json_all "G" "G7" ".data.issues" '.parent_id == null'

  run list --json --parent DKT-1
  assert_exit "G" "G8" 0
  assert_json_all "G" "G8" ".data.issues" '.parent_id == "DKT-1"'

  run list --json --sort created_at:asc
  assert_exit "G" "G9" 0

  run list --json --limit 2
  assert_exit "G" "G10" 0
  assert_json_array_max "G" "G10" ".data.issues" 2

  run list
  assert_exit "G" "G11" 0

  run list --tree
  assert_exit "G" "G12" 0
}
