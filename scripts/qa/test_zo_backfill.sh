#!/usr/bin/env bash
#
# ZO — THE USAGE BACK-FILL RETIRES THE D2 WEDGE
# (docs/tdd/usage-backfill-wedge.md).
#
# THIS SECTION IS THE INVERSE OF RUN-3'S RUN-ENDING WEDGE. That run recorded,
# through the CLI:
#
#   $ docket step complete STEP-N --artifact-file art.md     # no --usage
#   ✔ Completed STEP-N
#   $ docket dispatch close --run RUN-N --accept-missing-usage
#   ✔ DISPATCH-N is closed (accepted-missing-usage)
#   $ docket next --run RUN-N
#   ✘ Error: ... usage-rows-missing ...                      # forever
#
# Acceptance let the CLOSE succeed — its real contract — and could not clear the
# discrepancy, because D2 is computed from `steps.usage_recorded` and never
# reads `close_reason`. Every later `next` recomputed the same refusal from the
# same column, so no step was ever offered again and the run's builtin action
# steps (driven only from `next`) starved. RUN-3 ended there.
#
# THE STATE THAT WAS MISSING WAS USAGE, NOT ACCEPTANCE. So the headline
# assertion is a `next` that ANSWERS after a back-fill — the exact call that
# refused forever, through the same verbs, at the same layer. The Go tests prove
# the transaction; this proves the WIRE.
#
# THE STRANGER TEST (§1.1). The fixture is a print shop, as ZM's is: a press
# operator runs a job, and the shop's own dispatcher — not the operator — knows
# how many sheets were consumed, because the operator cannot see the counter.
# That is exactly the shape the back-fill exists for. Units here are `sheets`,
# and scripts/qa/genericity.sh scans this file like any other.

