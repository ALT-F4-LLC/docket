#!/usr/bin/env bash
#
# ZK — THE ZERO-TOUCH REHEARSAL, THE EVENTS READ SURFACE, AND THE TWO GUARDS
# (runs-dispatch §7, §8, §9, §9.8).
#
# TWO HEADLINES, both engine-spec §9 acceptance criteria, verbatim:
#
#   2. Zero model-made scheduling decisions in a full run: every transition in
#      events traceable to next/gate/threshold/human input.
#
#  11. Zero-touch: from `docket workflow init --template …` through a completed
#      run, the only human inputs are conversational approvals relayed by the
#      harness — no hand-edited config, no manual registration.
#
# ITEM 11'S CRITERION IS Z9'S TRACE-GREP, and it is the reason this section is
# built the way it is. A rehearsal that merely reaches `done` proves the engine
# works. A rehearsal that reaches `done` WITH `workflow register` AND `schema
# register` PROVEN ABSENT FROM THE COMMAND TRACE proves item 11 — because if the
# implementation quietly needed a manual register, the grep finds it. Every
# docket invocation in the rehearsal goes through `zk_run`, which appends its
# argv to a trace file; nothing else may invoke the binary.
#
# ITEM 2 IS AN AUDIT OVER A COMPLETED RUN, now checkable end-to-end because the
# events read surface exists: the section drives a run to `done`, lists every
# event through `events list`, and asserts that each kind maps to one of the four
# actors and that no kind is outside the closed set. That is §8.7's attribution
# table, executed rather than argued.
#
# THE STRANGER TEST (§1.1). The fixture is a print shop's proofing sheet: a
# person runs the checks, a person approves. No model, no agent, no prompt —
# `executor` is a role name and the whole rehearsal reads as "we wrote down how
# we work, and the tracker followed it".
#
# The section also carries the GROUP-3 DORMANCY proof (§3): `events list` and
# both guards add no output to any existing verb, and A REPO WITH NO
# `.docket/config/` ACTIVATES BYTE-IDENTICALLY (F17/F19).

