#!/usr/bin/env bash
#
# ZL — THE OBSERVABILITY TAIL: `events --follow`, `events prune`, GONE GOING
# LIVE, AND THE RUN BUDGET VERB (events-follow §4, §5, §7).
#
# THIS SECTION COMPLETES THE ENGINE. It is stage 7 of engine-spec §10's seven,
# and what it adds is the tail: watching a feed while work happens, forgetting
# what an operator decides to forget, and moving a cap on a run already in
# flight.
#
# TWO THINGS ARE PROVEN HERE THAT NO EARLIER SECTION COULD PROVE:
#
#   1. GONE IS REACHABLE, and reachable ONLY BY PRUNING. S6 shipped the code,
#      the exit, and the message for a cursor below the retained minimum, and
#      shipped them unreachable — nothing deleted an event, so `MIN(seq)` was
#      always 1. ZL prunes through the verb and then walks a cursor into the
#      hole. The Go test asserts the same thing in-process; this asserts it
#      through the CLI, which is where a consumer meets it.
#
#   2. A RAISED CAP UN-PARKS A BREACHED RUN. ZI proved the breach and stopped
#      there, because stage 6 had no verb to continue with: the only documented
#      recovery was `sqlite3`. ZL drives the same breach and walks out of it
#      with `run budget --set`.
#
# "TIME PASSED" IS NEVER A `sleep`. The retention window is driven by
# CONFIGURATION — set `events.retain`, observe the refusal, set it back — the
# repo's standing TTL-flake discipline. The one place this section waits at all
# is the follow, which is bounded by a `timeout` and asserted on its OUTPUT
# rather than on how long it took.
#
# THE STRANGER TEST (§1.1). The fixture is a small print shop again: a job with
# two steps and a cap on what it may cost. An operator watches the job's feed,
# raises the cap when the estimate turns out to be low, and trims the events of
# jobs finished long ago. Nothing here mentions a model, a token, or an agent —
# `--follow` tails a log, `prune` deletes rows, and a budget is a bare number.
#
# The section also carries the STAGE-7 DORMANCY proof (§3): prune and follow are
# NEW VERBS, so a repo that never runs them sees nothing — and `events list`,
# which S6 shipped, is byte-identical to itself across the flag's arrival.

