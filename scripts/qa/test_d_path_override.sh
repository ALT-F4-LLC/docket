#!/usr/bin/env bash
# Section D: DOCKET_PATH Override

test_d_path_override() {
  printf "Section D: DOCKET_PATH Override"

  # Under TMPDIR, not a hardcoded /tmp: a TMPDIR-confined environment denies
  # the mkdir outright and the suite aborts mid-section. The path only needs
  # to be somewhere other than the default DOCKET_PATH, which is what this
  # section is checking the override reaches — /tmp was never load-bearing.
  local alt="${TMPDIR:-/tmp}/docket-qa-alt"
  rm -rf "$alt"
  mkdir -p "$alt"

  run_env "$alt" init
  assert_exit "D" "D1" 0

  run_env "$alt" config --json
  assert_exit "D" "D2" 0
  assert_stdout_contains "D" "D2" "$alt"

  rm -rf "$alt"

  check_cond "D" "D3" "directory $alt still exists after cleanup" [ ! -d "$alt" ]
}