test_zk_zerotouch() {
  printf "Section ZK: Zero-touch, the events read surface, and the guards"

  # SB5: this section grants trust (Z3), so the sandbox is asserted before
  # anything runs. A hard abort, never a skip — a skip would report "fine" while
  # writing into the operator's real trust store.
  assert_trust_sandbox

  local ZK ZK_TRACE ZK_SENTINEL ZK_WF
  ZK=$(qa_mktemp_d)
  ZK_TRACE="$ZK/command-trace"
  ZK_SENTINEL="$ZK/checks-ran"
  : >"$ZK_TRACE"

  # zk_run is THE ONLY WAY this section invokes docket. It appends the
  # invocation to the trace before running, which is what makes Z9's grep a
  # statement about the WHOLE rehearsal rather than about the commands someone
  # remembered to record.
  #
  # It records the VERB — the leading non-flag words — on ONE LINE, rather than
  # the whole argv. An issue body is a multi-line argument (it carries a fenced
  # block, which is the point of Z2), and a trace that spilled it across lines
  # would make every line-oriented assertion below read fragments of prose as
  # commands. The verb is what Z9 and Z11 are about: whether `workflow register`
  # was invoked, not what was passed to `issue create`.
  zk_run() {
    local verb=""
    local word
    for word in "$@"; do
      case "$word" in
        -*) break ;;
        *) verb="${verb:+$verb }$word" ;;
      esac
    done
    printf 'docket %s\n' "$verb" >>"$ZK_TRACE"
    run_env "$ZK" "$@"
  }

  # ---------------------------------------------------------------------------
  # Z1 — SCAFFOLD. `workflow init` writes the template and REGISTERS NOTHING.
  # ---------------------------------------------------------------------------
  zk_run init
  assert_exit "ZK" "ZK_Z1_init" 0

  zk_run workflow init --template standard-dev --json
  assert_exit "ZK" "ZK_Z1_scaffold" 0

  ZK_WF="$ZK/config/workflows/standard-dev.toml"
  check_cond "ZK" "ZK_Z1_file_written" "no template at $ZK_WF" [ -f "$ZK_WF" ]

  # NOTHING IS REGISTERED YET. This is half of what makes Z5 meaningful: the
  # registration must happen AT ACTIVATION, not as a side effect of scaffolding.
  local ZK_WF_ROWS
  ZK_WF_ROWS=$(sqlite3 "$ZK/issues.db" "SELECT COUNT(*) FROM workflows;")
  check_cond "ZK" "ZK_Z1_nothing_registered" "$ZK_WF_ROWS workflows registered by \`workflow init\`; registration is activation's" [ "$ZK_WF_ROWS" = "0" ]

  # Z10's first half: the file's hash, taken now, to be compared at Z8. It
  # proves NO CONFIG FILE WAS EDITED after the template wrote it.
  local ZK_WF_HASH_BEFORE
  ZK_WF_HASH_BEFORE=$(shasum "$ZK_WF" | cut -d' ' -f1)

  # ---------------------------------------------------------------------------
  # Z2 — THE WORK. An issue whose body carries a ```checks fence.
  # ---------------------------------------------------------------------------
  local ZK_BODY
  ZK_BODY=$(printf 'Proof the sheets before they go to press.\n\n```checks\n/usr/bin/touch %s\n```\n' "$ZK_SENTINEL")
  zk_run issue create -t "Proof the sheets" -d "$ZK_BODY" --type task --json
  assert_exit "ZK" "ZK_Z2_issue" 0

  # The fence is NOT harvested yet — harvesting is activation's, which is what
  # makes "post-activation edits cannot inject" true.
  local ZK_FENCES
  ZK_FENCES=$(sqlite3 "$ZK/issues.db" "SELECT COUNT(*) FROM run_fences;")
  check_cond "ZK" "ZK_Z2_not_harvested_yet" "$ZK_FENCES fences before activation" [ "$ZK_FENCES" = "0" ]

  # ---------------------------------------------------------------------------
  # Z3 — THE ONE TRUST GRANT. The single human-authorized act, matching §4's
  # conversational posture: a person says yes to a specific command, once.
  #
  # The trust store is SHARED ACROSS QA SECTIONS (one XDG_CONFIG_HOME per run,
  # per SB2), and section ZH has already granted the name `checks` for ITS
  # sentinel path. The gate's tag comes from the shipped template and this
  # section must use that template verbatim (Z1/Z10), so the name cannot be
  # varied — the stale entry is removed first, which is exactly the recovery the
  # CONFLICT message itself prescribes.
  #
  # This `trust rm` is SETUP, not part of the rehearsal, so it deliberately
  # bypasses `zk_run`: Z11 enumerates the rehearsal's human-shaped inputs, and a
  # cleanup command in that list would misreport what the rehearsal required.
  run_env "$ZK" trust rm checks >/dev/null 2>&1 || true

  zk_run trust add checks --yes -- /usr/bin/touch "$ZK_SENTINEL"
  assert_exit "ZK" "ZK_Z3_trust" 0

  # ---------------------------------------------------------------------------
  # Z4/Z5 — START AND ACTIVATE. Activation AUTO-REGISTERS standard-dev@1,
  # reports it (F20/F21), harvests the fence, binds, expands, and promotes — all
  # in one command, with no register verb anywhere.
  # ---------------------------------------------------------------------------
  zk_run run start --issue DKT-1 --json
  assert_exit "ZK" "ZK_Z4_start" 0
  assert_json "ZK" "ZK_Z4_planning" ".data.status" "planning"

  zk_run run activate RUN-1 --json
  assert_exit "ZK" "ZK_Z5_activate" 0

  # F21: the `registered` array, with the outcome.
  assert_json "ZK" "ZK_Z5_registered_len" "(.data.registered | length)" "1"
  assert_json "ZK" "ZK_Z5_registered_kind" ".data.registered[0].kind" "workflow"
  assert_json "ZK" "ZK_Z5_registered_name" ".data.registered[0].name" "standard-dev"
  assert_json "ZK" "ZK_Z5_registered_outcome" ".data.registered[0].outcome" "new"

  # And it actually landed in the registry, bound the issue, and expanded.
  ZK_WF_ROWS=$(sqlite3 "$ZK/issues.db" "SELECT COUNT(*) FROM workflows;")
  check_cond "ZK" "ZK_Z5_registry_row" "$ZK_WF_ROWS workflow rows after activation, want 1" [ "$ZK_WF_ROWS" = "1" ]
  assert_json "ZK" "ZK_Z5_bound" ".data.issues_bound" "1"

  local ZK_STEPS
  ZK_STEPS=$(sqlite3 "$ZK/issues.db" "SELECT COUNT(*) FROM steps;")
  check_cond "ZK" "ZK_Z5_expanded" "$ZK_STEPS steps after activation, want the check and the approval" [ "$ZK_STEPS" -ge 2 ]

  # ---------------------------------------------------------------------------
  # Z6 — EXECUTE. next -> claim -> complete. The fenced gate runs and passes,
  # because Z3's grant authorized exactly this argv.
  # ---------------------------------------------------------------------------
  zk_run next --run RUN-1 --json
  assert_exit "ZK" "ZK_Z6_next" 0

  local ZK_STEP
  ZK_STEP=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.steps[0].step')
  zk_run step claim "$ZK_STEP" --owner press-operator --json
  assert_exit "ZK" "ZK_Z6_claim" 0

  local ZK_TOKEN
  ZK_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')

  printf 'the sheets read clean\n' >"$ZK/report.txt"
  # Traced by hand because the token rides in the ENVIRONMENT, not argv (a token
  # in argv is world-readable through `ps`), so this one invocation cannot go
  # through `zk_run`. The recorded line is the verb, in the same shape zk_run
  # writes.
  printf 'docket step complete %s\n' "$ZK_STEP" >>"$ZK_TRACE"
  DOCKET_TOKEN="$ZK_TOKEN" run_env "$ZK" step complete "$ZK_STEP" \
    --artifact-file "$ZK/report.txt" --json
  assert_exit "ZK" "ZK_Z6_complete" 0

  # The gate MATCHED and RAN — Z3's grant is what made that legal, and the
  # sentinel is the evidence.
  check_cond "ZK" "ZK_Z6_gate_ran" "the trusted fenced gate did not execute; the rehearsal is not exercising the gate path" [ -e "$ZK_SENTINEL" ]

  local ZK_VERDICT
  ZK_VERDICT=$(sqlite3 "$ZK/issues.db" "SELECT verdict FROM gate_results ORDER BY id LIMIT 1;")
  check_cond "ZK" "ZK_Z6_gate_passed" "the gate verdict is '$ZK_VERDICT', want pass" [ "$ZK_VERDICT" = "pass" ]

  # ---------------------------------------------------------------------------
  # Z7/Z8 — APPROVE, AND REACH `done`.
  # ---------------------------------------------------------------------------
  zk_run next --run RUN-1 --json
  local ZK_HUMAN
  ZK_HUMAN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.steps[0].step')

  zk_run step approve "$ZK_HUMAN" --json
  assert_exit "ZK" "ZK_Z7_approve" 0

  zk_run next --run RUN-1 --json
  assert_exit "ZK" "ZK_Z8_next_empty" 0
  assert_json "ZK" "ZK_Z8_no_work_left" "(.data.steps | length)" "0"

  zk_run run status RUN-1 --json
  assert_exit "ZK" "ZK_Z8_status" 0
  # `run status` nests the run under `.data.run` and reports the issue count
  # beside it, so the status path is one level deeper than `run start`'s.
  assert_json "ZK" "ZK_Z8_done" ".data.run.status" "done"

  # ---------------------------------------------------------------------------
  # Z9 — **THE ASSERTION**. `docket workflow register` and `docket schema
  # register` were NEVER INVOKED across the whole rehearsal.
  #
  # This is item 11's criterion, and the trace-grep is the mechanism because IT
  # CANNOT PASS BY ACCIDENT: every invocation above went through `zk_run`, which
  # recorded it, so a hidden manual register would be in the file.
  # ---------------------------------------------------------------------------
  local ZK_REGISTER_HITS
  ZK_REGISTER_HITS=$(grep -cE '^docket (workflow|schema) register' "$ZK_TRACE" || true)
  check_cond "ZK" "ZK_Z9_no_manual_registration" "$ZK_REGISTER_HITS manual register invocation(s) in the trace; §9 item 11 requires zero" [ "$ZK_REGISTER_HITS" = "0" ]

  # The trace is NON-EMPTY and actually holds the rehearsal, so the zero above
  # is the zero of a search that ran rather than of a file nobody wrote to.
  local ZK_TRACE_LINES
  ZK_TRACE_LINES=$(wc -l <"$ZK_TRACE" | tr -d ' ')
  check_cond "ZK" "ZK_Z9_trace_populated" "the trace holds $ZK_TRACE_LINES lines; the grep above would be vacuous" [ "$ZK_TRACE_LINES" -ge 10 ]

  # ---------------------------------------------------------------------------
  # Z10 — NO CONFIG FILE WAS EDITED after `workflow init` wrote it.
  # ---------------------------------------------------------------------------
  local ZK_WF_HASH_AFTER
  ZK_WF_HASH_AFTER=$(shasum "$ZK_WF" | cut -d' ' -f1)
  check_cond "ZK" "ZK_Z10_config_unedited" "the workflow file changed during the rehearsal; §9 item 11 forbids hand-edited config" [ "$ZK_WF_HASH_BEFORE" = "$ZK_WF_HASH_AFTER" ]

  # ---------------------------------------------------------------------------
  # Z11 — THE HUMAN-SHAPED INPUTS, ENUMERATED.
  #
  # The only ones across the REHEARSAL are Z3's `trust add --yes`, Z7's
  # `approve`, and the ordinary issue/run verbs a harness relays. Asserted by
  # the same trace: no verb outside that set appears.
  #
  # The snapshot is taken HERE, at the end of the rehearsal proper, because the
  # audit and dormancy checks below legitimately run read verbs (`events list`,
  # `guard record|spawn`, `run report`) that are not rehearsal inputs. Freezing
  # the trace at this line keeps Z11 a statement about the ZERO-TOUCH PATH rather
  # than about everything the section happens to do afterwards.
  # ---------------------------------------------------------------------------
  local ZK_REHEARSAL_TRACE
  ZK_REHEARSAL_TRACE="$ZK/rehearsal-trace"
  cp "$ZK_TRACE" "$ZK_REHEARSAL_TRACE"

  local ZK_UNEXPECTED
  ZK_UNEXPECTED=$(grep -vE '^docket (init|workflow init|issue create|trust add|run start|run activate|run status|next|step (claim|complete|approve))' \
    "$ZK_REHEARSAL_TRACE" | grep -v '^$' || true)
  if [ -z "$ZK_UNEXPECTED" ]; then
    check "ZK" "ZK_Z11_inputs_enumerated" "PASS"
  else
    check "ZK" "ZK_Z11_inputs_enumerated" "FAIL" \
      "the rehearsal used a verb outside the enumerated set: $(printf '%s' "$ZK_UNEXPECTED" | head -1)"
  fi

  # ---------------------------------------------------------------------------
  # §9 ITEM 2 — THE FULL ATTRIBUTION AUDIT OVER THE COMPLETED RUN.
  #
  # "Every transition in events traceable to next/gate/threshold/human input."
  # Now checkable END-TO-END through the read surface: every event the run
  # produced is listed, and every kind must map to one of the four actors.
  #
  # The actor table lives in internal/engine/event_read.go and is asserted
  # complete by TestEveryEventKindHasAnActor; this is the same claim executed
  # over REAL EVENTS FROM A REAL RUN, which is what §9 item 2 asks for.
  # ---------------------------------------------------------------------------
  zk_run events list --run RUN-1 --limit 500 --json
  assert_exit "ZK" "ZK_A1_events_list" 0

  local ZK_EVENT_TOTAL ZK_TABLE_TOTAL
  ZK_EVENT_TOTAL=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.total')
  ZK_TABLE_TOTAL=$(sqlite3 "$ZK/issues.db" "SELECT COUNT(*) FROM events WHERE run_id = 1;")
  if [ "$ZK_EVENT_TOTAL" = "$ZK_TABLE_TOTAL" ] && [ "$ZK_EVENT_TOTAL" -gt 0 ]; then
    check "ZK" "ZK_A1_feed_complete" "PASS"
  else
    check "ZK" "ZK_A1_feed_complete" "FAIL" \
      "the feed reports $ZK_EVENT_TOTAL and the table holds $ZK_TABLE_TOTAL"
  fi

  # THE CLOSED SET AND THE ATTRIBUTION, over every kind the run produced. The
  # actor list is restated here as the FOUR CAUSES §9 item 2 names — this is the
  # acceptance criterion's own vocabulary, not a copy of the implementation's
  # table, so a kind that drifted into a fifth category fails here.
  local ZK_KINDS ZK_UNATTRIBUTED
  ZK_KINDS=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.events[].kind' | sort -u)
  ZK_UNATTRIBUTED=""
  local kind
  for kind in $ZK_KINDS; do
    case "$kind" in
      # next — the scheduler
      step-ready|lease-reaped|join-completed|loop-entered|dispatch-abandoned|issue-promoted|issue-in-progress|issue-review) ;;
      # gate — a deterministic check, actions included
      gate-started|gate-recorded|gate-unmatched|gate-rerun|vote-opened|vote-tallied) ;;
      # threshold — computed routing
      step-routed|step-failed|step-superseded|step-skipped|step-held) ;;
      # human — an operator verb, including one a harness relays
      run-started|run-activated|run-paused|run-resumed|run-abandoned|run-done) ;;
      step-claimed|step-heartbeat|step-recorded|step-resolved|step-approved|step-rejected) ;;
      issue-abandoned|trust-added|trust-removed) ;;
      dispatch-opened|dispatch-closed|reap-acknowledged) ;;
      *) ZK_UNATTRIBUTED="$ZK_UNATTRIBUTED $kind" ;;
    esac
  done
  check_cond "ZK" "ZK_A2_every_kind_attributable" "event kind(s) outside the four causes §9 item 2 names:$ZK_UNATTRIBUTED" [ -z "$ZK_UNATTRIBUTED" ]

  # The audit is NOT VACUOUS: the run produced several distinct kinds spanning
  # more than one actor. A run with one kind would pass the loop above while
  # proving nothing about attribution.
  local ZK_KIND_COUNT
  ZK_KIND_COUNT=$(printf '%s\n' "$ZK_KINDS" | grep -c . || true)
  check_cond "ZK" "ZK_A2_audit_nonvacuous" "the completed run produced only $ZK_KIND_COUNT distinct event kinds" [ "$ZK_KIND_COUNT" -ge 4 ]

  # The report's per-actor rollup agrees with the feed, so §9 item 2's answer is
  # readable without leaving `run report`.
  zk_run run report RUN-1 --json
  assert_exit "ZK" "ZK_A3_report" 0
  local ZK_ACTOR_SUM
  ZK_ACTOR_SUM=$(printf '%s' "$CMD_STDOUT" | jq -r '[.data.actors[].count] | add')
  if [ "$ZK_ACTOR_SUM" = "$ZK_TABLE_TOTAL" ]; then
    check "ZK" "ZK_A3_actor_counts_sum" "PASS"
  else
    check "ZK" "ZK_A3_actor_counts_sum" "FAIL" \
      "the per-actor counts sum to $ZK_ACTOR_SUM over $ZK_TABLE_TOTAL events — " \
      "an event attributable to nothing"
  fi

  # ---------------------------------------------------------------------------
  # THE CURSOR CONTRACT (§8.3), over the completed run's real feed.
  # ---------------------------------------------------------------------------
  zk_run events list --run RUN-1 --limit 2 --json
  local ZK_FIRST_SEQ ZK_SECOND_SEQ
  ZK_FIRST_SEQ=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.events[0].seq')
  ZK_SECOND_SEQ=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.events[1].seq')

  # E9: `total` counts matching events BEFORE the slice, so truncation is
  # computable rather than guessed.
  assert_json "ZK" "ZK_C1_total_undistorted" ".data.total" "$ZK_TABLE_TOTAL"

  # E5: STRICTLY greater. A consumer stores the last seq it saw and passes it
  # back WITHOUT re-reading it.
  zk_run events list --run RUN-1 --since "$ZK_FIRST_SEQ" --limit 1 --json
  local ZK_AFTER_SEQ
  ZK_AFTER_SEQ=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.events[0].seq')
  check_cond "ZK" "ZK_C2_cursor_strict" "--since $ZK_FIRST_SEQ returned seq $ZK_AFTER_SEQ, want $ZK_SECOND_SEQ" [ "$ZK_AFTER_SEQ" = "$ZK_SECOND_SEQ" ]

  # A cursor past the end is an EMPTY PAGE, not an error: a consumer that has
  # caught up must be able to poll without handling a refusal.
  zk_run events list --run RUN-1 --since 100000 --json
  assert_exit "ZK" "ZK_C3_caught_up_exit" 0
  assert_json "ZK" "ZK_C3_caught_up_empty" "(.data.events | length)" "0"

  # E4: the shape is JOINABLE. `step_id` addresses a step the way every
  # other verb does, so a consumer can follow the feed into `step show`.
  zk_run events list --run RUN-1 --limit 500 --json
  local ZK_STEP_ID
  ZK_STEP_ID=$(printf '%s' "$CMD_STDOUT" | jq -r '[.data.events[] | select(.step_id != null)][0].step_id')
  if [ -n "$ZK_STEP_ID" ] && [ "$ZK_STEP_ID" != "null" ]; then
    zk_run step show "$ZK_STEP_ID" --json
    assert_exit "ZK" "ZK_C4_step_id_joins" 0
  else
    check "ZK" "ZK_C4_step_id_joins" "FAIL" \
      "no event carried a step_id; the shape is unjoinable"
  fi

  # E12/E11: the v2 envelope is the uniform Collection shape.
  zk_run events list --run RUN-1 --limit 1 --json=v2
  assert_json "ZK" "ZK_C5_v2_keys" "(.data | keys | sort | join(\",\"))" "items,total,truncated"
  assert_json "ZK" "ZK_C5_v2_truncated" ".data.truncated" "true"

  # ---------------------------------------------------------------------------
  # THE GUARDS (§7), over the completed run.
  #
  # Both allow: nothing is unreconciled, and no reap is unacknowledged. That is
  # the ORDINARY state, and a guard that denied by default would be one every
  # harness disables on its second day.
  # ---------------------------------------------------------------------------
  zk_run guard record --run RUN-1
  assert_exit "ZK" "ZK_G1_record_allows" 0

  zk_run guard spawn --run RUN-1
  assert_exit "ZK" "ZK_G2_spawn_allows" 0

  # G4: without `--run`, the guard answers over every non-terminal run — the
  # `guard stop` shape, so a hook wired once keeps working as runs come and go.
  zk_run guard record
  assert_exit "ZK" "ZK_G3_record_all_runs" 0

  # G8: `--rows` with no open dispatch is a DENIAL (exit 2), not a vacuous pass.
  # The relay believes it is spawning a batch the engine never issued.
  printf '[{"step":"STEP-1","instance":"check@0"}]\n' >"$ZK/proposed.json"
  zk_run guard spawn --run RUN-1 --rows "$ZK/proposed.json"
  assert_exit "ZK" "ZK_G4_rows_without_manifest_denied" 2

  # The denial's REASON rides the error channel under --json, which is where
  # §6.12 puts it.
  zk_run guard spawn --run RUN-1 --rows "$ZK/proposed.json" --json
  assert_json "ZK" "ZK_G5_denial_ok_false" ".ok" "false"

  # ---------------------------------------------------------------------------
  # GROUP-3 DORMANCY (§3): the read verb and the guards add NO OUTPUT to any
  # existing verb.
  # ---------------------------------------------------------------------------
  local ZK_SHOW_BEFORE ZK_SHOW_AFTER ZK_NEXT_BEFORE ZK_NEXT_AFTER
  zk_run issue show DKT-1 --json
  ZK_SHOW_BEFORE="$CMD_STDOUT"
  zk_run next --json
  ZK_NEXT_BEFORE="$CMD_STDOUT"

  zk_run events list --run RUN-1 --json >/dev/null
  zk_run guard record --run RUN-1 >/dev/null
  zk_run guard spawn --run RUN-1 >/dev/null

  zk_run issue show DKT-1 --json
  ZK_SHOW_AFTER="$CMD_STDOUT"
  zk_run next --json
  ZK_NEXT_AFTER="$CMD_STDOUT"

  if [ "$ZK_SHOW_BEFORE" = "$ZK_SHOW_AFTER" ] && [ "$ZK_NEXT_BEFORE" = "$ZK_NEXT_AFTER" ]; then
    check "ZK" "ZK_D1_read_verbs_change_nothing" "PASS"
  else
    check "ZK" "ZK_D1_read_verbs_change_nothing" "FAIL" \
      "an existing verb's output changed after a read verb ran"
  fi

  # ---------------------------------------------------------------------------
  # F17/F19 — A REPO WITH NO `.docket/config/` ACTIVATES BYTE-IDENTICALLY.
  #
  # This is the group-3 dormancy proof §3 names: the scan `stat`s the directory
  # once, and an absent one skips it in full. The comparison is against an
  # activation of the SAME workflow registered MANUALLY, so the only difference
  # between the two repos is where the definition came from.
  # ---------------------------------------------------------------------------
  local ZK_NOCFG ZK_NOCFG_OUT
  ZK_NOCFG=$(qa_mktemp_d)
  run_env "$ZK_NOCFG" init >/dev/null

  # The same bytes, registered by hand, with NO config directory anywhere.
  run_env "$ZK_NOCFG" workflow register "$ZK_WF" --json >/dev/null
  run_env "$ZK_NOCFG" issue create -t "Proof the sheets" -d "$ZK_BODY" --type task --json >/dev/null
  run_env "$ZK_NOCFG" run start --issue DKT-1 --json >/dev/null
  run_env "$ZK_NOCFG" run activate RUN-1 --json
  assert_exit "ZK" "ZK_F17_no_config_activates" 0
  ZK_NOCFG_OUT="$CMD_STDOUT"

  check_cond "ZK" "ZK_F17_no_config_dir" "the repo grew a config directory" [ ! -d "$ZK_NOCFG/config" ]

  # F23: NO `registered` KEY AT ALL — not an empty array. The dormancy is
  # visible in the output rather than merely true underneath it.
  local ZK_HAS_REGISTERED
  ZK_HAS_REGISTERED=$(printf '%s' "$ZK_NOCFG_OUT" | jq -r 'has("data") and (.data | has("registered"))')
  check_cond "ZK" "ZK_F23_no_registered_key" "a repo with no config directory emitted a registered key" [ "$ZK_HAS_REGISTERED" = "false" ]

  # AND THE ACTIVATION OUTPUT IS BYTE-IDENTICAL to a config-driven one, once the
  # `registered` block — the only thing this stage adds — and the run's own clock
  # readings are removed.
  #
  # THIS IS THE F19 COMPARISON, and it is made between two FRESH activations of
  # the same definition rather than against the rehearsal's run, which has since
  # reached `done`. What must match is what activation DID: how many issues it
  # bound, how many it expanded, how many steps and pins and fences it produced.
  # The scan changed WHERE the definition came from and nothing else, so any
  # other difference is this stage leaking into the dormant path.
  local ZK_CFG2 ZK_CFG_ACT ZK_NOCFG_ACT
  ZK_CFG2=$(qa_mktemp_d)
  run_env "$ZK_CFG2" init >/dev/null
  mkdir -p "$ZK_CFG2/config/workflows"
  cp "$ZK_WF" "$ZK_CFG2/config/workflows/standard-dev.toml"
  run_env "$ZK_CFG2" issue create -t "Proof the sheets" -d "$ZK_BODY" --type task --json >/dev/null
  run_env "$ZK_CFG2" run start --issue DKT-1 --json >/dev/null
  run_env "$ZK_CFG2" run activate RUN-1 --json
  ZK_CFG_ACT=$(printf '%s' "$CMD_STDOUT" | jq -S \
    'del(.data.registered, .data.pins_from_config, .data.run, .message)')

  ZK_NOCFG_ACT=$(printf '%s' "$ZK_NOCFG_OUT" | jq -S \
    'del(.data.registered, .data.pins_from_config, .data.run, .message)')

  check_cond "ZK" "ZK_F19_activation_shape_identical" "activation differs between a config-driven and a hand-registered repo beyond the registered block: $ZK_CFG_ACT vs $ZK_NOCFG_ACT" [ "$ZK_CFG_ACT" = "$ZK_NOCFG_ACT" ]
  rm -rf "$ZK_CFG2"

  # ---------------------------------------------------------------------------
  # Z12 — THE SCHEMA VARIANT. The rehearsal again with a config directory
  # holding BOTH a schema and a workflow that names it, proving F2's ordering
  # END-TO-END: activation SUCCEEDS, which it could not if the workflow
  # registered first.
  #
  # The workflow's filename sorts BEFORE the schema's, so a
  # lexical-across-everything order would register it first and hit §4.6 (a)'s
  # refusal. That the file is named `a-assess.toml` is the whole test.
  # ---------------------------------------------------------------------------
  local ZK_S
  ZK_S=$(qa_mktemp_d)
  run_env "$ZK_S" init >/dev/null
  mkdir -p "$ZK_S/config/workflows" "$ZK_S/config/schemas"

  cat >"$ZK_S/config/schemas/sheet@1.json" <<'JSON'
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "sheet@1",
  "type": "object",
  "properties": {
    "defects": { "type": "integer" }
  },
  "required": ["defects"]
}
JSON

  cat >"$ZK_S/config/workflows/a-assess.toml" <<'TOML'
