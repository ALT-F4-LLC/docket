#!/usr/bin/env bash
#
# ZR — THE CROSS-RUN LEDGER (DKT-2453; docs/tdd/runs-dispatch.md §4.10's
# amendment).
#
# `report executors` reads the whole window of runs and groups what became of
# each executor hint's steps and each voter name's panels. This section is the
# process-level contract: the verb answers on an empty store with zero runs
# and empty arrays rather than nulls, echoes its window, refuses a window it
# cannot parse (exit 3), and renders a human page that names its sections.
#
# THE STRANGER TEST. No workflow needs to exist for the page to be readable:
# a shop that has run nothing gets a page saying so.

test_zr_report() {
  printf "Section ZR: The cross-run ledger (DKT-2453)"

  local ZR
  ZR=$(qa_mktemp_d)

  run_env "$ZR" init --json
  assert_exit "ZR" "ZR0_init" 0

  # ZR1: the JSON contract on an empty store.
  run_env "$ZR" report executors --json
  assert_exit "ZR" "ZR1" 0
  assert_json "ZR" "ZR1_ok" ".ok" "true"
  assert_json "ZR" "ZR1_runs" ".data.runs" "0"
  assert_json "ZR" "ZR1_scope" ".data.scope" "project"
  assert_json "ZR" "ZR1_executors" ".data.executors | length" "0"
  assert_json "ZR" "ZR1_voters" ".data.voters | length" "0"
  assert_json "ZR" "ZR1_executors_array" ".data.executors | type" "array"
  assert_json "ZR" "ZR1_voters_array" ".data.voters | type" "array"

  # ZR2: the window echoes in its canonical form, for both accepted shapes.
  run_env "$ZR" report executors --json --since RUN-3
  assert_exit "ZR" "ZR2_run" 0
  assert_json "ZR" "ZR2_run_since" ".data.since" "RUN-3"
  run_env "$ZR" report executors --json --since 2026-09-01
  assert_exit "ZR" "ZR2_date" 0
  assert_json "ZR" "ZR2_date_since" ".data.since" "2026-09-01T00:00:00Z"

  # ZR3: the whole store under --all-projects.
  run_env "$ZR" report executors --json --all-projects
  assert_exit "ZR" "ZR3" 0
  assert_json "ZR" "ZR3_scope" ".data.scope" "store"

  # ZR4: a window the verb cannot parse is a VALIDATION_ERROR naming the flag.
  run_env "$ZR" report executors --json --since "last tuesday"
  assert_exit "ZR" "ZR4" 3
  assert_json "ZR" "ZR4_code" ".code" "VALIDATION_ERROR"
  # The needle starts past the flag's dashes: the assertion helper hands it
  # to grep, which would read a leading `--` as its own option.
  assert_stdout_contains "ZR" "ZR4_flag" "want a run (RUN-N)"

  # ZR5: the human page names its window and says an empty one is empty.
  run_env "$ZR" report executors
  assert_exit "ZR" "ZR5" 0
  assert_stdout_contains "ZR" "ZR5_window" "Window"
  assert_stdout_contains "ZR" "ZR5_empty" "no steps or casts in the window"

  # ZR6: --json=v2 carries the same document.
  run_env "$ZR" report executors --json=v2
  assert_exit "ZR" "ZR6" 0
  assert_json "ZR" "ZR6_ok" ".ok" "true"
  assert_json "ZR" "ZR6_runs" ".data.runs" "0"

  rm -rf "$ZR"
}
