#!/usr/bin/env bash
#
# ZM — COMPLETION METADATA REACHES THE STEP
# (docs/tdd/completion-metadata.md).
#
# THIS SECTION IS THE LITERAL INVERSE OF THE ORIGINAL REPRODUCTION, which
# recorded, through the CLI:
#
#   $ docket step complete STEP-27 --artifact-file art.txt --metadata '{…}'
#   ✔ Completed STEP-27
#   $ docket run report RUN-4 --json | jq .data.metadata
#   null
#
# Exit 0, and the bag gone. The flag parsed, the option field existed and
# documented a merge, the column existed, and the R7 rollup read that column
# correctly — but nothing in the engine read the option, so ZERO steps in ANY
# database carried metadata. The failure was SILENT, which is what made it
# expensive: an instance emitting its bag correctly observed nothing and
# reasonably concluded it was itself at fault.
#
# So the headline assertion here is `.data.metadata` NON-NULL after one
# completion — the exact query that returned `null`, through the same verbs, at
# the same layer. The Go tests prove the merge; this proves the WIRE.
#
# THE STRANGER TEST (§1.1). The fixture is a print shop, as ZJ's is: a person
# runs a press, a person records which desk did the work and whether it needed
# rework. The bag's keys are `desk` and `rework` — the reference instance's own
# routing keys are INSTANCE DATA and must never appear in core, and
# scripts/qa/genericity.sh scans this file like any other.