[pipeline]
name        = "sheet-assess"
version     = 1
description = "Count the defects on a proof sheet."

[match]
kind = ["task"]

[[step]]
name     = "assess"
after    = []
executor = "proofreader"
emits    = "assessment"
payload  = "sheet@1"
TOML

  run_env "$ZK_S" issue create -t "Assess the proof" -d "count them" --type task --json >/dev/null
  run_env "$ZK_S" run start --issue DKT-1 --json >/dev/null
  run_env "$ZK_S" run activate RUN-1 --json
  assert_exit "ZK" "ZK_Z12_activate" 0

  # F2, END-TO-END: two registrations, THE SCHEMA FIRST. If the order were
  # lexical across everything, `a-assess.toml` would have registered before
  # `sheet@1.json` and been refused for naming an unregistered payload.
  assert_json "ZK" "ZK_Z12_registered_len" "(.data.registered | length)" "2"
  assert_json "ZK" "ZK_Z12_schema_first" ".data.registered[0].kind" "schema"
  # `name` and `version` are SEPARATE fields in F21's row, so the reference
  # `sheet@1` appears as its two parts rather than as one string — which is what
  # lets a consumer address the schema without re-parsing a ref.
  assert_json "ZK" "ZK_Z12_schema_name" ".data.registered[0].name" "sheet"
  assert_json "ZK" "ZK_Z12_schema_version" ".data.registered[0].version" "1"
  assert_json "ZK" "ZK_Z12_workflow_second" ".data.registered[1].kind" "workflow"

  # And no register verb was needed for EITHER of them.
  local ZK_S_SCHEMAS
  ZK_S_SCHEMAS=$(sqlite3 "$ZK_S/issues.db" \
    "SELECT COUNT(*) FROM schemas WHERE name = 'sheet' AND builtin = 0;")
  check_cond "ZK" "ZK_Z12_schema_registered" "$ZK_S_SCHEMAS rows for the auto-registered schema, want 1" [ "$ZK_S_SCHEMAS" = "1" ]

  # F9, over the schema variant: editing a registered file without bumping its
  # version refuses the NEXT activation, with the version-bump message.
  printf '\n# an edit nobody bumped for\n' >>"$ZK_S/config/workflows/a-assess.toml"
  run_env "$ZK_S" issue create -t "Second sheet" -d "count them too" --type task --json >/dev/null
  run_env "$ZK_S" run start --issue DKT-2 --json >/dev/null
  run_env "$ZK_S" run activate RUN-2 --json
  assert_exit "ZK" "ZK_F9_collision_exit" 4
  assert_json "ZK" "ZK_F9_collision_code" ".code" "CONFLICT"
  if printf '%s' "$CMD_STDOUT" | jq -r '.error' | grep -q "version to 2"; then
    check "ZK" "ZK_F10_names_the_edit" "PASS"
  else
    check "ZK" "ZK_F10_names_the_edit" "FAIL" \
      "the refusal does not name the literal edit to make"
  fi

  rm -rf "$ZK" "$ZK_NOCFG" "$ZK_S"
  unset -f zk_run
}
