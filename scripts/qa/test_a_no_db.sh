#!/usr/bin/env bash
# Section A: No-DB Commands

test_a_no_db() {
  printf "Section A: No-DB Commands"
  local NO_DB_DIR
  NO_DB_DIR=$(mktemp -d)
  mkdir -p "$NO_DB_DIR"

  run_env "$NO_DB_DIR" version
  assert_exit "A" "A1" 0

  run_env "$NO_DB_DIR" version --json
  assert_exit "A" "A2" 0
  assert_json "A" "A2" ".ok" "true"
  assert_json_exists "A" "A2" ".data.version"

  run_env "$NO_DB_DIR" --help
  assert_exit "A" "A3" 0

  run_env "$NO_DB_DIR" config
  assert_exit "A" "A4" 0

  run_env "$NO_DB_DIR" config --json
  assert_exit "A" "A5" 0
  assert_json "A" "A5" ".ok" "true"
  assert_json "A" "A5" ".data.db_size_bytes" "0"
  assert_json "A" "A5" ".data.schema_version" "0"

  rm -rf "$NO_DB_DIR"
}
