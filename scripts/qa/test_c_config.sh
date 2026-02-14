#!/usr/bin/env bash
# Section C: Config After Init

test_c_config() {
  printf "Section C: Config After Init\n"

  run config
  assert_exit "C" "C1" 0

  run config --json
  assert_exit "C" "C2" 0
  assert_json "C" "C2" ".data.schema_version" "1"
  assert_json "C" "C2" ".data.issue_prefix" "DKT"
}