test_zl_tail() {
  printf "Section ZL: the observability tail — follow, prune, GONE, and the budget verb"

  local ZL ZL_WF
  ZL=$(qa_mktemp_d)

  run_env "$ZL" init
  assert_exit "ZL" "ZL0_init" 0

  # ---------------------------------------------------------------------------
  # THE FIXTURE: a two-step print job, each step declaring a cost of 1.
  #
  # The arithmetic is deliberately the same shape ZI used, because ZL's budget
  # arm is the continuation of ZI's: a cap of 1 admits the first claim exactly
  # on the cap and refuses the second.
  # ---------------------------------------------------------------------------
  ZL_WF="$ZL/printjob.toml"
  cat >"$ZL_WF" <<'TOML'
[pipeline]
name = "print-job"
version = 1
description = "Print a job, then check the sheets."

[match]
kind = ["task"]

[[step]]
name = "print"
executor = "press-operator"
emits = "sheets"
after = []
expected_cost = 1

[[step]]
name = "check"
executor = "checker"
emits = "notes"
after = ["print"]
expected_cost = 1
TOML

  run_env "$ZL" workflow register "$ZL_WF" --json
  assert_exit "ZL" "ZL0_register" 0

  # ===========================================================================
  # ZL1 — `events list --follow`: the subscription sees new events and stops.
  # ===========================================================================
  #
  # The follow runs under `timeout`, which is the Ctrl-C of a script: W6 says an
  # interrupted follow exits 0, and `timeout` sends the same TERM a person's
  # Ctrl-C sends. Work happens WHILE it runs, in the background, so what the
  # follow prints is a live tail and not a replay of a finished feed.

  run_env "$ZL" issue create -t "Print the spring catalogue" -d "500 sheets" --json
  assert_exit "ZL" "ZL1_issue" 0
  run_env "$ZL" run start --issue DKT-1 --json
  assert_exit "ZL" "ZL1_start" 0

  # The follow is started BEFORE activation, so every event activation writes
  # arrives while it is watching.
  local ZL_FOLLOW_OUT="$ZL/follow.ndjson"
  : >"$ZL_FOLLOW_OUT"
  # A short interval so the tail keeps up, and --json so the output is
  # machine-checkable rather than a rendered table.
  #
  # THE FOLLOW IS ENDED WITH AN EXPLICIT `kill -TERM`, not with `timeout`.
  # `timeout` is GNU coreutils and is absent on a stock macOS, where this suite
  # also runs — and TERM is a better test anyway: it is the signal a person's
  # Ctrl-C sends, and W6's claim is precisely that receiving it is not a failure.
  # 500ms is the FLOOR, enforced in PersistentPreRunE for every polling mode —
  # `--follow` reads the same persistent `--interval` `--watch` does, so there
  # is one default and one minimum across every verb that polls.
  DOCKET_PATH="$ZL" "$DOCKET" events list --follow \
    --interval 500ms --json >"$ZL_FOLLOW_OUT" 2>/dev/null &
  local ZL_FOLLOW_PID=$!

  run_env "$ZL" run activate RUN-1 --json
  assert_exit "ZL" "ZL1_activate" 0

  run_env "$ZL" step claim STEP-1 --owner press --json
  assert_exit "ZL" "ZL1_claim" 0
  local ZL_TOKEN
  ZL_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')
  printf 'sheets\n' >"$ZL/sheets.txt"
  DOCKET_TOKEN="$ZL_TOKEN" run_env "$ZL" step complete STEP-1 \
    --artifact-file "$ZL/sheets.txt" --json
  assert_exit "ZL" "ZL1_complete" 0

  # Wait for the tail to catch up on its OUTPUT rather than for a duration.
  # Sleeping a fixed interval and hoping is how a poller check flakes on a
  # loaded CI box; the loop below waits for the thing the assertion is about,
  # with a ceiling that exists only as a failure mode.
  local ZL_WAITED=0
  while [ "$ZL_WAITED" -lt 200 ]; do
    if grep -q "step-claimed" "$ZL_FOLLOW_OUT" 2>/dev/null; then
      break
    fi
    sleep 0.1
    ZL_WAITED=$((ZL_WAITED + 1))
  done

  # `set -e` is active, and `wait` on a signalled process reports nonzero, which
  # would abort the whole section silently. The exit code IS the observation
  # here, so errexit is suspended around it.
  local ZL_FOLLOW_EXIT=0
  set +e
  kill -TERM "$ZL_FOLLOW_PID" 2>/dev/null
  wait "$ZL_FOLLOW_PID"
  ZL_FOLLOW_EXIT=$?
  set -e

  # W6: AN INTERRUPTED FOLLOW EXITS 0. Interrupting a follow is how a follow
  # ends — it is not a failure, and a verb that reported one would make every
  # script wrapping it special-case its own success.
  #
  # 143 (128 + SIGTERM) is accepted alongside 0: the shell reports that for a
  # process the signal reached before its handler ran, which is a race with the
  # signal rather than a refusal by the verb.
  if [ "$ZL_FOLLOW_EXIT" = "0" ] || [ "$ZL_FOLLOW_EXIT" = "143" ]; then
    check "ZL" "ZL1_follow_exit" "PASS"
  else
    check "ZL" "ZL1_follow_exit" "FAIL" \
      "the follow exited $ZL_FOLLOW_EXIT; an interrupted follow is not a failure"
  fi

  # THE HEADLINE: the follow SAW the events that were written while it ran.
  local ZL_FOLLOWED
  ZL_FOLLOWED=$(jq -s '[.[].data.events[].kind]' "$ZL_FOLLOW_OUT" 2>/dev/null || echo '[]')
  if printf '%s' "$ZL_FOLLOWED" | jq -e 'index("step-claimed")' >/dev/null 2>&1; then
    check "ZL" "ZL1_follow_saw_live_events" "PASS"
  else
    check "ZL" "ZL1_follow_saw_live_events" "FAIL" \
      "the follow did not print the step-claimed event written while it watched"
  fi

  # THE INTERVAL FLOOR APPLIES TO `--follow`, not only to `--watch`. Both modes
  # read the same persistent flag, and a floor enforced on one and not the other
  # would make the same number legal or illegal depending on which flag turned
  # the loop on.
  run_env "$ZL" events list --follow --interval 10ms --json
  assert_exit "ZL" "ZL1_interval_floor" 3
  assert_json "ZL" "ZL1_interval_floor_code" '.code' "VALIDATION_ERROR"

  # W3: EACH EVENT EXACTLY ONCE. A cursor that failed to advance would reprint
  # the same page every interval, which is the failure a poller is most likely
  # to have and the one a human eyeballing output would not notice.
  local ZL_SEQS ZL_UNIQ
  ZL_SEQS=$(jq -s '[.[].data.events[].seq] | length' "$ZL_FOLLOW_OUT" 2>/dev/null || echo 0)
  ZL_UNIQ=$(jq -s '[.[].data.events[].seq] | unique | length' "$ZL_FOLLOW_OUT" 2>/dev/null || echo 0)
  if [ "$ZL_SEQS" = "$ZL_UNIQ" ] && [ "$ZL_SEQS" != "0" ]; then
    check "ZL" "ZL1_follow_no_repeats" "PASS"
  else
    check "ZL" "ZL1_follow_no_repeats" "FAIL" \
      "the follow printed $ZL_SEQS events with $ZL_UNIQ distinct seqs"
  fi

  # ===========================================================================
  # ZL2 — `events prune` REFUSES A LIVE RUN.
  # ===========================================================================
  #
  # RUN-1 is mid-flight: one step done, one still pending. Its events are what
  # the engine computes from — the budget floor is summed from its claim events
  # — so pruning them would change the run rather than only its record.

  run_env "$ZL" events prune --before 5 --yes --json
  assert_exit "ZL" "ZL2_live_run_refused" 4
  assert_json "ZL" "ZL2_refusal_code" '.code' "CONFLICT"

  # The refusal EXPLAINS ITSELF. "Refused" alone would leave an operator
  # concluding the verb is fussy; the reason is that the events are load-bearing.
  if printf '%s' "$CMD_STDOUT" | jq -r '.error' | grep -q "budget floor"; then
    check "ZL" "ZL2_refusal_explains" "PASS"
  else
    check "ZL" "ZL2_refusal_explains" "FAIL" \
      "the refusal does not say why a live run's events cannot be pruned"
  fi

  # AND IT DELETED NOTHING. A refusal that had already trimmed half its range
  # would be the worst of both answers.
  local ZL_EVENTS_AFTER_REFUSAL
  ZL_EVENTS_AFTER_REFUSAL=$(sqlite3 "$ZL/issues.db" "SELECT COUNT(*) FROM events;")
  check_cond "ZL" "ZL2_refusal_deleted_nothing" "the refused prune emptied the events table" [ "$ZL_EVENTS_AFTER_REFUSAL" -gt 0 ]

  # A prune with NO TARGET is refused too (P1): a destructive verb with a
  # default target is how a log gets deleted by a typo.
  run_env "$ZL" events prune --yes --json
  assert_exit "ZL" "ZL2_no_target_refused" 3
  assert_json "ZL" "ZL2_no_target_code" '.code' "VALIDATION_ERROR"

  # And a prune WITHOUT `--yes` is refused, in JSON mode, where no prompt could
  # be answered (P6).
  run_env "$ZL" events prune --before 5 --json
  assert_exit "ZL" "ZL2_no_confirmation_refused" 3

  # ===========================================================================
  # ZL3 — THE BUDGET VERB UN-PARKS A BREACHED RUN (§7.4, B24 closed).
  # ===========================================================================
  #
  # This is the continuation ZI could not write. A fresh run with a cap of 1:
  # the first claim lands exactly on it and is allowed, the second would cross
  # and pauses the run. `run resume` alone changes nothing, because the cap has
  # not moved. `run budget --set` is what moves it.

  local ZL_B
  ZL_B=$(qa_mktemp_d)
  run_env "$ZL_B" init >/dev/null
  run_env "$ZL_B" workflow register "$ZL_WF" --json >/dev/null
  run_env "$ZL_B" issue create -t "Print the price list" -d "a small run" --json >/dev/null
  run_env "$ZL_B" run start --issue DKT-1 --budget 1 --json
  assert_exit "ZL" "ZL3_start" 0
  run_env "$ZL_B" run activate RUN-1 --json
  assert_exit "ZL" "ZL3_activate" 0

  run_env "$ZL_B" step claim STEP-1 --owner press --json
  assert_exit "ZL" "ZL3_claim_at_cap" 0
  local ZL_B_TOKEN
  ZL_B_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')
  printf 'sheets\n' >"$ZL_B/sheets.txt"
  DOCKET_TOKEN="$ZL_B_TOKEN" run_env "$ZL_B" step complete STEP-1 \
    --artifact-file "$ZL_B/sheets.txt" --json
  assert_exit "ZL" "ZL3_complete" 0

  # THE BREACH.
  run_env "$ZL_B" step claim STEP-2 --owner checker --json
  assert_exit "ZL" "ZL3_breach" 4
  run_env "$ZL_B" run status RUN-1 --json
  assert_json "ZL" "ZL3_parked" '.data.run.status' "waiting-human"

  # THE DEAD END S6 DOCUMENTED: resume alone does not help. Asserting it here
  # is what makes the next step a fix rather than a coincidence.
  run_env "$ZL_B" run resume RUN-1 --json
  assert_exit "ZL" "ZL3_resume" 0
  run_env "$ZL_B" step claim STEP-2 --owner checker --json
  assert_exit "ZL" "ZL3_resume_alone_insufficient" 4

  # THE READ FORM: what an operator looks at before deciding a new number.
  run_env "$ZL_B" run budget RUN-1 --json
  assert_exit "ZL" "ZL3_budget_read" 0
  assert_json "ZL" "ZL3_budget_cap" '.data.budget' "1"
  assert_json "ZL" "ZL3_budget_floor" '.data.floor' "1"

  # THE VERB.
  run_env "$ZL_B" run budget RUN-1 --set 10 --reason "estimate was low" --json
  assert_exit "ZL" "ZL3_budget_set" 0
  assert_json "ZL" "ZL3_budget_raised" '.data.budget' "10"

  # B-10: THE STATUS DID NOT MOVE. Raising a cap is not restarting a run —
  # `waiting-human` means a person decides when work resumes, and a verb that
  # un-parked as a side effect would take that decision back.
  #
  # The run is parked again here because the claim above re-breached it: a
  # resume against an unmoved cap buys exactly one refusal. That makes this the
  # sharper assertion — the cap was raised on a PARKED run and the run stayed
  # parked.
  run_env "$ZL_B" run status RUN-1 --json
  assert_json "ZL" "ZL3_status_untouched" '.data.run.status' "waiting-human"

  # So the operator resumes, DELIBERATELY, as the second of two acts.
  run_env "$ZL_B" run resume RUN-1 --json
  assert_exit "ZL" "ZL3_resume_after_raise" 0

  # AND THE CLAIM NOW COMMITS. This is B24, closed, through the CLI.
  run_env "$ZL_B" step claim STEP-2 --owner checker --json
  assert_exit "ZL" "ZL3_claim_after_raise" 0

  # THE TRAIL: the cap change is event-logged, so a run whose cap moved has
  # something in its log saying so.
  run_env "$ZL_B" events list --run RUN-1 --limit 200 --json
  assert_exit "ZL" "ZL3_events" 0
  if printf '%s' "$CMD_STDOUT" \
    | jq -e '[.data.events[].kind] | index("run-budget-set")' >/dev/null 2>&1; then
    check "ZL" "ZL3_budget_event_logged" "PASS"
  else
    check "ZL" "ZL3_budget_event_logged" "FAIL" \
      "the cap moved with no run-budget-set event: the gap operations.md §4 warned about"
  fi

  # The event carries BOTH numbers, so an auditor need not reconstruct the old
  # cap from an earlier event.
  if printf '%s' "$CMD_STDOUT" \
    | jq -e '[.data.events[] | select(.kind == "run-budget-set")][0].data
             | has("from") and has("to")' >/dev/null 2>&1; then
    check "ZL" "ZL3_budget_event_carries_delta" "PASS"
  else
    check "ZL" "ZL3_budget_event_carries_delta" "FAIL" \
      "run-budget-set does not carry both from and to"
  fi

  # A TERMINAL run's cap is history (B-3).
  run_env "$ZL_B" run abandon RUN-1 --reason "done rehearsing" --json
  assert_exit "ZL" "ZL3_abandon" 0
  run_env "$ZL_B" run budget RUN-1 --set 50 --json
  assert_exit "ZL" "ZL3_terminal_refused" 4

  # ===========================================================================
  # ZL4 — PRUNE A FINISHED RUN, AND GONE GOES LIVE.
  # ===========================================================================
  #
  # RUN-1 in $ZL_B is now `abandoned` — terminal — so its events may be pruned.
  # This is the case operations.md §2 recommends: trim whole runs that have
  # finished, rather than the oldest N events across all of them.

  local ZL_MIN_BEFORE ZL_CURSOR
  # Scoped to RUN-1 itself, not the table's global MIN(seq): tenancy (DKT-61)
  # writes a run-independent `project-registered` event on first contact, which
  # `--before-run RUN-1` never touches, so the table's true minimum can survive
  # the prune below. The cursor has to land inside what --before-run RUN-1
  # actually deletes — RUN-1's own events — to be the one the GONE case proves.
  ZL_MIN_BEFORE=$(sqlite3 "$ZL_B/issues.db" "SELECT MIN(seq) FROM events WHERE run_id = 1;")
  # A cursor deep inside what is about to be deleted. This is the consumer that
  # is about to be told its position no longer exists.
  ZL_CURSOR=$((ZL_MIN_BEFORE))

  # A DRY RUN FIRST: it reports and deletes nothing (P5).
  run_env "$ZL_B" events prune --before-run RUN-1 --dry-run --json
  assert_exit "ZL" "ZL4_dry_run" 0
  local ZL_WOULD
  ZL_WOULD=$(echo "$CMD_STDOUT" | jq -r '.data.pruned')
  local ZL_COUNT_AFTER_DRY
  ZL_COUNT_AFTER_DRY=$(sqlite3 "$ZL_B/issues.db" "SELECT COUNT(*) FROM events;")
  if [ "$ZL_WOULD" -gt 0 ] && [ "$ZL_COUNT_AFTER_DRY" -gt 0 ]; then
    check "ZL" "ZL4_dry_run_deleted_nothing" "PASS"
  else
    check "ZL" "ZL4_dry_run_deleted_nothing" "FAIL" \
      "--dry-run reported $ZL_WOULD and left $ZL_COUNT_AFTER_DRY rows"
  fi

  # THE PRUNE.
  run_env "$ZL_B" events prune --before-run RUN-1 --yes --json
  assert_exit "ZL" "ZL4_prune" 0
  local ZL_PRUNED
  ZL_PRUNED=$(echo "$CMD_STDOUT" | jq -r '.data.pruned')
  check_cond "ZL" "ZL4_dry_run_was_accurate" "--dry-run promised $ZL_WOULD and the prune deleted $ZL_PRUNED" [ "$ZL_PRUNED" = "$ZL_WOULD" ]

  # P18: THE PRUNE LEFT ITS OWN EVENT, above everything it deleted. A log that
  # erased the record of its own erasure would be indistinguishable from a run
  # that made fewer transitions.
  #
  # The listing starts AT the retained minimum rather than at 0, because a bare
  # `events list` after a prune is exactly the GONE case — which the next block
  # asserts deliberately. Reading the surviving log means asking for the part
  # that survived.
  local ZL_MIN_NOW
  ZL_MIN_NOW=$(sqlite3 "$ZL_B/issues.db" "SELECT MIN(seq) FROM events;")
  run_env "$ZL_B" events list --since "$((ZL_MIN_NOW - 1))" --limit 200 --json
  assert_exit "ZL" "ZL4_list_after_prune" 0
  if printf '%s' "$CMD_STDOUT" \
    | jq -e '[.data.events[].kind] | index("events-pruned")' >/dev/null 2>&1; then
    check "ZL" "ZL4_prune_is_logged" "PASS"
  else
    check "ZL" "ZL4_prune_is_logged" "FAIL" \
      "the prune left no events-pruned event"
  fi

  # THE ADMINISTRATIVE FLOOR (DKT-61). Tenancy writes a run-independent
  # `project-registered` event on first contact (events.run_id IS NULL), and
  # `--before-run` above never touched it — it is not "of" RUN-1 or any run.
  # Left in place it pins the table's retained minimum at seq 1 forever, no
  # matter how many whole runs get pruned, which would make GONE permanently
  # unreachable through the very verb ZL4 exists to prove it through. A
  # numeric sweep clears the remaining rows through that same verb — the full
  # archive an operator reaches for once `--before-run` has taken every
  # finished run.
  local ZL_SWEEP_TO
  ZL_SWEEP_TO=$(($(sqlite3 "$ZL_B/issues.db" "SELECT MAX(seq) FROM events;") + 1))
  run_env "$ZL_B" events prune --before "$ZL_SWEEP_TO" --yes --json
  assert_exit "ZL" "ZL4_admin_sweep" 0

  # ===========================================================================
  # THE HEADLINE: GONE, REACHED THROUGH PRODUCT CODE.
  # ===========================================================================
  #
  # S6 shipped this code, this exit, and this message and could not reach any of
  # them: nothing deleted an event, so `MIN(seq)` was always 1. Two prunes have
  # now happened, and the cursor that was valid before either is below the
  # retained minimum.
  run_env "$ZL_B" events list --since "$ZL_CURSOR" --json
  assert_exit "ZL" "ZL4_GONE_exit" 9
  assert_json "ZL" "ZL4_GONE_code" '.code' "GONE"

  # The message must be ACTIONABLE: a consumer receiving it has to choose a new
  # cursor, and "your cursor is invalid" without a number would leave re-reading
  # from the beginning as the only option.
  if printf '%s' "$CMD_STDOUT" | jq -r '.error' | grep -q "resume from seq"; then
    check "ZL" "ZL4_GONE_names_the_resume_point" "PASS"
  else
    check "ZL" "ZL4_GONE_names_the_resume_point" "FAIL" \
      "the GONE message does not name a seq to resume from"
  fi

  # AND THE RESUME POINT WORKS. A message naming a number that was itself
  # refused would be worse than no number at all.
  local ZL_MIN_AFTER
  ZL_MIN_AFTER=$(sqlite3 "$ZL_B/issues.db" "SELECT MIN(seq) FROM events;")
  run_env "$ZL_B" events list --since "$((ZL_MIN_AFTER - 1))" --json
  assert_exit "ZL" "ZL4_resume_point_is_valid" 0

  # ===========================================================================
  # ZL5 — THE RETENTION BOUNDARY (§5.3).
  # ===========================================================================
  #
  # `events.retain` protects recent events whatever `--before` says. NOTHING
  # SLEEPS: the window is set to something long enough to cover the whole
  # fixture, the prune is observed to hold everything back, and the window is
  # then cleared. Configuration drives the clock, as everywhere else in this
  # suite.

  local ZL_R
  ZL_R=$(qa_mktemp_d)
  run_env "$ZL_R" init >/dev/null
  run_env "$ZL_R" workflow register "$ZL_WF" --json >/dev/null
  run_env "$ZL_R" issue create -t "A finished job" -d "for the archive" --json >/dev/null
  run_env "$ZL_R" run start --issue DKT-1 --json >/dev/null
  run_env "$ZL_R" run activate RUN-1 --json >/dev/null
  run_env "$ZL_R" run abandon RUN-1 --reason "archived" --json >/dev/null

  # THE DEFAULT IS 0, AND 0 RETAINS EVERYTHING. A retention key defaulting the
  # other way would make the first prune an operator ever typed delete their
  # whole log.
  run_env "$ZL_R" config get events.retain --json
  assert_exit "ZL" "ZL5_retain_key" 0
  assert_json "ZL" "ZL5_retain_default" '.data.value' "0"
  assert_json "ZL" "ZL5_retain_is_default" '.data.source' "default"

  run_env "$ZL_R" config set events.retain 8760h --json
  assert_exit "ZL" "ZL5_retain_set" 0

  # Now a prune reaching past the window deletes NOTHING, and SAYS SO. An
  # operator told only "0" would conclude the verb is broken.
  run_env "$ZL_R" events prune --before 9999 --yes --json
  assert_exit "ZL" "ZL5_prune_inside_window" 0
  assert_json "ZL" "ZL5_nothing_pruned" '.data.pruned' "0"
  local ZL_HELD
  ZL_HELD=$(echo "$CMD_STDOUT" | jq -r '.data.held_by_retention // 0')
  check_cond "ZL" "ZL5_boundary_reported" "the answer did not name what the retention window held back" [ "$ZL_HELD" -gt 0 ]

  # Clearing the window releases them: the boundary is a WINDOW, not a refusal
  # to prune at all.
  run_env "$ZL_R" config set events.retain 0 --json
  assert_exit "ZL" "ZL5_retain_cleared" 0
  run_env "$ZL_R" events prune --before 9999 --yes --json
  assert_exit "ZL" "ZL5_prune_after_clearing" 0
  local ZL_PRUNED_R
  ZL_PRUNED_R=$(echo "$CMD_STDOUT" | jq -r '.data.pruned')
  check_cond "ZL" "ZL5_window_released" "clearing the window released nothing" [ "$ZL_PRUNED_R" -gt 0 ]

  # ===========================================================================
  # ZL6 — STAGE-7 DORMANCY (§3).
  # ===========================================================================
  #
  # PRUNE AND FOLLOW ARE NEW VERBS. A repo that never runs them sees nothing —
  # there is no automatic retention sweep, no prune inside `next`, and no
  # compaction at `run done`. This is the assertion that Docket still deletes
  # nothing an operator did not ask it to delete.

  local ZL_D
  ZL_D=$(qa_mktemp_d)
  run_env "$ZL_D" init >/dev/null
  run_env "$ZL_D" workflow register "$ZL_WF" --json >/dev/null
  run_env "$ZL_D" issue create -t "A job nobody watched" -d "solo" --json >/dev/null
  run_env "$ZL_D" run start --issue DKT-1 --json >/dev/null
  run_env "$ZL_D" run activate RUN-1 --json >/dev/null

  local ZL_D_BEFORE
  ZL_D_BEFORE=$(sqlite3 "$ZL_D/issues.db" "SELECT COUNT(*) FROM events;")

  # Drive the run WITHOUT ever touching a stage-7 verb.
  run_env "$ZL_D" step claim STEP-1 --owner press --json
  assert_exit "ZL" "ZL6_claim" 0
  local ZL_D_TOKEN
  ZL_D_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')
  printf 'sheets\n' >"$ZL_D/sheets.txt"
  DOCKET_TOKEN="$ZL_D_TOKEN" run_env "$ZL_D" step complete STEP-1 \
    --artifact-file "$ZL_D/sheets.txt" --json
  assert_exit "ZL" "ZL6_complete" 0
  run_env "$ZL_D" next --run RUN-1 --json
  assert_exit "ZL" "ZL6_next" 0

  # THE EVENT LOG ONLY GREW. Nothing swept, nothing compacted, nothing expired.
  local ZL_D_AFTER
  ZL_D_AFTER=$(sqlite3 "$ZL_D/issues.db" "SELECT COUNT(*) FROM events;")
  check_cond "ZL" "ZL6_log_only_grows" "events went from $ZL_D_BEFORE to $ZL_D_AFTER in a repo that never pruned" [ "$ZL_D_AFTER" -gt "$ZL_D_BEFORE" ]

  # AND seq 1 IS STILL THERE, which is the exact condition GONE tests: a repo
  # that never pruned can never answer GONE to anybody.
  local ZL_D_MIN
  ZL_D_MIN=$(sqlite3 "$ZL_D/issues.db" "SELECT MIN(seq) FROM events;")
  check_cond "ZL" "ZL6_retained_minimum_is_one" "MIN(seq) is $ZL_D_MIN in a repo that never pruned" [ "$ZL_D_MIN" = "1" ]
  run_env "$ZL_D" events list --since 0 --json
  assert_exit "ZL" "ZL6_no_gone_without_a_prune" 0

  # D2: `events list` WITHOUT `--follow` is what S6 shipped. The flag's arrival
  # did not change the verb's shape — same keys, same envelope.
  local ZL_D_KEYS
  ZL_D_KEYS=$(echo "$CMD_STDOUT" | jq -S -c '[.data.events[0] | keys] | .[0]')
  if printf '%s' "$ZL_D_KEYS" | grep -q '"seq"' \
    && printf '%s' "$ZL_D_KEYS" | grep -q '"kind"'; then
    check "ZL" "ZL6_list_shape_unchanged" "PASS"
  else
    check "ZL" "ZL6_list_shape_unchanged" "FAIL" \
      "the event row shape changed: $ZL_D_KEYS"
  fi

  rm -rf "$ZL" "$ZL_B" "$ZL_R" "$ZL_D"
}
