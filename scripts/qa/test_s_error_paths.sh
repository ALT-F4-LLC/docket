#!/usr/bin/env bash
# Section S: Error Paths (No DB)

test_s_error_paths() {
  printf "Section S: Error Paths (No DB)"
  local NO_DB_DIR2
  NO_DB_DIR2=$(mktemp -d)
  mkdir -p "$NO_DB_DIR2"

  run_env "$NO_DB_DIR2" config
  assert_exit "S" "S1" 0

  run_env "$NO_DB_DIR2" --help
  assert_exit "S" "S2" 0

  run_env "$NO_DB_DIR2" issue create --json -t "No DB"
  assert_exit_nonzero "S" "S3"

  run_env "$NO_DB_DIR2" issue list --json
  assert_exit_nonzero "S" "S4"

  run_env "$NO_DB_DIR2" issue show 1 --json
  assert_exit_nonzero "S" "S5"

  run_env "$NO_DB_DIR2" issue move 1 todo --json
  assert_exit_nonzero "S" "S6"

  run_env "$NO_DB_DIR2" issue close 1 --json
  assert_exit_nonzero "S" "S7"

  run_env "$NO_DB_DIR2" issue reopen 1 --json
  assert_exit_nonzero "S" "S8"

  run_env "$NO_DB_DIR2" issue edit 1 --json -t "X"
  assert_exit_nonzero "S" "S9"

  run_env "$NO_DB_DIR2" issue delete 1 --json
  assert_exit_nonzero "S" "S10"

  run_env "$NO_DB_DIR2" issue comment add 1 --json -m "test"
  assert_exit_nonzero "S" "S11"

  run_env "$NO_DB_DIR2" issue comment list 1 --json
  assert_exit_nonzero "S" "S12"

  run_env "$NO_DB_DIR2" issue label add 1 "bug" --json
  assert_exit_nonzero "S" "S13"

  run_env "$NO_DB_DIR2" issue label rm 1 "bug" --json
  assert_exit_nonzero "S" "S14"

  run_env "$NO_DB_DIR2" issue label list --json
  assert_exit_nonzero "S" "S15"

  run_env "$NO_DB_DIR2" issue label delete "bug" --force --json
  assert_exit_nonzero "S" "S16"

  # S17: link without DB
  run_env "$NO_DB_DIR2" issue link add 1 blocks 2 --json
  assert_exit_nonzero "S" "S17"

  # S18: unlink without DB
  run_env "$NO_DB_DIR2" issue link remove 1 blocks 2 --json
  assert_exit_nonzero "S" "S18"

  # S19: links without DB
  run_env "$NO_DB_DIR2" issue link list 1 --json
  assert_exit_nonzero "S" "S19"

  # S20: next without DB
  run_env "$NO_DB_DIR2" next --json
  assert_exit_nonzero "S" "S20"

  # S21: plan without DB
  run_env "$NO_DB_DIR2" plan --json
  assert_exit_nonzero "S" "S21"

  # S22: graph without DB
  run_env "$NO_DB_DIR2" issue graph 1 --json
  assert_exit_nonzero "S" "S22"

  # S23: log without DB
  run_env "$NO_DB_DIR2" issue log 1 --json
  assert_exit_nonzero "S" "S23"

  # S24: stats without DB
  run_env "$NO_DB_DIR2" stats --json
  assert_exit_nonzero "S" "S24"

  # S25: board without DB
  run_env "$NO_DB_DIR2" board --json
  assert_exit_nonzero "S" "S25"

  # S26: export without DB
  run_env "$NO_DB_DIR2" export
  assert_exit_nonzero "S" "S26"

  rm -rf "$NO_DB_DIR2"
}
