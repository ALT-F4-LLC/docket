#!/usr/bin/env bash
#
# ZQ — THE CONDUCTOR CAPABILITY (DKT-2465; docs/tdd/claims-leases.md §4.0,
# docs/tdd/reliability-delta.md §2's v29 amendment).
#
# The seven operator verbs — step approve, step reject, step resolve, step
# reap, run pause, run resume, run abandon — used to be token-free: "the
# authority is repository access". This section is the process-level proof of
# the new posture, the integration test the issue's acceptance names: a run's
# first activation mints the capability and returns it ONCE; a caller without
# it is refused on every verb (R9: none supplied, exit 3; R10: wrong, exit 5)
# and writes nothing; the holder is not; `run conduct` re-mints it and retires
# the standing one; and a terminal run refuses the seat (R12, exit 4).
#
# THE STRANGER TEST. The fixture is a print shop again: a press run, then a
# proof-reader's sign-off (a `type="human"` gate). The desk that activated the
# job holds its ticket; anyone else who wants to sign off must ask for the
# ticket, and the ledger says who asked.

test_zq_conductor() {
  printf "Section ZQ: The conductor capability (DKT-2465)"

  local ZQ ZQ_WF ZQ_TOKEN ZQ_TOKEN2 ZQ_STEP_TOKEN
  ZQ=$(qa_mktemp_d)

  run_env "$ZQ" init
  assert_exit "ZQ" "ZQ0_init" 0

  ZQ_WF="$ZQ/printshop-signoff.toml"
  cat >"$ZQ_WF" <<'TOML'
[pipeline]
name = "print-shop-signoff"
version = 1
description = "Run a job on the press, then a proof-reader signs it off."

[match]
kind = ["task"]

[[step]]
name = "print-run"
executor = "press-operator"
emits = "sheets"
after = []

[[step]]
name = "sign-off"
type = "human"
after = ["print-run"]
on_fail = "skip"
TOML

  run_env "$ZQ" workflow register "$ZQ_WF" --json
  assert_exit "ZQ" "ZQ0_register" 0

  run_env "$ZQ" issue create -t "Print the spring catalogue" --json
  assert_exit "ZQ" "ZQ0_issue" 0
  run_env "$ZQ" run start --issue DKT-1 --json
  assert_exit "ZQ" "ZQ0_start" 0

  # ---------------------------------------------------------------------------
  # ZQ1: the first activation mints the capability and returns it once, as
  # `conductor_token` — 32 bytes of crypto/rand, hex encoded, like every
  # capability token. A dry run mints nothing.
  # ---------------------------------------------------------------------------
  run_env "$ZQ" run activate RUN-1 --dry-run --json=v2
  assert_exit "ZQ" "ZQ1_dry_run" 0
  assert_json_null "ZQ" "ZQ1_dry_run_mints_nothing" '.data.conductor_token'

  run_env "$ZQ" run activate RUN-1 --json=v2
  assert_exit "ZQ" "ZQ1_activate" 0
  assert_json_exists "ZQ" "ZQ1_token_present" '.data.conductor_token'
  ZQ_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.conductor_token')
  check_cond "ZQ" "ZQ1_token_length" "token length ${#ZQ_TOKEN}, want 64 hex chars" [ "${#ZQ_TOKEN}" -eq 64 ]

  # The store keeps the hash, never the token.
  local ZQ_LEAK
  ZQ_LEAK=$(sqlite3 "$ZQ/issues.db" "SELECT COUNT(*) FROM runs WHERE conductor_token_hash = '$ZQ_TOKEN'")
  check_cond "ZQ" "ZQ1_hash_only" "the token itself is stored on the run row" [ "$ZQ_LEAK" -eq 0 ]

  # ---------------------------------------------------------------------------
  # ZQ2: the press run completes under its own lease token — an executor's
  # path is untouched by the capability — and the sign-off gate is ready.
  # ---------------------------------------------------------------------------
  run_env "$ZQ" step claim STEP-1 --owner press --json
  assert_exit "ZQ" "ZQ2_claim" 0
  ZQ_STEP_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  printf 'the printed sheets\n' >"$ZQ/sheets.txt"
  DOCKET_TOKEN="$ZQ_STEP_TOKEN" run_env "$ZQ" step complete STEP-1 \
    --artifact-file "$ZQ/sheets.txt" --json
  assert_exit "ZQ" "ZQ2_complete" 0

  run_env "$ZQ" step show STEP-2 --json=v2
  assert_exit "ZQ" "ZQ2_gate_show" 0
  assert_json "ZQ" "ZQ2_gate_ready" '.data.status' "ready"

  # ---------------------------------------------------------------------------
  # ZQ3: THE REFUSALS. An executor sharing the checkout — or anyone else
  # without the ticket — is refused on every verb, before anything is
  # written. R9: no token at all is VALIDATION_ERROR naming both channels and
  # the recovery verb. R10: a wrong token is AUTH_ERROR, and the refusal never
  # echoes what was presented.
  # ---------------------------------------------------------------------------
  local ZQ_V_BEFORE ZQ_V_AFTER
  run_env "$ZQ" run status RUN-1 --json=v2
  ZQ_V_BEFORE=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.run.row_version')

  run_env "$ZQ" step approve STEP-2 --json </dev/null
  assert_exit "ZQ" "ZQ3_approve_no_token" 3
  assert_json "ZQ" "ZQ3_approve_no_token_code" '.code' "VALIDATION_ERROR"
  assert_stdout_contains "ZQ" "ZQ3_names_env" "DOCKET_TOKEN"
  assert_stdout_contains "ZQ" "ZQ3_names_recovery" "run conduct"

  DOCKET_TOKEN=deadbeef run_env "$ZQ" step approve STEP-2 --json
  assert_exit "ZQ" "ZQ3_approve_wrong_token" 5
  assert_json "ZQ" "ZQ3_approve_wrong_code" '.code' "AUTH_ERROR"
  if printf '%s' "$CMD_STDOUT" | grep -q "deadbeef"; then
    check "ZQ" "ZQ3_no_echo" "FAIL" "the refusal echoes the presented token"
  else
    check "ZQ" "ZQ3_no_echo" "PASS"
  fi

  DOCKET_TOKEN=deadbeef run_env "$ZQ" step reject STEP-2 --json
  assert_exit "ZQ" "ZQ3_reject_wrong_token" 5
  DOCKET_TOKEN=deadbeef run_env "$ZQ" step resolve STEP-2 --as skip --json
  assert_exit "ZQ" "ZQ3_resolve_wrong_token" 5
  DOCKET_TOKEN=deadbeef run_env "$ZQ" step reap STEP-1 --reason "gone" --json
  assert_exit "ZQ" "ZQ3_reap_wrong_token" 5
  DOCKET_TOKEN=deadbeef run_env "$ZQ" run pause RUN-1 --json
  assert_exit "ZQ" "ZQ3_pause_wrong_token" 5
  run_env "$ZQ" run pause RUN-1 --json </dev/null
  assert_exit "ZQ" "ZQ3_pause_no_token" 3
  DOCKET_TOKEN=deadbeef run_env "$ZQ" run resume RUN-1 --json
  assert_exit "ZQ" "ZQ3_resume_wrong_token" 5
  DOCKET_TOKEN=deadbeef run_env "$ZQ" run abandon RUN-1 --reason "no" --json
  assert_exit "ZQ" "ZQ3_abandon_wrong_token" 5
  DOCKET_TOKEN=deadbeef run_env "$ZQ" run abandon RUN-1 --issue DKT-1 --reason "no" --json
  assert_exit "ZQ" "ZQ3_abandon_issue_wrong_token" 5

  # Nothing moved: the gate is still ready, the run still active and at the
  # same version.
  run_env "$ZQ" step show STEP-2 --json=v2
  assert_json "ZQ" "ZQ3_gate_untouched" '.data.status' "ready"
  run_env "$ZQ" run status RUN-1 --json=v2
  assert_json "ZQ" "ZQ3_run_untouched" '.data.run.status' "active"
  ZQ_V_AFTER=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.run.row_version')
  check_cond "ZQ" "ZQ3_version_unchanged" "run version moved $ZQ_V_BEFORE -> $ZQ_V_AFTER under refusals" \
    [ "$ZQ_V_BEFORE" = "$ZQ_V_AFTER" ]

  # ---------------------------------------------------------------------------
  # ZQ4: THE HOLDER IS NOT REFUSED. Pause and resume under the ticket.
  # ---------------------------------------------------------------------------
  DOCKET_TOKEN="$ZQ_TOKEN" run_env "$ZQ" run pause RUN-1 --reason "lunch" --json=v2
  assert_exit "ZQ" "ZQ4_pause" 0
  assert_json "ZQ" "ZQ4_paused" '.data.status' "waiting-human"
  DOCKET_TOKEN="$ZQ_TOKEN" run_env "$ZQ" run resume RUN-1 --json=v2
  assert_exit "ZQ" "ZQ4_resume" 0
  assert_json "ZQ" "ZQ4_resumed" '.data.status' "active"

  # ---------------------------------------------------------------------------
  # ZQ5: `run conduct` re-mints. The new ticket works, the old one is retired
  # (AUTH_ERROR, so the displaced holder learns at once), and the ledger names
  # who took the seat and that a standing ticket was rotated.
  # ---------------------------------------------------------------------------
  run_env "$ZQ" run conduct RUN-1 --json=v2
  assert_exit "ZQ" "ZQ5_conduct" 0
  assert_json "ZQ" "ZQ5_rotated" '.data.rotated' "true"
  ZQ_TOKEN2=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  check_cond "ZQ" "ZQ5_fresh_token" "conduct returned the standing token" [ "$ZQ_TOKEN2" != "$ZQ_TOKEN" ]

  DOCKET_TOKEN="$ZQ_TOKEN" run_env "$ZQ" step approve STEP-2 --json
  assert_exit "ZQ" "ZQ5_old_token_retired" 5
  assert_json "ZQ" "ZQ5_old_token_code" '.code' "AUTH_ERROR"

  run_env "$ZQ" events list --json=v2
  assert_exit "ZQ" "ZQ5_events" 0
  assert_json "ZQ" "ZQ5_seated_event" \
    '[.data.items[] | select(.kind == "conductor-seated")] | length' "1"
  assert_json_exists "ZQ" "ZQ5_seated_actor" \
    '[.data.items[] | select(.kind == "conductor-seated")][0].data.actor'
  # A non-empty actor: the seat names WHO took it, whatever git identity this
  # machine reports (the resolver's last fallback is the literal "unknown",
  # which still counts as named — an empty string never appears).
  ZQ_SEAT_ACTOR=$(printf '%s' "$CMD_STDOUT" | jq -r '[.data.items[] | select(.kind == "conductor-seated")][0].data.actor')
  check_cond "ZQ" "ZQ5_seated_actor_named" "the conductor-seated actor is empty" [ -n "$ZQ_SEAT_ACTOR" ]
  assert_json "ZQ" "ZQ5_seated_rotated" \
    '[.data.items[] | select(.kind == "conductor-seated")][0].data.rotated' "true"
  assert_json_exists "ZQ" "ZQ5_seated_names_cwd" \
    '[.data.items[] | select(.kind == "conductor-seated")][0].data.cwd'

  # Human mode prints the token on its own line, never inside the message.
  run_env "$ZQ" run conduct RUN-1
  assert_exit "ZQ" "ZQ5_conduct_human" 0
  local ZQ_LAST
  ZQ_LAST=$(printf '%s' "$CMD_STDOUT" | tail -n 1)
  check_cond "ZQ" "ZQ5_human_own_line" "last stdout line is not a bare 64-char token: $ZQ_LAST" \
    [ "${#ZQ_LAST}" -eq 64 ]
  ZQ_TOKEN2="$ZQ_LAST"

  # ---------------------------------------------------------------------------
  # ZQ6: the sign-off lands under the current ticket via STDIN — the other
  # accepted channel — which reconciles this single-gate run to done. A
  # terminal run then refuses the seat (R12).
  # ---------------------------------------------------------------------------
  DOCKET_PATH="$ZQ" run_stdin "$ZQ_TOKEN2" step approve STEP-2 --note "looks right" --json=v2
  assert_exit "ZQ" "ZQ6_approve_via_stdin" 0
  run_env "$ZQ" step show STEP-2 --json=v2
  assert_json "ZQ" "ZQ6_gate_done" '.data.status' "done"

  run_env "$ZQ" run status RUN-1 --json=v2
  assert_json "ZQ" "ZQ6_run_done" '.data.run.status' "done"

  run_env "$ZQ" run conduct RUN-1 --json=v2
  assert_exit "ZQ" "ZQ6_conduct_terminal" 4
  assert_json "ZQ" "ZQ6_conduct_terminal_code" '.code' "CONFLICT"

  # ---------------------------------------------------------------------------
  # ZQ7: R11 — a run activated before the capability existed asks for nothing.
  # Simulated by clearing the hash, which is exactly what a migrated pre-v29
  # row holds; the verbs behave on it as they always did.
  # ---------------------------------------------------------------------------
  run_env "$ZQ" issue create -t "Print the summer catalogue" --json
  run_env "$ZQ" run start --issue DKT-2 --json
  run_env "$ZQ" run activate RUN-2 --json=v2
  assert_exit "ZQ" "ZQ7_activate" 0
  sqlite3 "$ZQ/issues.db" "UPDATE runs SET conductor_token_hash = NULL WHERE id = 2"

  run_env "$ZQ" run pause RUN-2 --reason "legacy" --json=v2 </dev/null
  assert_exit "ZQ" "ZQ7_legacy_pause_allowed" 0
  assert_json "ZQ" "ZQ7_legacy_paused" '.data.status' "waiting-human"

  # And conducting it binds it: from here the verbs ask for the ticket.
  run_env "$ZQ" run conduct RUN-2 --json=v2
  assert_exit "ZQ" "ZQ7_conduct_legacy" 0
  assert_json "ZQ" "ZQ7_legacy_not_rotated" '.data.rotated' "false"
  run_env "$ZQ" run resume RUN-2 --json </dev/null
  assert_exit "ZQ" "ZQ7_bound_now_refuses" 3

  rm -rf "$ZQ"
}
