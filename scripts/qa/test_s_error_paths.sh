#!/usr/bin/env bash
# Section S: Error Paths (No DB)

test_s_error_paths() {
  printf "Section S: Error Paths (No DB)\n"
  local NO_DB_DIR2
  NO_DB_DIR2=$(mktemp -d)
  mkdir -p "$NO_DB_DIR2"

  run_env "$NO_DB_DIR2" config
  assert_exit "S" "S1" 0

  run_env "$NO_DB_DIR2" --help
  assert_exit "S" "S2" 0

  run_env "$NO_DB_DIR2" create --json -t "No DB"
  assert_exit_nonzero "S" "S3"

  run_env "$NO_DB_DIR2" list --json
  assert_exit_nonzero "S" "S4"

  run_env "$NO_DB_DIR2" show 1 --json
  assert_exit_nonzero "S" "S5"

  run_env "$NO_DB_DIR2" move 1 todo --json
  assert_exit_nonzero "S" "S6"

  run_env "$NO_DB_DIR2" close 1 --json
  assert_exit_nonzero "S" "S7"

  run_env "$NO_DB_DIR2" reopen 1 --json
  assert_exit_nonzero "S" "S8"

  run_env "$NO_DB_DIR2" edit 1 --json -t "X"
  assert_exit_nonzero "S" "S9"

  run_env "$NO_DB_DIR2" delete 1 --json
  assert_exit_nonzero "S" "S10"

  run_env "$NO_DB_DIR2" comment 1 --json -m "test"
  assert_exit_nonzero "S" "S11"

  run_env "$NO_DB_DIR2" comments 1 --json
  assert_exit_nonzero "S" "S12"

  run_env "$NO_DB_DIR2" label add 1 "bug" --json
  assert_exit_nonzero "S" "S13"

  run_env "$NO_DB_DIR2" label rm 1 "bug" --json
  assert_exit_nonzero "S" "S14"

  run_env "$NO_DB_DIR2" label list --json
  assert_exit_nonzero "S" "S15"

  run_env "$NO_DB_DIR2" label delete "bug" --force --json
  assert_exit_nonzero "S" "S16"

  rm -rf "$NO_DB_DIR2"
}