test_zo_backfill() {
  printf "Section ZO: Usage back-fill (DKT-77)"

  local ZO ZO_WF ZO_TOKEN ZO_ROWS ZO_SOURCE
  ZO=$(qa_mktemp_d)

  run_env "$ZO" init
  assert_exit "ZO" "ZO0_init" 0

  # ---------------------------------------------------------------------------
  # THE FIXTURE: one executor step, which is all the wire proof needs.
  # ---------------------------------------------------------------------------
  ZO_WF="$ZO/printshop.toml"
  cat >"$ZO_WF" <<'TOML'
[pipeline]
name = "print-shop-backfill"
version = 1
description = "Run a job on the press; the shop counts the sheets afterward."

[match]
kind = ["task"]

[[step]]
name = "print-run"
executor = "press-operator"
emits = "sheets"
after = []
TOML

  run_env "$ZO" workflow register "$ZO_WF" --json
  assert_exit "ZO" "ZO0_register" 0

  run_env "$ZO" issue create -t "Print the autumn catalogue" -d "300 copies" --json
  assert_exit "ZO" "ZO0_issue" 0
  run_env "$ZO" run start --issue DKT-1 --json
  assert_exit "ZO" "ZO0_start" 0
  run_env "$ZO" run activate RUN-1 --json
  assert_exit "ZO" "ZO0_activate" 0

  printf 'the printed sheets\n' >"$ZO/sheets.txt"

  # ===========================================================================
  # O1 — THE WEDGE, RETIRED: a dispatched run, completed with no usage and
  # closed under acceptance, does NOT refuse (DKT-315).
  # ===========================================================================
  #
  # D2 applies only to runs that ever dispatched, so the manifest is what makes
  # the relay accountable for the step's usage.

  run_env "$ZO" dispatch open --run RUN-1 --json
  assert_exit "ZO" "ZO1_dispatch_open" 0

  run_env "$ZO" step claim STEP-1 --owner press-a --json
  assert_exit "ZO" "ZO1_claim" 0
  ZO_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  # No --usage: the press operator cannot read the shop's sheet counter.
  DOCKET_TOKEN="$ZO_TOKEN" run_env "$ZO" step complete STEP-1 \
    --artifact-file "$ZO/sheets.txt" --json
  assert_exit "ZO" "ZO1_complete_without_usage" 0

  # The ledger is empty and the fast path the probe reads is unset.
  ZO_ROWS=$(sqlite3 "$ZO/issues.db" "SELECT COUNT(*) FROM usage_ledger;")
  check_cond "ZO" "ZO1_ledger_empty" "expected an empty ledger, found $ZO_ROWS row(s)" [ "$ZO_ROWS" = "0" ]

  # The close SUCCEEDS under acceptance — that is acceptance's real contract.
  run_env "$ZO" dispatch close --run RUN-1 --accept-missing-usage --json
  assert_exit "ZO" "ZO1_close_accepts" 0

  # E-4, AT THE WIRE: DKT-315 made `--accept-missing-usage` SETTLE the accepted
  # steps (writes `usage_recorded`, the same fast path D2 probes) rather than
  # only recording the acceptance in the close event — so `next` answers
  # immediately, with no back-fill required. This is the assertion that would
  # have caught RUN-3/RUN-14/RUN-32's wedge before the fix; it now proves the
  # wedge is gone at the wire, not just in the Go transaction tests.
  run_env "$ZO" next --run RUN-1 --json
  assert_exit "ZO" "ZO1_next_answers_after_acceptance" 0

  # Settling is not reporting: the ledger stays empty until an explicit
  # back-fill writes real numbers (O2, below).
  ZO_ROWS=$(sqlite3 "$ZO/issues.db" "SELECT COUNT(*) FROM usage_ledger;")
  check_cond "ZO" "ZO1_ledger_still_empty_after_settle" \
    "expected an empty ledger, found $ZO_ROWS row(s)" [ "$ZO_ROWS" = "0" ]

  # ===========================================================================
  # O2 — THE HEADLINE: the back-fill records the usage, and `next` answers
  # ===========================================================================

  run_env "$ZO" dispatch backfill-usage --run RUN-1 \
    --step STEP-1 --unit sheets --quantity 300 --json
  assert_exit "ZO" "ZO2_backfill" 0

  ZO_ROWS=$(sqlite3 "$ZO/issues.db" "SELECT COUNT(*) FROM usage_ledger;")
  check_cond "ZO" "ZO2_ledger_written" "expected 1 ledger row, found $ZO_ROWS" [ "$ZO_ROWS" = "1" ]

  # §1.3.2: the source is written EXPLICITLY, so a relay's reconstruction is
  # never mistaken for the claimant's own report. An empty source would have
  # fallen through InsertUsageRowTx's default to "reported".
  ZO_SOURCE=$(sqlite3 "$ZO/issues.db" "SELECT source FROM usage_ledger LIMIT 1;")
  check_cond "ZO" "ZO2_source_is_explicit" "source is '$ZO_SOURCE', want 'backfilled' — 'reported' means a claimant said so" [ "$ZO_SOURCE" = "backfilled" ]

  # §1.3.1: the row lands on the step's RECORDED attempt (one claim ⇒ 1).
  ZO_ROWS=$(sqlite3 "$ZO/issues.db" "SELECT attempt FROM usage_ledger LIMIT 1;")
  check_cond "ZO" "ZO2_recorded_attempt" "attempt is $ZO_ROWS, want 1" [ "$ZO_ROWS" = "1" ]

  # THE ASSERTION THIS SECTION EXISTS FOR: `next` no longer refuses.
  run_env "$ZO" next --run RUN-1 --json
  assert_exit "ZO" "ZO2_next_answers" 0

  # And the usage reaches the run report, where the budget reads it. This is
  # the number that was stranded journal-side for every step of RUN-3.
  run_env "$ZO" run report RUN-1 --json
  assert_exit "ZO" "ZO2_report" 0
  assert_json "ZO" "ZO2_report_unit" '.data.budget.reported[0].unit' "sheets"
  assert_json "ZO" "ZO2_report_quantity" '.data.budget.reported[0].quantity' "300"

  # ===========================================================================
  # O3 — A DOUBLE BACK-FILL IS REFUSED, in words rather than SQLite's
  # ===========================================================================
  #
  # The ledger's (step, attempt, unit) key is asserted rather than worked
  # around: a back-fill ADDS to the ledger and never overwrites it, so silently
  # merging would hide a double-count of real spend.

  run_env "$ZO" dispatch backfill-usage --run RUN-1 \
    --step STEP-1 --unit sheets --quantity 300
  assert_exit "ZO" "ZO3_double_backfill_refused" 4
  assert_stderr_contains "ZO" "ZO3_refusal_is_phrased" "already has"
  # The refusal must NOT leak SQLite's own text — §2's rule is that a refusal
  # names its way out, and "UNIQUE constraint failed" names a table instead.
  if echo "$CMD_STDERR" | grep -q "UNIQUE constraint"; then
    check "ZO" "ZO3_refusal_hides_sqlite" "FAIL" \
      "the refusal leaks raw SQLite text: $CMD_STDERR"
  else
    check "ZO" "ZO3_refusal_hides_sqlite" "PASS"
  fi

  # A SECOND UNIT on the same step is not a double back-fill — it is another
  # measurement, and it lands beside the first.
  run_env "$ZO" dispatch backfill-usage --run RUN-1 \
    --step STEP-1 --unit minutes --quantity 12 --json
  assert_exit "ZO" "ZO3_second_unit_lands" 0

  # ===========================================================================
  # O4 — THE BATCH FORM, and the all-or-nothing rule
  # ===========================================================================
  #
  # A relay with N spawns uses one invocation, not N process launches.

  run_env "$ZO" issue create -t "Print the winter catalogue" -d "150 copies" --json
  assert_exit "ZO" "ZO4_issue" 0
  run_env "$ZO" run start --issue DKT-2 --json
  assert_exit "ZO" "ZO4_start" 0
  run_env "$ZO" run activate RUN-2 --json
  assert_exit "ZO" "ZO4_activate" 0
  run_env "$ZO" dispatch open --run RUN-2 --json
  assert_exit "ZO" "ZO4_dispatch_open" 0

  run_env "$ZO" step claim STEP-2 --owner press-b --json
  assert_exit "ZO" "ZO4_claim" 0
  ZO_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')
  DOCKET_TOKEN="$ZO_TOKEN" run_env "$ZO" step complete STEP-2 \
    --artifact-file "$ZO/sheets.txt" --json
  assert_exit "ZO" "ZO4_complete" 0

  # A batch whose SECOND row names a step of another run writes NOTHING — the
  # whole back-fill is one transaction.
  printf '[{"step":"STEP-2","unit":"sheets","quantity":150},{"step":"STEP-1","unit":"reams","quantity":3}]' \
    >"$ZO/batch-bad.json"
  run_env "$ZO" dispatch backfill-usage --run RUN-2 --from-json "$ZO/batch-bad.json"
  assert_exit "ZO" "ZO4_cross_run_batch_refused" 3

  ZO_ROWS=$(sqlite3 "$ZO/issues.db" "SELECT COUNT(*) FROM usage_ledger WHERE step_id = 2;")
  check_cond "ZO" "ZO4_refused_batch_wrote_nothing" "the good row survived a refused batch: $ZO_ROWS row(s) for STEP-2" [ "$ZO_ROWS" = "0" ]

  # The same batch, corrected, applies whole.
  printf '[{"step":"STEP-2","unit":"sheets","quantity":150}]' >"$ZO/batch-good.json"
  run_env "$ZO" dispatch backfill-usage --run RUN-2 --from-json "$ZO/batch-good.json" --json
  assert_exit "ZO" "ZO4_batch_applies" 0

  # And the two forms are mutually exclusive: one batch, one source of truth.
  run_env "$ZO" dispatch backfill-usage --run RUN-2 \
    --from-json "$ZO/batch-good.json" --step STEP-2 --unit sheets --quantity 1
  assert_exit "ZO" "ZO4_both_forms_refused" 3

  # ===========================================================================
  # O5 — --source is carried verbatim; core enumerates no sources
  # ===========================================================================

  run_env "$ZO" dispatch backfill-usage --run RUN-2 \
    --step STEP-2 --unit minutes --quantity 20 --source "shop-floor-tally" --json
  assert_exit "ZO" "ZO5_source_override" 0

  ZO_SOURCE=$(sqlite3 "$ZO/issues.db" \
    "SELECT source FROM usage_ledger WHERE unit = 'minutes' AND step_id = 2;")
  check_cond "ZO" "ZO5_source_verbatim" "source is '$ZO_SOURCE', want the string the operator passed" [ "$ZO_SOURCE" = "shop-floor-tally" ]

  rm -rf "$ZO"
}
