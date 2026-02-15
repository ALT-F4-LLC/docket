#!/usr/bin/env bash
# Section K: Reopen Command

test_k_reopen() {
  printf "Section K: Reopen Command"

  run reopen 1 --json
  assert_exit "K" "K1" 0
  assert_json "K" "K1" ".data.status" "backlog"

  run reopen 1 --json
  assert_exit "K" "K2" 0

  run reopen DKT-2 --json
  assert_exit "K" "K3" 0
  assert_json "K" "K3" ".data.status" "backlog"

  run reopen 9999 --json
  assert_exit "K" "K4" 2

  run reopen
  assert_exit_nonzero "K" "K5"

  run reopen 1
  assert_exit "K" "K6" 0
}
