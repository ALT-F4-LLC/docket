#!/usr/bin/env bash
# Section D: DOCKET_PATH Override

test_d_path_override() {
  printf "Section D: DOCKET_PATH Override"
  mkdir -p /tmp/docket-qa-alt

  run_env /tmp/docket-qa-alt init
  assert_exit "D" "D1" 0

  run_env /tmp/docket-qa-alt config --json
  assert_exit "D" "D2" 0
  assert_stdout_contains "D" "D2" "/tmp/docket-qa-alt"

  rm -rf /tmp/docket-qa-alt

  if [ ! -d /tmp/docket-qa-alt ]; then
    check "D" "D3" "PASS"
  else
    check "D" "D3" "FAIL" "directory /tmp/docket-qa-alt still exists after cleanup"
  fi
}
