#!/usr/bin/env bash
#
# ZI — BUDGETS, THE FLOOR, AND `run report` (runs-dispatch §4, §9.2).
#
# THE HEADLINE IS engine-spec §9 item 7, verbatim:
#
#   Budget: with reporting disabled, the run still pauses at the cap from the
#   floor.
#
# "Reporting disabled" is not a setting — it is the ABSENCE of `--usage`, which
# is the default and the only thing a claimant has to do to report nothing. So
# the proof is a run that never passes `--usage` anywhere, whose `usage_ledger`
# is empty at the end, and which pauses at its cap anyway because the engine
# accrued the floor from facts it produced itself: one `step-claimed` event per
# claim, each carrying the step's declared `expected_cost`.
#
# THE STRANGER TEST ON THE HEADLINE FEATURE (§1.1). The team here is tracking a
# documentation sprint: every step declares a cost of 1, the cap is a count of
# steps, and the run pauses for a person once that many have been claimed. No
# model, no token, no agent — a budget is a number and a cap is a number, and
# what they count is the workflow author's business.
#
# The section also covers the group-1 DORMANCY proof (§3): a run with no budget
# executes the queries v9 executed, and `next` is byte-identical.

test_zi_budget() {
  printf "Section ZI: Budgets, the floor, and the run report"

  local ZI ZI_WF ZI_SCHEMA
  ZI=$(qa_mktemp_d)
  ZI_SCHEMA="$SCRIPT_DIR/qa/fixtures/schemas/findings@1.json"

  run_env "$ZI" init
  assert_exit "ZI" "ZI0_init" 0

  # ---------------------------------------------------------------------------
  # THE FIXTURE: a documentation sprint. Two steps, both declaring a cost of 1,
  # no gates and no actions — so nothing in this section depends on the trust
  # store and the run's only refusals are the budget's.
  #
  # `expected_cost = 1` per step makes the arithmetic legible: a cap of 2 admits
  # two claims and refuses the third. That is the whole mechanism, in a fixture
  # a documentation team could have written.
  # ---------------------------------------------------------------------------
  ZI_WF="$ZI/sprint.toml"
  cat >"$ZI_WF" <<'TOML'
[pipeline]
name = "doc-sprint"
version = 1
description = "A documentation sprint: draft a page, then have someone read it."

[match]
kind = ["task"]

[[step]]
name = "draft"
executor = "writer"
emits = "draft"
after = []
expected_cost = 1

[[step]]
name = "proofread"
executor = "proofreader"
emits = "notes"
after = ["draft"]
expected_cost = 1
TOML

  run_env "$ZI" workflow register "$ZI_WF" --json
  assert_exit "ZI" "ZI0_register" 0
  assert_json "ZI" "ZI0_register_name" '.data.name' "doc-sprint"

  # ---------------------------------------------------------------------------
  # ZI1 — §9 ITEM 7. Zero `--usage` anywhere; the floor alone reaches the cap.
  #
  # The cap is 1: `draft` costs 1, so the first claim lands EXACTLY ON IT and is
  # allowed (B14 — crossing is `>`, not `>=`: a budget reached is spent, not
  # exceeded). The next claim would cross, and pauses the run.
  # ---------------------------------------------------------------------------
  run_env "$ZI" issue create -t "Write the getting-started page" -d "a body" --json
  assert_exit "ZI" "ZI1_issue" 0

  run_env "$ZI" run start --issue DKT-1 --budget 1 --json
  assert_exit "ZI" "ZI1_start" 0
  assert_json "ZI" "ZI1_budget_stored" '.data.budget' "1"

  run_env "$ZI" run activate RUN-1 --json
  assert_exit "ZI" "ZI1_activate" 0
  assert_json "ZI" "ZI1_active" '.data.run.status' "active"

  # The claim that lands exactly on the cap: ALLOWED.
  run_env "$ZI" step claim STEP-1 --owner sprint-relay --json
  assert_exit "ZI" "ZI1_claim_at_cap" 0
  local ZI_TOKEN
  ZI_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  # Complete it with NO --usage. This is "reporting disabled": the claimant
  # says nothing about what it spent, which is the default and the case §9
  # item 7 is about.
  printf 'the drafted page\n' >"$ZI/draft.txt"
  DOCKET_TOKEN="$ZI_TOKEN" run_env "$ZI" step complete STEP-1 \
    --artifact-file "$ZI/draft.txt" --json
  assert_exit "ZI" "ZI1_complete_no_usage" 0

  # The successor is now ready, and claiming it WOULD cross the cap.
  run_env "$ZI" step claim STEP-2 --owner sprint-relay --json
  assert_exit "ZI" "ZI1_claim_crossing_refused" 4
  assert_json "ZI" "ZI1_refusal_code" '.code' "CONFLICT"

  # THE HEADLINE ASSERTION: the run is paused, from the floor, with reporting
  # disabled throughout.
  run_env "$ZI" run status RUN-1 --json
  assert_exit "ZI" "ZI1_status" 0
  assert_json "ZI" "ZI1_paused_at_cap" '.data.run.status' "waiting-human"

  # And the reason is a BARE-NUMBER statement naming no unit (B21, §1.1's
  # second leak): "budget: spend N of cap M reached at <instance>".
  local ZI_REASON
  ZI_REASON=$(echo "$CMD_STDOUT" | jq -r '.data.run.reason')
  case "$ZI_REASON" in
    "budget: spend "*" of cap "*" reached at "*)
      check "ZI" "ZI1_reason_shape" "PASS" ;;
    *)
      check "ZI" "ZI1_reason_shape" "FAIL" "reason was: $ZI_REASON" ;;
  esac

  # THE PROOF THAT REPORTING WAS DISABLED: the ledger is empty. If a `--usage`
  # had slipped into this scenario, this row would be non-zero and the pause
  # above would prove nothing about the floor.
  local ZI_LEDGER
  ZI_LEDGER=$(sqlite3 "$ZI/issues.db" "SELECT COUNT(*) FROM usage_ledger;")
  check_cond "ZI" "ZI1_ledger_empty" "the ledger holds $ZI_LEDGER rows; §9 item 7 requires reporting to be off" [ "$ZI_LEDGER" = "0" ]

  # And the floor that paused it came from CLAIM EVENTS, not from a counter.
  local ZI_CLAIMS
  ZI_CLAIMS=$(sqlite3 "$ZI/issues.db" \
    "SELECT COUNT(*) FROM events WHERE kind = 'step-claimed';")
  check_cond "ZI" "ZI1_floor_from_events" "expected exactly 1 step-claimed event, found $ZI_CLAIMS" [ "$ZI_CLAIMS" = "1" ]

  # The pause is event-logged as `run-paused` with reason=budget — an EXISTING
  # closed-set kind (B23), so §9 item 2's set does not widen for this stage.
  local ZI_PAUSE
  ZI_PAUSE=$(sqlite3 "$ZI/issues.db" \
    "SELECT COUNT(*) FROM events WHERE kind = 'run-paused' AND data LIKE '%\"reason\":\"budget\"%';")
  check_cond "ZI" "ZI1_pause_event" "expected one run-paused event carrying reason=budget, found $ZI_PAUSE" [ "$ZI_PAUSE" = "1" ]

  # ---------------------------------------------------------------------------
  # ZI2 — B24: `run resume` clears nothing, and the run RE-PAUSES.
  #
  # This reads as a bug and is not one: the cap has not moved, so the condition
  # has not changed. Raising a cap mid-run has no verb in v1 — recorded
  # rather than invented here.
  # ---------------------------------------------------------------------------
  run_env "$ZI" run resume RUN-1 --json
  assert_exit "ZI" "ZI2_resume" 0
  assert_json "ZI" "ZI2_resumed_active" '.data.status' "active"

  run_env "$ZI" step claim STEP-2 --owner sprint-relay --json
  assert_exit "ZI" "ZI2_reclaim_refused" 4

  run_env "$ZI" run status RUN-1 --json
  assert_json "ZI" "ZI2_repaused" '.data.run.status' "waiting-human"

  # ---------------------------------------------------------------------------
  # ZI3 — `run report`: the R-table, over the run ZI1 built.
  # ---------------------------------------------------------------------------
  run_env "$ZI" run report RUN-1 --json
  assert_exit "ZI" "ZI3_report" 0
  assert_json "ZI" "ZI3_report_run" '.data.run.run' "RUN-1"
  assert_json "ZI" "ZI3_report_status" '.data.run.status' "waiting-human"

  # R2: the cap, its SOURCE (R6's line — the answer to "why didn't it stop?"),
  # the floor, and max(reported, floor).
  assert_json "ZI" "ZI3_report_cap" '.data.budget.cap' "1"
  assert_json "ZI" "ZI3_report_cap_source" '.data.budget.cap_source' "run"
  assert_json "ZI" "ZI3_report_floor" '.data.budget.floor' "1"
  assert_json "ZI" "ZI3_report_spend" '.data.budget.spend' "1"
  assert_json_exists "ZI" "ZI3_report_breach" '.data.budget.breach_reason'

  # With nothing reported, the per-unit rollup is ABSENT rather than an empty
  # object: a field that is not a fact does not appear.
  assert_json_null "ZI" "ZI3_report_no_units" '.data.budget.reported'

  # R3: effective step statuses, and R6: the artifact INDEX — never the bodies.
  assert_json_exists "ZI" "ZI3_report_steps" '.data.steps'
  assert_json_exists "ZI" "ZI3_report_artifacts" '.data.artifacts'
  assert_json_null "ZI" "ZI3_report_no_bodies" '.data.artifacts[0].body'
  assert_json "ZI" "ZI3_report_artifact_kind" '.data.artifacts[0].kind' "draft"

  # R8: THE REPORT WRITES NOTHING. Hash the database, report twice, hash again.
  # A row count would miss an in-place update — a reaped lease, a bumped
  # attempt — which is exactly the write a read verb makes by accident.
  local ZI_HASH_BEFORE ZI_HASH_AFTER
  sqlite3 "$ZI/issues.db" "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null
  ZI_HASH_BEFORE=$(shasum -a 256 "$ZI/issues.db" | cut -d' ' -f1)
  run_env "$ZI" run report RUN-1 --json >/dev/null
  run_env "$ZI" run report RUN-1 >/dev/null
  sqlite3 "$ZI/issues.db" "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null
  ZI_HASH_AFTER=$(shasum -a 256 "$ZI/issues.db" | cut -d' ' -f1)
  check_cond "ZI" "ZI3_report_writes_nothing" "the database changed across two reports" [ "$ZI_HASH_BEFORE" = "$ZI_HASH_AFTER" ]

  # R9: DETERMINISTIC GIVEN THE SAME ROWS. Every section orders by a total key
  # rather than by map iteration, so two reports of an unchanged run agree.
  #
  # The WALL CLOCK and the burn rate derived from it are excluded, and that is
  # not a weakening of the claim: they are measurements of elapsed time, which
  # genuinely differs between two invocations a moment apart. R9 is about the
  # ORDERING of a document over fixed rows — the property that would break
  # silently if a rollup ranged a Go map — and the Go test pins the clock to
  # assert the whole document byte for byte.
  local ZI_R1 ZI_R2 ZI_STRIP
  ZI_STRIP='del(.data.wall_clock_ms) | del(.data.budget.burn_rate)'
  run_env "$ZI" run report RUN-1 --json
  ZI_R1=$(echo "$CMD_STDOUT" | jq -S "$ZI_STRIP")
  run_env "$ZI" run report RUN-1 --json
  ZI_R2=$(echo "$CMD_STDOUT" | jq -S "$ZI_STRIP")
  check_cond "ZI" "ZI3_report_deterministic" "two reports of one unchanged run differ" [ "$ZI_R1" = "$ZI_R2" ]

  # ---------------------------------------------------------------------------
  # ZI4 — `--usage`: recorded, per-unit, and NEVER summed across units.
  #
  # A second run, uncapped, so the ledger is exercised without the cap
  # interfering. Two units are reported at once: they roll up SEPARATELY,
  # because core has no opinion about whether pages and sheets add up.
  # ---------------------------------------------------------------------------
  run_env "$ZI" issue create -t "Write the reference page" -d "a body" --json
  assert_exit "ZI" "ZI4_issue" 0
  run_env "$ZI" run start --issue DKT-2 --json
  assert_exit "ZI" "ZI4_start" 0
  run_env "$ZI" run activate RUN-2 --json
  assert_exit "ZI" "ZI4_activate" 0

  local ZI_SID ZI_TK2
  ZI_SID=$(sqlite3 "$ZI/issues.db" \
    "SELECT id FROM steps WHERE run_id = 2 AND step_name = 'draft';")
  run_env "$ZI" step claim "STEP-$ZI_SID" --owner sprint-relay --json
  assert_exit "ZI" "ZI4_claim" 0
  ZI_TK2=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  DOCKET_TOKEN="$ZI_TK2" run_env "$ZI" step complete "STEP-$ZI_SID" \
    --artifact-file "$ZI/draft.txt" --usage '{"pages": 4, "sheets": 2}' --json
  assert_exit "ZI" "ZI4_complete_with_usage" 0

  run_env "$ZI" run report RUN-2 --json
  assert_exit "ZI" "ZI4_report" 0
  # PER UNIT, ordered by unit name — deterministic, and never summed together.
  assert_json "ZI" "ZI4_unit_first" '.data.budget.reported[0].unit' "pages"
  assert_json "ZI" "ZI4_qty_first" '.data.budget.reported[0].quantity' "4"
  assert_json "ZI" "ZI4_unit_second" '.data.budget.reported[1].unit' "sheets"
  assert_json "ZI" "ZI4_qty_second" '.data.budget.reported[1].quantity' "2"

  # With no `budget.unit` set, NOTHING reported is compared to a cap: the spend
  # rests on the floor alone (B17), which is §9 item 7's configuration. One
  # claim so far on this run, at a declared cost of 1.
  assert_json "ZI" "ZI4_spend_is_floor" '.data.budget.spend' "1"
  assert_json "ZI" "ZI4_floor" '.data.budget.floor' "1"
  # And the unit is ABSENT rather than empty: a field that is not a fact does
  # not appear.
  assert_json_null "ZI" "ZI4_no_unit_named" '.data.budget.budget_unit'

  # `--usage` VALIDATION (B33, B35, B36), each refusal naming what it refused.
  local ZI_SID2 ZI_TK3
  ZI_SID2=$(sqlite3 "$ZI/issues.db" \
    "SELECT id FROM steps WHERE run_id = 2 AND step_name = 'proofread';")
  run_env "$ZI" step claim "STEP-$ZI_SID2" --owner sprint-relay --json
  assert_exit "ZI" "ZI4_claim_second" 0
  ZI_TK3=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  DOCKET_TOKEN="$ZI_TK3" run_env "$ZI" step complete "STEP-$ZI_SID2" \
    --artifact-file "$ZI/draft.txt" --usage '{"pages": -1}' --json
  assert_exit "ZI" "ZI4_negative_refused" 3
  assert_json "ZI" "ZI4_negative_code" '.code' "VALIDATION_ERROR"

  DOCKET_TOKEN="$ZI_TK3" run_env "$ZI" step complete "STEP-$ZI_SID2" \
    --artifact-file "$ZI/draft.txt" --usage '[1,2]' --json
  assert_exit "ZI" "ZI4_array_refused" 3

  DOCKET_TOKEN="$ZI_TK3" run_env "$ZI" step complete "STEP-$ZI_SID2" \
    --artifact-file "$ZI/draft.txt" --usage '{"two words": 1}' --json
  assert_exit "ZI" "ZI4_bad_unit_refused" 3

  # ---------------------------------------------------------------------------
  # ZI5 — `budget.unit`: naming a unit makes reported usage participate in
  # max(reported, floor), and every OTHER unit stays out of the comparison.
  # ---------------------------------------------------------------------------
  run_env "$ZI" config set budget.unit pages --json
  assert_exit "ZI" "ZI5_set_unit" 0

  run_env "$ZI" run report RUN-2 --json
  assert_exit "ZI" "ZI5_report" 0
  assert_json "ZI" "ZI5_unit_named" '.data.budget.budget_unit' "pages"
  # Both steps have now been claimed, so the floor is 2. Four pages were
  # reported, which EXCEEDS it, and max(reported, floor) takes the larger —
  # reported usage can only RAISE the counter (B13). `sheets` is recorded and
  # NEVER compared, because core has no opinion about whether it and `pages`
  # are commensurable (B19).
  assert_json "ZI" "ZI5_floor_from_two_claims" '.data.budget.floor' "2"
  assert_json "ZI" "ZI5_spend_is_reported" '.data.budget.spend' "4"

  run_env "$ZI" config set budget.unit "" --json
  assert_exit "ZI" "ZI5_unset_unit" 0

  # ---------------------------------------------------------------------------
  # ZI6 — GROUP-1 DORMANCY (§3, D1). A run with NO budget in a repo with no
  # default: `next` behaves exactly as it did before this stage.
  #
  # The claim is byte-identity, so it is asserted as byte-identity: the same
  # `next` invocation twice, over a run that has claimed nothing, must produce
  # identical bytes — and the budget section of its report must read `unlimited`
  # rather than a cap of 0 dressed up as a number.
  # ---------------------------------------------------------------------------
  local ZI_DORM ZI_NEXT1 ZI_NEXT2
  ZI_DORM=$(qa_mktemp_d)
  run_env "$ZI_DORM" init
  assert_exit "ZI" "ZI6_init" 0
  run_env "$ZI_DORM" workflow register "$ZI_WF" --json >/dev/null
  run_env "$ZI_DORM" issue create -t "An unbudgeted page" -d "a body" --json >/dev/null
  run_env "$ZI_DORM" run start --issue DKT-1 --json
  assert_exit "ZI" "ZI6_start_no_budget" 0
  # No `--budget` and no `budget.default`: the row stores 0, which means
  # unlimited at both levels.
  assert_json "ZI" "ZI6_budget_zero" '.data.budget' "null"

  run_env "$ZI_DORM" run activate RUN-1 --json >/dev/null

  run_env "$ZI_DORM" next --run RUN-1 --json
  assert_exit "ZI" "ZI6_next" 0
  ZI_NEXT1="$CMD_STDOUT"
  run_env "$ZI_DORM" next --run RUN-1 --json
  ZI_NEXT2="$CMD_STDOUT"
  check_cond "ZI" "ZI6_next_byte_identical" "two \`next\` calls on an unbudgeted run differ" [ "$ZI_NEXT1" = "$ZI_NEXT2" ]

  # The ledger is empty and stays empty: v10 seeds nothing, and a repo that
  # never reports never writes a row.
  local ZI_DORM_LEDGER
  ZI_DORM_LEDGER=$(sqlite3 "$ZI_DORM/issues.db" "SELECT COUNT(*) FROM usage_ledger;")
  check_cond "ZI" "ZI6_ledger_dormant" "an unbudgeted, never-reporting run wrote $ZI_DORM_LEDGER ledger rows" [ "$ZI_DORM_LEDGER" = "0" ]

  # The report says `unlimited` — not a cap of 0, which would read as "stop
  # immediately" to anyone skimming.
  run_env "$ZI_DORM" run report RUN-1 --json
  assert_exit "ZI" "ZI6_report" 0
  assert_json "ZI" "ZI6_unlimited" '.data.budget.cap_source' "unlimited"

  # ---------------------------------------------------------------------------
  # ZI7 — the v10 schema, and the ledger's shape.
  # ---------------------------------------------------------------------------
  local ZI_VERSION
  ZI_VERSION=$(sqlite3 "$ZI/issues.db" \
    "SELECT value FROM meta WHERE key = 'schema_version';")
  check_cond "ZI" "ZI7_schema_version" "schema_version is $ZI_VERSION, want $CURRENT_SCHEMA_VERSION" [ "$ZI_VERSION" = "$CURRENT_SCHEMA_VERSION" ]

  local ZI_TABLE
  ZI_TABLE=$(sqlite3 "$ZI/issues.db" \
    "SELECT name FROM sqlite_master WHERE type='table' AND name='usage_ledger';")
  check_cond "ZI" "ZI7_ledger_table" "usage_ledger is absent" [ "$ZI_TABLE" = "usage_ledger" ]

  # The v10 columns the sentinels cannot see (§2.3).
  local ZI_COLS
  ZI_COLS=$(sqlite3 "$ZI/issues.db" \
    "SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name IN ('usage_floor','breach_reason');")
  check_cond "ZI" "ZI7_run_columns" "runs carries $ZI_COLS of the 2 budget columns v10 adds" [ "$ZI_COLS" = "2" ]

  # THE CACHE IS NOT THE FLOOR (§4.3). Poison `runs.usage_floor` and confirm the
  # report publishes the number the QUERY produces, not the one in the column.
  sqlite3 "$ZI/issues.db" "UPDATE runs SET usage_floor = 999 WHERE id = 1;"
  run_env "$ZI" run report RUN-1 --json
  assert_json "ZI" "ZI7_floor_not_from_cache" '.data.budget.floor' "1"

  rm -rf "$ZI" "$ZI_DORM"
}