test_zm_metadata() {
  printf "Section ZM: Completion metadata (DKT-68)"

  local ZM ZM_WF ZM_TOKEN ZM_ROWS
  ZM=$(qa_mktemp_d)

  run_env "$ZM" init
  assert_exit "ZM" "ZM0_init" 0

  # ---------------------------------------------------------------------------
  # THE FIXTURE: one executor step, which is all the wire proof needs.
  # ---------------------------------------------------------------------------
  ZM_WF="$ZM/printshop.toml"
  cat >"$ZM_WF" <<'TOML'
[pipeline]
name = "print-shop-metadata"
version = 1
description = "Run a job on the press and record which desk handled it."

[match]
kind = ["task"]

[[step]]
name = "print-run"
executor = "press-operator"
emits = "sheets"
after = []
TOML

  run_env "$ZM" workflow register "$ZM_WF" --json
  assert_exit "ZM" "ZM0_register" 0

  run_env "$ZM" issue create -t "Print the spring catalogue" -d "500 copies" --json
  assert_exit "ZM" "ZM0_issue" 0
  run_env "$ZM" run start --issue DKT-1 --json
  assert_exit "ZM" "ZM0_start" 0
  run_env "$ZM" run activate RUN-1 --json
  assert_exit "ZM" "ZM0_activate" 0

  printf 'the printed sheets\n' >"$ZM/sheets.txt"

  # ===========================================================================
  # M1 — THE HEADLINE: the bag survives the completion and reaches the report
  # ===========================================================================

  run_env "$ZM" step claim STEP-1 --owner press-a --json
  assert_exit "ZM" "ZM1_claim" 0
  ZM_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  DOCKET_TOKEN="$ZM_TOKEN" run_env "$ZM" step complete STEP-1 \
    --artifact-file "$ZM/sheets.txt" --metadata '{"desk":"front","rework":"true"}' --json
  assert_exit "ZM" "ZM1_complete" 0

  # The column is populated. The original probe returned 0 here, across every
  # run in its sandbox.
  ZM_ROWS=$(sqlite3 "$ZM/issues.db" \
    "SELECT COUNT(*) FROM steps WHERE metadata IS NOT NULL AND metadata != '';")
  check_cond "ZM" "ZM1_column_written" "expected 1 step carrying metadata, found $ZM_ROWS (DKT-68's probe found 0)" [ "$ZM_ROWS" = "1" ]

  # THE ASSERTION THIS SECTION EXISTS FOR: `.data.metadata` is no longer null.
  run_env "$ZM" run report RUN-1 --json
  assert_exit "ZM" "ZM1_report" 0
  assert_json_exists "ZM" "ZM1_report_metadata_present" '.data.metadata'

  # R7 is key -> distinct value -> count, both levels sorted. `desk` sorts
  # before `rework`, so the nesting is addressable without a search.
  assert_json "ZM" "ZM1_rollup_key" '.data.metadata[0].key' "desk"
  assert_json "ZM" "ZM1_rollup_value" '.data.metadata[0].values[0].value' "front"
  assert_json "ZM" "ZM1_rollup_count" '.data.metadata[0].values[0].count' "1"

  # The text renderer shows it too — the operator who never passes --json is
  # the one most likely to be debugging a silent drop.
  run_env "$ZM" run report RUN-1
  assert_exit "ZM" "ZM1_report_text" 0
  assert_stdout_contains "ZM" "ZM1_report_text_metadata" "Metadata"
  assert_stdout_contains "ZM" "ZM1_report_text_key" "desk"

  # ===========================================================================
  # M2 — A REFUSAL WRITES NOTHING, and the step survives it
  # ===========================================================================
  #
  # §6.9's property at the CLI: validation happens BEFORE the transaction
  # opens, so a rejected completion leaves the step exactly as it was — still
  # claimable, still completable. A worker that fat-fingers its bag loses the
  # round trip and nothing else.

  run_env "$ZM" issue create -t "Print the summer catalogue" -d "200 copies" --json
  assert_exit "ZM" "ZM2_issue" 0
  run_env "$ZM" run start --issue DKT-2 --json
  assert_exit "ZM" "ZM2_start" 0
  run_env "$ZM" run activate RUN-2 --json
  assert_exit "ZM" "ZM2_activate" 0

  run_env "$ZM" step claim STEP-2 --owner press-b --json
  assert_exit "ZM" "ZM2_claim" 0
  ZM_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  # Not JSON at all -> VALIDATION_ERROR (exit 3).
  DOCKET_TOKEN="$ZM_TOKEN" run_env "$ZM" step complete STEP-2 \
    --artifact-file "$ZM/sheets.txt" --metadata 'not json' --json
  assert_exit "ZM" "ZM2_invalid_refused" 3

  # Valid JSON, wrong shape: a KV bag with no keys has nothing to merge and
  # nothing for the rollup to group by.
  DOCKET_TOKEN="$ZM_TOKEN" run_env "$ZM" step complete STEP-2 \
    --artifact-file "$ZM/sheets.txt" --metadata '["front"]' --json
  assert_exit "ZM" "ZM2_array_refused" 3

  # Neither refusal recorded an artifact — the step never entered its saga.
  ZM_ROWS=$(sqlite3 "$ZM/issues.db" \
    "SELECT COUNT(*) FROM artifacts WHERE step_id = 2;")
  check_cond "ZM" "ZM2_refusal_wrote_nothing" "a refused completion recorded $ZM_ROWS artifacts" [ "$ZM_ROWS" = "0" ]

  # And the step still completes on the SAME token: the refusal cost nothing.
  DOCKET_TOKEN="$ZM_TOKEN" run_env "$ZM" step complete STEP-2 \
    --artifact-file "$ZM/sheets.txt" --metadata '{"desk":"back"}' --json
  assert_exit "ZM" "ZM2_completes_after_refusal" 0

  # ===========================================================================
  # M3 — NO `--metadata` IS NOT A WRITE
  # ===========================================================================
  #
  # Every caller that predates the flag passes no `--metadata`, and none of
  # them may see a step's bag change. The step below carries no definition-side
  # metadata either, so its column must stay empty rather than gaining `{}`.

  run_env "$ZM" issue create -t "Print the autumn catalogue" -d "100 copies" --json
  assert_exit "ZM" "ZM3_issue" 0
  run_env "$ZM" run start --issue DKT-3 --json
  assert_exit "ZM" "ZM3_start" 0
  run_env "$ZM" run activate RUN-3 --json
  assert_exit "ZM" "ZM3_activate" 0

  run_env "$ZM" step claim STEP-3 --owner press-c --json
  assert_exit "ZM" "ZM3_claim" 0
  ZM_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  DOCKET_TOKEN="$ZM_TOKEN" run_env "$ZM" step complete STEP-3 \
    --artifact-file "$ZM/sheets.txt" --json
  assert_exit "ZM" "ZM3_complete" 0

  ZM_ROWS=$(sqlite3 "$ZM/issues.db" \
    "SELECT COUNT(*) FROM steps WHERE id = 3 AND (metadata IS NULL OR metadata = '');")
  check_cond "ZM" "ZM3_absent_flag_writes_nothing" "a completion with no --metadata wrote to the column" [ "$ZM_ROWS" = "1" ]

  # That run's report has no metadata section at all — `omitempty` elides it,
  # which is the CORRECT null: nothing was reported, so nothing is claimed.
  run_env "$ZM" run report RUN-3 --json
  assert_exit "ZM" "ZM3_report" 0
  assert_json_null "ZM" "ZM3_report_metadata_absent" '.data.metadata'

  # ===========================================================================
  # M4 — THE CAP, at the CLI
  # ===========================================================================
  #
  # The bag is opaque, so nothing else bounds it; the rollup groups BY DISTINCT
  # VALUE, so an unbounded bag degrades the one read surface this path feeds.
  # The refusal names both numbers and points at the two purpose-built channels.

  run_env "$ZM" issue create -t "Print the winter catalogue" -d "50 copies" --json
  assert_exit "ZM" "ZM4_issue" 0
  run_env "$ZM" run start --issue DKT-4 --json
  assert_exit "ZM" "ZM4_start" 0
  run_env "$ZM" run activate RUN-4 --json
  assert_exit "ZM" "ZM4_activate" 0

  run_env "$ZM" step claim STEP-4 --owner press-d --json
  assert_exit "ZM" "ZM4_claim" 0
  ZM_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  local ZM_BIG
  ZM_BIG=$(printf '{"desk":"%s"}' "$(head -c 17000 /dev/zero | tr '\0' 'a')")
  DOCKET_TOKEN="$ZM_TOKEN" run_env "$ZM" step complete STEP-4 \
    --artifact-file "$ZM/sheets.txt" --metadata "$ZM_BIG" --json
  assert_exit "ZM" "ZM4_oversized_refused" 3
  assert_json "ZM" "ZM4_code" '.code' "VALIDATION_ERROR"

  # BOTH numbers and the remedy. An operator who hits the cap needs to know how
  # far over they are and where the detail belongs instead — the two
  # purpose-built channels are named rather than implied.
  assert_stdout_contains "ZM" "ZM4_names_the_actual_size" "17011"
  assert_stdout_contains "ZM" "ZM4_names_the_cap" "16384"
  assert_stdout_contains "ZM" "ZM4_names_the_remedy" "artifact or the payload"

  rm -rf "$ZM"
}
