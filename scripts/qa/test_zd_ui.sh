#!/usr/bin/env bash
# Section ZD: TUI Command

test_zd_ui() {
  printf "Section ZD: TUI Command"

  # ZD1: Help text renders successfully.
  run tui --help
  assert_exit "ZD" "ZD1" 0
  assert_stdout_contains "ZD" "ZD1_use" "docket tui"

  # ZD2: Non-interactive mode rejects the TUI command.
  run tui
  assert_exit "ZD" "ZD2" 3
  assert_stderr_contains "ZD" "ZD2_err" "requires an interactive terminal"

  # ZD3: --json remains an explicit unsupported exception with JSON envelope.
  run tui --json
  assert_exit "ZD" "ZD3" 3
  assert_json "ZD" "ZD3_ok" ".ok" "false"
  assert_json "ZD" "ZD3_code" ".code" "VALIDATION_ERROR"
  assert_json "ZD" "ZD3_error" ".error" "--json is not supported with 'docket tui'"
}
