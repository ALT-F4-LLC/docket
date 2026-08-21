#!/usr/bin/env bash
#
# ZP — `step fail --metadata` PARITY WITH `step complete --metadata`
# (docs/tdd/completion-metadata.md §1.6).
#
# An earlier fix landed `complete`'s metadata write; this section proves `fail`
# shares it. The headline assertion is the fail-side mirror of ZM's: a bag reported
# on `step fail` reaches `run report --json`'s `.data.metadata` — a step that
# routed to waiting-human rather than done, which is exactly the run an
# operator most wants tier-drift data from (a rough run, not a clean one).
#
# genericity.sh scans `internal` and `cmd` only (`SEARCH_PATHS` in that
# script) — nothing under `scripts/` is scanned. The prior ZO section's
# header claimed otherwise; this header makes no such claim (C11).
#
# THE STRANGER TEST. Same print-shop fixture as ZM: a press run that jammed is
# reported failed, and the desk that ran it is recorded via `--metadata` for
# the same "how many jobs did each desk handle" report — no AI concept in
# sight. The bag's keys are `desk` and `rework`, instance data that must never
# appear in core.

test_zp_fail_metadata() {
  printf "Section ZP: step fail --metadata parity (DKT-69)"

  local ZP ZP_WF ZP_TOKEN ZP_ROWS
  ZP=$(qa_mktemp_d)

  run_env "$ZP" init
  assert_exit "ZP" "ZP0_init" 0

  # ---------------------------------------------------------------------------
  # THE FIXTURE: one executor step with max_attempts = 1, so a single `fail`
  # exhausts it and routes — the terminal case a report most needs data from.
  # A second workflow, with max_attempts = 2, proves the survives-into-retry
  # AC (N4 below).
  # ---------------------------------------------------------------------------
  ZP_WF="$ZP/printshop-fail.toml"
  cat >"$ZP_WF" <<'TOML'
[pipeline]
name = "print-shop-fail-metadata"
version = 1
description = "Run a job on the press; record which desk handled a jam."

[match]
kind = ["task"]

[[step]]
name = "print-run"
executor = "press-operator"
emits = "sheets"
after = []
max_attempts = 1
on_fail = "waiting-human"
TOML

  run_env "$ZP" workflow register "$ZP_WF" --json
  assert_exit "ZP" "ZP0_register" 0

  # A DISTINCT issue kind (`bug`, not `task`), so this workflow's [match] never
  # collides with the fixture above's — a run's [match] must bind exactly one
  # workflow, and both fixtures declaring `kind = ["task"]` would leave DKT-4
  # ambiguous.
  ZP_WF="$ZP/printshop-retry.toml"
  cat >"$ZP_WF" <<'TOML'
[pipeline]
name = "print-shop-retry-metadata"
version = 1
description = "Run a job on the press, retryable; the desk carries a seeded tier."

[match]
kind = ["bug"]

[[step]]
name = "print-run"
executor = "press-operator"
emits = "sheets"
after = []
max_attempts = 2
on_fail = "waiting-human"
metadata = { tier = "standard" }
TOML

  run_env "$ZP" workflow register "$ZP_WF" --json
  assert_exit "ZP" "ZP0_register_retry" 0

  # ===========================================================================
  # N1 — THE HEADLINE: a failed step's bag reaches the report
  # ===========================================================================

  run_env "$ZP" issue create -t "Print the spring catalogue" -d "500 copies" --json
  assert_exit "ZP" "ZP1_issue" 0
  run_env "$ZP" run start --issue DKT-1 --json
  assert_exit "ZP" "ZP1_start" 0
  run_env "$ZP" run activate RUN-1 --json
  assert_exit "ZP" "ZP1_activate" 0

  run_env "$ZP" step claim STEP-1 --owner press-a --json
  assert_exit "ZP" "ZP1_claim" 0
  ZP_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  DOCKET_TOKEN="$ZP_TOKEN" run_env "$ZP" step fail STEP-1 \
    --note "press jammed" --metadata '{"desk":"front","rework":"true"}' --json
  assert_exit "ZP" "ZP1_fail" 0

  ZP_ROWS=$(sqlite3 "$ZP/issues.db" \
    "SELECT COUNT(*) FROM steps WHERE metadata IS NOT NULL AND metadata != '';")
  check_cond "ZP" "ZP1_column_written" "expected 1 step carrying metadata, found $ZP_ROWS" [ "$ZP_ROWS" = "1" ]

  # The step exhausted its single attempt and routed to waiting-human — the
  # metadata reached the row even though the step never completed.
  ZP_ROWS=$(sqlite3 "$ZP/issues.db" \
    "SELECT status FROM steps WHERE id = 1;")
  check_cond "ZP" "ZP1_routed_on_fail" "step status = $ZP_ROWS, want waiting-human" [ "$ZP_ROWS" = "waiting-human" ]

  run_env "$ZP" run report RUN-1 --json
  assert_exit "ZP" "ZP1_report" 0
  assert_json_exists "ZP" "ZP1_report_metadata_present" '.data.metadata'
  # Select the key by NAME, never by position: db.MetadataRollup sorts its
  # keys, so `[0]` is `desk` only because `desk` happens to sort before
  # `rework`. Adding any fixture key that sorts earlier would break this
  # section for a reason unrelated to what it tests (N4 below already uses
  # this robust form).
  assert_json "ZP" "ZP1_rollup_key" \
    '.data.metadata[] | select(.key == "desk") | .key' "desk"
  assert_json "ZP" "ZP1_rollup_value" \
    '.data.metadata[] | select(.key == "desk") | .values[0].value' "front"
  assert_json "ZP" "ZP1_rollup_count" \
    '.data.metadata[] | select(.key == "desk") | .values[0].count' "1"

  # ===========================================================================
  # N2 — A REFUSAL WRITES NOTHING AND SPENDS NO ATTEMPT
  # ===========================================================================
  #
  # §6.9's property at the CLI: validation happens BEFORE the transaction
  # opens, so a rejected failure leaves the step exactly as it was — still
  # failable on the same token, with its claim budget untouched.

  run_env "$ZP" issue create -t "Print the summer catalogue" -d "200 copies" --json
  assert_exit "ZP" "ZP2_issue" 0
  run_env "$ZP" run start --issue DKT-2 --json
  assert_exit "ZP" "ZP2_start" 0
  run_env "$ZP" run activate RUN-2 --json
  assert_exit "ZP" "ZP2_activate" 0

  run_env "$ZP" step claim STEP-2 --owner press-b --json
  assert_exit "ZP" "ZP2_claim" 0
  ZP_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  # Not JSON at all -> VALIDATION_ERROR (exit 3).
  DOCKET_TOKEN="$ZP_TOKEN" run_env "$ZP" step fail STEP-2 \
    --note "jammed" --metadata 'not json' --json
  assert_exit "ZP" "ZP2_invalid_refused" 3
  assert_json "ZP" "ZP2_invalid_code" '.code' "VALIDATION_ERROR"

  # Valid JSON, wrong shape.
  DOCKET_TOKEN="$ZP_TOKEN" run_env "$ZP" step fail STEP-2 \
    --note "jammed" --metadata '["front"]' --json
  assert_exit "ZP" "ZP2_array_refused" 3

  # The step still fails on the SAME token: the refusal cost nothing.
  DOCKET_TOKEN="$ZP_TOKEN" run_env "$ZP" step fail STEP-2 \
    --note "jammed for real" --metadata '{"desk":"back"}' --json
  assert_exit "ZP" "ZP2_fails_after_refusal" 0

  # ===========================================================================
  # N3 — THE CAP, AT THE CLI, WITH A REMEDY `fail` ACTUALLY OFFERS (C14)
  # ===========================================================================

  run_env "$ZP" issue create -t "Print the autumn catalogue" -d "100 copies" --json
  assert_exit "ZP" "ZP3_issue" 0
  run_env "$ZP" run start --issue DKT-3 --json
  assert_exit "ZP" "ZP3_start" 0
  run_env "$ZP" run activate RUN-3 --json
  assert_exit "ZP" "ZP3_activate" 0

  run_env "$ZP" step claim STEP-3 --owner press-c --json
  assert_exit "ZP" "ZP3_claim" 0
  ZP_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  local ZP_BIG
  ZP_BIG=$(printf '{"desk":"%s"}' "$(head -c 17000 /dev/zero | tr '\0' 'a')")
  DOCKET_TOKEN="$ZP_TOKEN" run_env "$ZP" step fail STEP-3 \
    --note "oversized" --metadata "$ZP_BIG" --json
  assert_exit "ZP" "ZP3_oversized_refused" 3
  assert_json "ZP" "ZP3_code" '.code' "VALIDATION_ERROR"

  assert_stdout_contains "ZP" "ZP3_names_the_cap" "16384"
  # The whole remedy phrase, not the bare word `note` — a substring that short
  # matches anywhere in the envelope, so the check would keep passing even if
  # the remedy text were replaced with something that merely mentions a note.
  assert_stdout_contains "ZP" "ZP3_names_the_remedy" \
    "record the detail in the note instead"

  # `fail` offers no --artifact-file or --payload-file — the remedy must not
  # send an operator looking for a flag this verb does not have.
  if echo "$CMD_STDOUT" "$CMD_STDERR" | grep -q -- "--artifact-file\|--payload-file"; then
    check "ZP" "ZP3_remedy_names_no_absent_flag" "FAIL" \
      "the refusal named a flag step fail does not offer"
  else
    check "ZP" "ZP3_remedy_names_no_absent_flag" "PASS"
  fi

  # ===========================================================================
  # N4 — SURVIVES INTO A RETRY, through an OPERATOR-REACHABLE seed (C10)
  # ===========================================================================
  #
  # The definition-side bag is seeded by the WORKFLOW'S OWN declared
  # `metadata = { tier = "standard" }` (activation writes it), never by a
  # hand-seeded UPDATE against the database file. max_attempts = 2 here, so
  # one failure returns the step to the pool with a real attempt left — the
  # same claims-only counting E-8 established.

  run_env "$ZP" issue create -t "Print the winter catalogue" -d "50 copies" -T bug --json
  assert_exit "ZP" "ZP4_issue" 0
  run_env "$ZP" run start --issue DKT-4 --json
  assert_exit "ZP" "ZP4_start" 0
  run_env "$ZP" run activate RUN-4 --json
  assert_exit "ZP" "ZP4_activate" 0

  run_env "$ZP" step claim STEP-4 --owner press-d --json
  assert_exit "ZP" "ZP4_claim" 0
  ZP_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  DOCKET_TOKEN="$ZP_TOKEN" run_env "$ZP" step fail STEP-4 \
    --note "first jam" --metadata '{"desk":"front"}' --json
  assert_exit "ZP" "ZP4_first_fail" 0

  ZP_ROWS=$(sqlite3 "$ZP/issues.db" "SELECT status, attempt FROM steps WHERE id = 4;")
  if [ "$ZP_ROWS" = "pending|1" ]; then
    check "ZP" "ZP4_real_retry_not_exhaustion" "PASS"
  else
    check "ZP" "ZP4_real_retry_not_exhaustion" "FAIL" \
      "status|attempt = $ZP_ROWS, want pending|1 — E-8's claims-only counting"
  fi

  # The definition's tier survived the failure's merge, and the worker's desk
  # was added — the operator-reachable proof of the merge (C10).
  ZP_ROWS=$(sqlite3 "$ZP/issues.db" "SELECT metadata FROM steps WHERE id = 4;")
  if echo "$ZP_ROWS" | grep -q '"tier":"standard"' && echo "$ZP_ROWS" | grep -q '"desk":"front"'; then
    check "ZP" "ZP4_merge_over_declared_metadata" "PASS"
  else
    check "ZP" "ZP4_merge_over_declared_metadata" "FAIL" \
      "metadata = $ZP_ROWS, want both the declared tier and the reported desk"
  fi

  # Re-claim (the retry) and complete — the eventual report carries the
  # OVERLAID bag, reachable end to end through the CLI.
  run_env "$ZP" step claim STEP-4 --owner press-d --json
  assert_exit "ZP" "ZP4_reclaim" 0
  ZP_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  printf 'the printed sheets\n' >"$ZP/sheets.txt"
  DOCKET_TOKEN="$ZP_TOKEN" run_env "$ZP" step complete STEP-4 \
    --artifact-file "$ZP/sheets.txt" --metadata '{"desk":"back"}' --json
  assert_exit "ZP" "ZP4_retry_completes" 0

  run_env "$ZP" run report RUN-4 --json
  assert_exit "ZP" "ZP4_report" 0
  assert_json "ZP" "ZP4_report_tier_key" '.data.metadata[] | select(.key == "tier") | .values[0].value' "standard"
  assert_json "ZP" "ZP4_report_desk_overlay" '.data.metadata[] | select(.key == "desk") | .values[0].value' "back"

  rm -rf "$ZP"
}
