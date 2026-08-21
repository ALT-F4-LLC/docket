#!/usr/bin/env bash
# Section C: Config After Init

test_c_config() {
  printf "Section C: Config After Init"

  run config
  assert_exit "C" "C1" 0

  run config --json
  assert_exit "C" "C2" 0
  # schema_version tracks db.currentSchemaVersion — assert it is a positive
  # integer rather than a literal, so schema bumps don't break this check.
  # (It was pinned to "1" and silently stale from v2 onward.)
  assert_json "C" "C2" '(.data.schema_version | type == "number" and . >= 1)' "true"
  assert_json "C" "C2" ".data.issue_prefix" "DKT"
}
