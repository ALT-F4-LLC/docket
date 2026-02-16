#!/usr/bin/env bash
# Section N: Delete (Simple)

test_n_delete_simple() {
  printf "Section N: Delete (Simple)"

  run issue create --json -t "Delete Me"
  assert_exit "N" "N1" 0
  local DEL_ID
  DEL_ID=$(extract_id)

  run issue delete "$DEL_ID" --json
  assert_exit "N" "N2" 0
  assert_json "N" "N2" ".ok" "true"

  run issue show "$DEL_ID" --json
  assert_exit "N" "N3" 2

  run issue delete 9999 --json
  assert_exit "N" "N4" 2

  run issue delete
  assert_exit_nonzero "N" "N5"
}
