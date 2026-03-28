#!/usr/bin/env bash
# Section ZD: UI Command

test_zd_ui() {
  printf "Section ZD: UI Command"

  # ZD1: Help text renders successfully.
  run ui --help
  assert_exit "ZD" "ZD1" 0
  assert_stdout_contains "ZD" "ZD1_use" "docket ui"

  # ZD2: Non-interactive mode rejects the TUI command.
  run ui
  assert_exit "ZD" "ZD2" 3
  assert_stderr_contains "ZD" "ZD2_err" "requires an interactive terminal"

  # ZD3: --json remains an explicit unsupported exception with JSON envelope.
  run ui --json
  assert_exit "ZD" "ZD3" 3
  assert_json "ZD" "ZD3_ok" ".ok" "false"
  assert_json "ZD" "ZD3_code" ".code" "VALIDATION_ERROR"
  assert_json "ZD" "ZD3_error" ".error" "--json is not supported with 'docket ui'"
}
