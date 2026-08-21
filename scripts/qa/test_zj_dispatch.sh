#!/usr/bin/env bash
#
# ZJ — DISPATCH, `next`'s REFUSAL, AND THE WRITE-REAP ACKNOWLEDGMENT
# (runs-dispatch §5, §6, §9.2).
#
# TWO HEADLINES, both engine-spec §9 acceptance criteria, verbatim:
#
#   9. Dispatch recovery: kill the relay with a dispatch open — TTL auto-abandon
#      (or an explicit `dispatch abandon`) restores `next`; nothing is lost or
#      double-executed.
#
#  10. (second half) a reaped write-class step cannot gain a successor until the
#      reap is acknowledged.
#
# Item 9 is executed LITERALLY, BOTH ARMS: arm A recovers through the TTL, arm B
# through the explicit verb. Item 10's gate half was proven at S4 (gates-trust
# §9.2); the reap half is here, which completes it.
#
# "THE CLOCK ADVANCED" IS NOT A `sleep` (§5.9). The section sets `dispatch.ttl`
# and the lease TTL to small values through `docket config` and drives the clock
# by CONFIGURATION rather than by waiting — following the repo's existing
# TTL-flake discipline. Nothing here sleeps.
#
# THE STRANGER TEST (§1.1). The fixture is a print shop: two press operators who
# must not run the press at the same time, and two proofreaders who may work in
# parallel. `[limits] press = { max = 1 }` is the whole of the serialization, and
# what "write-class" means to core is exactly "a class the author bounded". No
# model, no agent, no token — a batch dispatcher is a person or a script that
# starts workers, and this section reads as a shop-floor recovery drill.
#
# The section also carries the GROUP-2 DORMANCY proof (§3): a run that never
# opens a dispatch, and never reaps a bounded class, behaves exactly as group 1's.

test_zj_dispatch() {
  printf "Section ZJ: Dispatch, next's refusal, and the write-reap ack"

  local ZJ ZJ_WF
  ZJ=$(qa_mktemp_d)

  run_env "$ZJ" init
  assert_exit "ZJ" "ZJ0_init" 0

  # ---------------------------------------------------------------------------
  # THE FIXTURE: a print shop.
  #
  # `press` is bounded at 1 — that is the instance policy engine-spec §2 says
  # serialization IS, and it is the only thing that makes a class "write-class"
  # to core (§6.5 A20). `proof` is unbounded, so it fans out freely and A6's
  # narrowness has something to be narrow about.
  # ---------------------------------------------------------------------------
  ZJ_WF="$ZJ/printshop.toml"
  cat >"$ZJ_WF" <<'TOML'
[pipeline]
name = "print-shop"
version = 1
description = "Run a job on the press, then have two people proof the sheets."

[limits]
press = { max = 1 }

[match]
kind = ["task"]

[[step]]
name = "print-run"
executor = "press-operator"
class = "press"
emits = "sheets"
after = []

[[step]]
name = "reprint"
executor = "press-operator"
class = "press"
emits = "sheets"
after = ["print-run"]

[[step]]
name = "proof-copy"
executor = "proofreader"
class = "proof"
emits = "notes"
after = []

[[step]]
name = "proof-colour"
executor = "proofreader"
class = "proof"
emits = "notes"
after = []
TOML

  run_env "$ZJ" workflow register "$ZJ_WF" --json
  assert_exit "ZJ" "ZJ0_register" 0
  assert_json "ZJ" "ZJ0_register_name" '.data.name' "print-shop"

  # The clock is driven by CONFIGURATION rather than by waiting, and the TTL is
  # moved between the arms rather than set once: K2 needs a manifest that is
  # still LIVE (so the refusal is the thing under test), and K3 needs one that
  # has lapsed. A single short value would make K2's refusal race the very
  # auto-abandon K3 is about.
  run_env "$ZJ" config set dispatch.ttl 100h --json
  assert_exit "ZJ" "ZJ0_ttl_set" 0

  # `dispatch.grace` is long throughout: D1 is exercised by the Go tests at its
  # boundary, and a short grace here would make every claimed step in this
  # section a discrepancy, refusing `next` for a reason no arm is testing.
  run_env "$ZJ" config set dispatch.grace 100h --json
  assert_exit "ZJ" "ZJ0_grace_set" 0

  # ===========================================================================
  # §9 ITEM 9, ARM A — TTL auto-abandon restores `next`
  # ===========================================================================

  run_env "$ZJ" issue create -t "Print the spring catalogue" -d "500 copies" --json
  assert_exit "ZJ" "ZJ1_issue" 0
  run_env "$ZJ" run start --issue DKT-1 --json
  assert_exit "ZJ" "ZJ1_start" 0
  run_env "$ZJ" run activate RUN-1 --json
  assert_exit "ZJ" "ZJ1_activate" 0

  # --- K1: open a dispatch over a real ready set; `verify` passes. -----------
  run_env "$ZJ" dispatch open --run RUN-1 --json
  assert_exit "ZJ" "ZJ1_K1_open" 0
  assert_json "ZJ" "ZJ1_K1_dispatch" '.data.dispatch' "DISPATCH-1"
  assert_json "ZJ" "ZJ1_K1_run" '.data.run' "RUN-1"
  assert_json_array_min "ZJ" "ZJ1_K1_rows" '.data.rows' 1

  # Record what the manifest offered, so K5's "nothing is lost" compares
  # against the manifest rather than against a re-derivation.
  local ZJ_MANIFEST
  ZJ_MANIFEST=$(echo "$CMD_STDOUT" | jq -c '[.data.rows[].instance] | sort')

  run_env "$ZJ" dispatch verify --run RUN-1 --json
  assert_exit "ZJ" "ZJ1_K1_verify" 0
  assert_json "ZJ" "ZJ1_K1_verified" '.data.verified' "true"

  # --- K2: `next --run` is REFUSED, and the reason names the dispatch. ------
  #
  # P26: a CONFLICT, not an empty list. A relay cannot tell "nothing to do"
  # from "I will not answer" if both arrive as a zero-length array.
  run_env "$ZJ" next --run RUN-1 --json
  assert_exit "ZJ" "ZJ1_K2_refused" 4
  assert_json "ZJ" "ZJ1_K2_code" '.code' "CONFLICT"
  assert_stdout_contains "ZJ" "ZJ1_K2_names_dispatch" "DISPATCH-1"
  assert_stdout_contains "ZJ" "ZJ1_K2_names_close" "dispatch close"
  assert_stdout_contains "ZJ" "ZJ1_K2_names_abandon" "dispatch abandon"
  assert_stdout_contains "ZJ" "ZJ1_K2_names_ttl" "TTL"

  # --- K3: past the TTL, `next` auto-abandons AND answers in ONE call. ------
  #
  # THE CRASH IS SIMULATED BY THE TTL, not by sleeping (§5.9).
  #
  # A manifest's expiry is PINNED AT OPEN TIME — `expires_ms` is a stored column
  # (P12), so shortening `dispatch.ttl` afterward does not retroactively expire
  # a live manifest. That is the correct behavior and it shapes this arm: the
  # K1/K2 manifest is retired explicitly, the TTL is shortened, and a FRESH
  # manifest is opened which is born already past its expiry. That fresh
  # manifest is the crashed relay's.
  run_env "$ZJ" dispatch abandon --run RUN-1 --reason "end of the K1/K2 arm" --json
  assert_exit "ZJ" "ZJ1_K3_retire_live_manifest" 0

  run_env "$ZJ" config set dispatch.ttl 1ms --json
  assert_exit "ZJ" "ZJ1_K3_ttl_short" 0

  # The staged closure raised a manifest's expiry floor to the SUM of its
  # stages' lease TTLs (stagedLeaseSumMS) — a doomed manifest needs the lease
  # TTL shortened too, or the floor keeps it alive past the 1ms dispatch.ttl.
  # Restored right after the doomed open: the expiry is pinned at open time
  # (P12), and later arms claim under the ordinary lease.
  run_env "$ZJ" config set lease.ttl.default 1ms --json
  assert_exit "ZJ" "ZJ1_K3_lease_short" 0

  run_env "$ZJ" dispatch open --run RUN-1 --json
  assert_exit "ZJ" "ZJ1_K3_open_doomed" 0
  local ZJ_DOOMED
  ZJ_DOOMED=$(echo "$CMD_STDOUT" | jq -r '.data.dispatch')

  run_env "$ZJ" config set lease.ttl.default 15m --json
  assert_exit "ZJ" "ZJ1_K3_lease_restored" 0

  # P16 is the assertion that matters: the relay that crashed and came back
  # does not poll twice. ONE invocation abandons the expired manifest AND
  # returns the ready set.
  run_env "$ZJ" next --run RUN-1 --json
  assert_exit "ZJ" "ZJ1_K3_answers" 0
  assert_json_array_min "ZJ" "ZJ1_K3_same_invocation" '.data.steps' 1

  # The dispatch is `abandoned` with reason=ttl, and the abandon is EVENT-LOGGED
  # — which engine-spec §2 requires by name ("event-logged").
  local ZJ_STATUS ZJ_REASON ZJ_DOOMED_ID
  ZJ_DOOMED_ID=${ZJ_DOOMED#DISPATCH-}
  ZJ_STATUS=$(sqlite3 "$ZJ/issues.db" "SELECT status FROM dispatches WHERE id = $ZJ_DOOMED_ID;")
  ZJ_REASON=$(sqlite3 "$ZJ/issues.db" "SELECT close_reason FROM dispatches WHERE id = $ZJ_DOOMED_ID;")
  if [ "$ZJ_STATUS" = "abandoned" ] && [ "$ZJ_REASON" = "ttl" ]; then
    check "ZJ" "ZJ1_K3_abandoned_ttl" "PASS"
  else
    check "ZJ" "ZJ1_K3_abandoned_ttl" "FAIL" \
      "dispatch 1 is status=$ZJ_STATUS reason=$ZJ_REASON, want abandoned/ttl"
  fi

  local ZJ_EV
  ZJ_EV=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM events WHERE kind = 'dispatch-abandoned' AND data LIKE '%\"reason\":\"ttl\"%';")
  check_cond "ZJ" "ZJ1_K3_event" "expected one dispatch-abandoned event carrying reason=ttl, found $ZJ_EV" [ "$ZJ_EV" = "1" ]

  # The TTL abandon and the operator's abandon are DISTINGUISHABLE in the feed:
  # exactly one carries reason=ttl, and the explicit one carries its own. An
  # operator reading the trail can tell a manifest that expired from one
  # somebody retired, which is the point of carrying the reason at all.
  local ZJ_EV_OP
  ZJ_EV_OP=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM events WHERE kind = 'dispatch-abandoned' AND data LIKE '%\"reason\":\"abandoned\"%';")
  check_cond "ZJ" "ZJ1_K3_reasons_distinguishable" "expected one operator-abandoned event, found $ZJ_EV_OP" [ "$ZJ_EV_OP" = "1" ]

  # --- K5 (arm A): NOTHING IS LOST. -----------------------------------------
  #
  # The ready set after recovery contains every step the manifest did, with the
  # same instances. Opening a dispatch never claimed anything, so recovery
  # returns the steps intact rather than reconstructing them.
  local ZJ_AFTER
  ZJ_AFTER=$(echo "$CMD_STDOUT" | jq -c '[.data.steps[].instance] | sort')
  check_cond "ZJ" "ZJ1_K5_nothing_lost" "manifest was $ZJ_MANIFEST; after recovery $ZJ_AFTER" [ "$ZJ_AFTER" = "$ZJ_MANIFEST" ]

  # ===========================================================================
  # §9 ITEM 9, ARM B — the EXPLICIT `dispatch abandon` restores `next`
  # ===========================================================================
  #
  # K4 runs from K2's state: a dispatch open, `next` refusing. The TTL is
  # lengthened first so the abandon is unambiguously the thing that recovered
  # the run rather than an expiry that happened to fire.

  run_env "$ZJ" config set dispatch.ttl 100h --json
  assert_exit "ZJ" "ZJ2_ttl_long" 0

  run_env "$ZJ" dispatch open --run RUN-1 --json
  assert_exit "ZJ" "ZJ2_open" 0
  run_env "$ZJ" next --run RUN-1 --json
  assert_exit "ZJ" "ZJ2_refused" 4

  # --- K4: abandon with a reason; `next` answers immediately. ---------------
  run_env "$ZJ" dispatch abandon --run RUN-1 --reason "relay died" --json
  assert_exit "ZJ" "ZJ2_K4_abandon" 0
  assert_json "ZJ" "ZJ2_K4_status" '.data.status' "abandoned"

  run_env "$ZJ" next --run RUN-1 --json
  assert_exit "ZJ" "ZJ2_K4_next_answers" 0
  assert_json_array_min "ZJ" "ZJ2_K4_rows" '.data.steps' 1

  # The operator's free text rides in the EVENT's opaque data, never in the
  # engine's short `close_reason` vocabulary — so `close_reason` stays a value
  # every read verb can render.
  local ZJ_DETAIL
  ZJ_DETAIL=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM events WHERE kind = 'dispatch-abandoned' AND data LIKE '%relay died%';")
  check_cond "ZJ" "ZJ2_K4_reason_recorded" "expected the operator's reason in one event, found $ZJ_DETAIL" [ "$ZJ_DETAIL" = "1" ]

  # ===========================================================================
  # K6 / K7 — nothing double-executed, and a pre-crash claim finishes
  # ===========================================================================

  # --- K7: a step claimed BEFORE the crash completes normally afterward. ----
  #
  # This is P28 as a drill: the dispatch is open, an executor claims anyway
  # (a dispatch is not a lock), the relay "dies", and the executor finishes.
  run_env "$ZJ" dispatch open --run RUN-1 --json
  assert_exit "ZJ" "ZJ3_open" 0

  run_env "$ZJ" step claim STEP-1 --owner press-a --json
  assert_exit "ZJ" "ZJ3_K7_claim_under_open_dispatch" 0
  local ZJ_TOKEN
  ZJ_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  # The relay dies; an operator retires its manifest.
  run_env "$ZJ" dispatch abandon --run RUN-1 --reason "relay died mid-batch" --json
  assert_exit "ZJ" "ZJ3_K7_abandon" 0

  # The executor finishes. Its usage is recorded, because a relay that
  # dispatches is accountable for it (§5.8 D2).
  printf 'the printed sheets\n' >"$ZJ/sheets.txt"
  DOCKET_TOKEN="$ZJ_TOKEN" run_env "$ZJ" step complete STEP-1 \
    --artifact-file "$ZJ/sheets.txt" --usage '{"sheets":500}' --json
  assert_exit "ZJ" "ZJ3_K7_completes" 0

  # Its artifact is recorded EXACTLY ONCE — the "nothing is lost, nothing is
  # duplicated" half of K7.
  #
  # The count is grouped by (step, KIND) rather than by step alone, because a
  # step legitimately carries more than one artifact: the one it emitted, plus
  # the engine-computed `issue.diff` that engine-spec §11.1 snapshots when its
  # producing step completed. Grouping by step alone would report that pair as a
  # duplicate — which would be this assertion misreading the schema rather than
  # catching a double execution.
  #
  # It is taken across the WHOLE RUN rather than for a hard-coded id: three
  # dispatches were opened and abandoned above, and what matters is that not one
  # of them caused a second recording anywhere.
  local ZJ_ART
  ZJ_ART=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM (SELECT a.step_id, a.kind FROM artifacts a
       JOIN steps s ON s.id = a.step_id WHERE s.run_id = 1
       GROUP BY a.step_id, a.kind HAVING COUNT(*) > 1);")
  check_cond "ZJ" "ZJ3_K7_one_artifact" "$ZJ_ART (step, kind) pair(s) were recorded twice across the recovery drill" [ "$ZJ_ART" = "0" ]

  # --- K6: NOTHING DOUBLE-EXECUTED, across the whole scenario. --------------
  #
  # Counted from `step-claimed` events PER STEP, which is the only record that
  # could show a step executed twice. Every step must have been claimed at most
  # once: three dispatches were opened and abandoned across this section, and
  # not one of them caused a second claim.
  local ZJ_DOUBLE
  ZJ_DOUBLE=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM (SELECT step_id FROM events WHERE kind = 'step-claimed'
      GROUP BY step_id HAVING COUNT(*) > 1);")
  check_cond "ZJ" "ZJ3_K6_nothing_double_executed" "$ZJ_DOUBLE step(s) were claimed more than once" [ "$ZJ_DOUBLE" = "0" ]

  # ===========================================================================
  # §9 ITEM 10's REAP HALF — W1-W9 minus the guard-spawn rows
  # ===========================================================================
  #
  # W4 and W6 are adapted to `dispatch open --ack-reap`, this group's entry
  # point (§11's group-2 row); group 3 adds the `guard spawn` rows.
  #
  # A fresh run, so the hold is the only thing in play.

  run_env "$ZJ" config set lease.ttl.press 1ms --json
  assert_exit "ZJ" "ZJ4_lease_short" 0

  run_env "$ZJ" issue create -t "Print the summer catalogue" -d "300 copies" --json
  assert_exit "ZJ" "ZJ4_issue" 0
  run_env "$ZJ" run start --issue DKT-2 --json
  assert_exit "ZJ" "ZJ4_start" 0
  run_env "$ZJ" run activate RUN-2 --json
  assert_exit "ZJ" "ZJ4_activate" 0

  # --- W1: the baseline. Both press steps exist; the press is bounded at 1. --
  run_env "$ZJ" next --run RUN-2 --json
  assert_exit "ZJ" "ZJ4_W1_baseline" 0

  # One CLAIMABLE press row — the successor legitimately rides `staged` at a
  # later stage (the class's one slot holds per stage, not per offer).
  local ZJ_PRESS
  ZJ_PRESS=$(echo "$CMD_STDOUT" \
    | jq -r '[.data.steps[] | select(.class == "press" and .status == "ready")] | length')
  check_cond "ZJ" "ZJ4_W1_one_press_step" "$ZJ_PRESS ready press-class steps offered at baseline, want 1" [ "$ZJ_PRESS" = "1" ]

  # --- W2: claim the press step, let its lease lapse, and reap it. ----------
  local ZJ_PRESS_STEP
  ZJ_PRESS_STEP=$(echo "$CMD_STDOUT" \
    | jq -r '[.data.steps[] | select(.class == "press")][0].step')

  run_env "$ZJ" step claim "$ZJ_PRESS_STEP" --owner press-b --json
  assert_exit "ZJ" "ZJ4_W2_claim" 0

  # `lease.ttl.press` is 1ms, so the lease has lapsed by the time this runs.
  run_env "$ZJ" next --run RUN-2 --json
  assert_exit "ZJ" "ZJ4_W2_reap" 0

  # The reap is event-logged AND leaves an UNACKNOWLEDGED ack row (A16, A19).
  local ZJ_REAPED ZJ_ACKS
  ZJ_REAPED=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM events WHERE run_id = 2 AND kind = 'lease-reaped';")
  ZJ_ACKS=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM reap_acks WHERE run_id = 2 AND acked_at_ms IS NULL;")
  if [ "$ZJ_REAPED" -ge 1 ] && [ "$ZJ_ACKS" = "1" ]; then
    check "ZJ" "ZJ4_W2_unacknowledged_row" "PASS"
  else
    check "ZJ" "ZJ4_W2_unacknowledged_row" "FAIL" \
      "$ZJ_REAPED lease-reaped events and $ZJ_ACKS unacknowledged reaps, want >=1 and 1"
  fi

  # The ack row names the reaped step's CLASS — whatever the author called it.
  # Core never read the string; what made this class hold is `max = 1`.
  local ZJ_CLASS
  ZJ_CLASS=$(sqlite3 "$ZJ/issues.db" "SELECT class FROM reap_acks WHERE run_id = 2;")
  check_cond "ZJ" "ZJ4_W2_class_is_the_authors" "the ack row names class '$ZJ_CLASS', want the workflow's own 'press'" [ "$ZJ_CLASS" = "press" ]

  # --- W3: the reaped step is re-offered; its SUCCESSOR is not. -------------
  #
  # The hold occupies the class's one slot. The reaped step itself returns to
  # the pool — re-offering it to a claimant that redoes the work is the retry
  # the lease mechanism exists for — while any OTHER press step is withheld.
  run_env "$ZJ" next --run RUN-2 --json
  assert_exit "ZJ" "ZJ4_W3_next" 0

  local ZJ_OFFERED_PRESS
  ZJ_OFFERED_PRESS=$(echo "$CMD_STDOUT" \
    | jq -r "[.data.steps[] | select(.class == \"press\") | .step] | sort | join(\",\")")
  check_cond "ZJ" "ZJ4_W3_only_the_reaped_step" "press steps offered: [$ZJ_OFFERED_PRESS]; want only the reaped $ZJ_PRESS_STEP" [ "$ZJ_OFFERED_PRESS" = "$ZJ_PRESS_STEP" ]

  # --- W5 / A6: read-class steps in the SAME run are still offered. ---------
  #
  # The hold is NARROW: it holds press headroom, not the whole run. A reaped
  # writer must not stop a fan-out that never touches the press.
  local ZJ_PROOF
  ZJ_PROOF=$(echo "$CMD_STDOUT" | jq -r '[.data.steps[] | select(.class == "proof")] | length')
  check_cond "ZJ" "ZJ4_W5_unbounded_class_flows" "$ZJ_PROOF proof-class steps offered under a press hold, want 2" [ "$ZJ_PROOF" = "2" ]

  # A11's discoverability: the denial NAMES the seq and the flag, on stderr.
  # A headroom denial with nothing running is otherwise baffling (§6.3), and an
  # operator should be able to copy the next command out of the message.
  #
  # It is asserted in HUMAN MODE, because the hold rides on stderr exactly where
  # the reap notice already does — and `Info` is suppressed under `--json` by
  # the writer's existing contract, so a JSON consumer's payload does not change
  # because a run happens to be holding headroom. Both halves are deliberate:
  # the operator gets the sentence, the program gets an unchanged shape.
  run_env "$ZJ" next --run RUN-2
  assert_exit "ZJ" "ZJ4_W3_human_mode" 0

  # The needle is matched with an explicit `-e`, not through
  # assert_stderr_contains: that helper passes the needle positionally to grep,
  # and a needle beginning with `--` is read as a grep OPTION rather than as a
  # pattern. Working around it here keeps a shared helper's behavior unchanged.
  if printf '%s' "$CMD_STDERR" | grep -qF -e "--ack-reap"; then
    check "ZJ" "ZJ4_W3_names_the_flag" "PASS"
  else
    check "ZJ" "ZJ4_W3_names_the_flag" "FAIL" \
      "the hold's stderr line does not name the flag that clears it"
  fi
  assert_stderr_contains "ZJ" "ZJ4_W3_names_the_instance" "print-run@0"

  # --- W9: an ack of a bogus seq is refused (A9 — the forgery point). -------
  run_env "$ZJ" dispatch open --run RUN-2 --ack-reap 999999 --json
  assert_exit "ZJ" "ZJ4_W9_bogus_refused" 3
  assert_json "ZJ" "ZJ4_W9_code" '.code' "VALIDATION_ERROR"
  assert_stdout_contains "ZJ" "ZJ4_W9_names_seq" "999999"

  # A refused ack acknowledges NOTHING: the row is still open.
  ZJ_ACKS=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM reap_acks WHERE run_id = 2 AND acked_at_ms IS NULL;")
  check_cond "ZJ" "ZJ4_W9_nothing_acknowledged" "$ZJ_ACKS unacknowledged reaps after a refused ack, want 1" [ "$ZJ_ACKS" = "1" ]

  # --- W6: acknowledge through `dispatch open --ack-reap`. -----------------
  #
  # This is the NEW-RELAY path: the session that takes over confirms the press
  # is idle — its own business, since core cannot check a process it did not
  # start — and acknowledges as part of claiming its next batch.
  local ZJ_SEQ
  ZJ_SEQ=$(sqlite3 "$ZJ/issues.db" "SELECT reaped_seq FROM reap_acks WHERE run_id = 2;")

  run_env "$ZJ" dispatch open --run RUN-2 --ack-reap "$ZJ_SEQ" --json
  assert_exit "ZJ" "ZJ4_W6_ack" 0

  local ZJ_ACKED_BY
  ZJ_ACKED_BY=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COALESCE(acked_by, '') FROM reap_acks WHERE run_id = 2;")
  check_cond "ZJ" "ZJ4_W6_acked_by_the_verb" "acked_by = '$ZJ_ACKED_BY', want 'dispatch-open' — the VERB, never an identity" [ "$ZJ_ACKED_BY" = "dispatch-open" ]

  # A3: the release is ATTRIBUTABLE. §9 item 2 requires every transition to be
  # traceable, and a successor becoming claimable IS a transition.
  local ZJ_ACK_EV
  ZJ_ACK_EV=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM events WHERE run_id = 2 AND kind = 'reap-acknowledged';")
  check_cond "ZJ" "ZJ4_W6_ack_event" "$ZJ_ACK_EV reap-acknowledged events, want 1" [ "$ZJ_ACK_EV" = "1" ]

  # --- W8: a SECOND ack of the same seq succeeds and changes nothing. -------
  run_env "$ZJ" dispatch abandon --run RUN-2 --json
  assert_exit "ZJ" "ZJ4_W8_clear_dispatch" 0
  run_env "$ZJ" dispatch open --run RUN-2 --ack-reap "$ZJ_SEQ" --json
  assert_exit "ZJ" "ZJ4_W8_second_ack_succeeds" 0

  ZJ_ACK_EV=$(sqlite3 "$ZJ/issues.db" \
    "SELECT COUNT(*) FROM events WHERE run_id = 2 AND kind = 'reap-acknowledged';")
  check_cond "ZJ" "ZJ4_W8_idempotent" "a second ack produced $ZJ_ACK_EV events, want the original 1 — A10 makes it a no-op" [ "$ZJ_ACK_EV" = "1" ]

  # --- W7: write-class steps flow again. -----------------------------------
  run_env "$ZJ" dispatch abandon --run RUN-2 --json
  assert_exit "ZJ" "ZJ4_W7_clear" 0

  # The lease goes back to a workable length first. `lease.ttl.press` was 1ms so
  # that W2's reap needed no waiting; a 1ms lease also lapses before any
  # claimant can complete against it, which would make this completion a
  # STALE_LEASE about the clock rather than a statement about the hold.
  run_env "$ZJ" config set lease.ttl.press 100h --json
  assert_exit "ZJ" "ZJ4_W7_lease_workable" 0

  # The reaped step completes, so its successor's predecessor is satisfied and
  # the hold is the only thing that could still withhold it.
  run_env "$ZJ" step claim "$ZJ_PRESS_STEP" --owner press-c --json
  assert_exit "ZJ" "ZJ4_W7_reclaim" 0
  ZJ_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')
  DOCKET_TOKEN="$ZJ_TOKEN" run_env "$ZJ" step complete "$ZJ_PRESS_STEP" \
    --artifact-file "$ZJ/sheets.txt" --usage '{"sheets":300}' --json
  assert_exit "ZJ" "ZJ4_W7_complete" 0

  run_env "$ZJ" next --run RUN-2 --json
  assert_exit "ZJ" "ZJ4_W7_next" 0
  ZJ_PRESS=$(echo "$CMD_STDOUT" | jq -r '[.data.steps[] | select(.class == "press")] | length')
  check_cond "ZJ" "ZJ4_W7_press_flows_again" "no press-class step offered after the ack; the hold did not release" [ "$ZJ_PRESS" -ge 1 ]

  # ===========================================================================
  # ZJ5 — THE GROUP-2 DORMANCY PROOF (§3's per-group table)
  # ===========================================================================
  #
  # "A run that never opens a dispatch produces a byte-identical `next`
  # transcript; a run with no write-class reap likewise; `dispatches` empty."
  #
  # This is the assertion the 2026-08-03 review fix to §5.8 D2 exists to make
  # true. A run driven to completion WITHOUT `--usage` and WITHOUT a dispatch
  # must never be refused — which is what keeps ZK's solo rehearsal and ZH's
  # stranger demo green.

  local ZJ_D
  ZJ_D=$(qa_mktemp_d)
  run_env "$ZJ_D" init
  assert_exit "ZJ" "ZJ5_init" 0
  run_env "$ZJ_D" workflow register "$ZJ_WF" --json
  assert_exit "ZJ" "ZJ5_register" 0
  run_env "$ZJ_D" issue create -t "A job nobody dispatched" -d "solo" --json
  assert_exit "ZJ" "ZJ5_issue" 0
  run_env "$ZJ_D" run start --issue DKT-1 --json
  assert_exit "ZJ" "ZJ5_start" 0
  run_env "$ZJ_D" run activate RUN-1 --json
  assert_exit "ZJ" "ZJ5_activate" 0

  # A step completes with NO `--usage` — the ordinary solo path.
  run_env "$ZJ_D" next --run RUN-1 --json
  assert_exit "ZJ" "ZJ5_next_before" 0
  local ZJ_D_BEFORE ZJ_D_STEP
  ZJ_D_BEFORE=$(echo "$CMD_STDOUT" | jq -c '.data.steps')
  ZJ_D_STEP=$(echo "$CMD_STDOUT" \
    | jq -r '[.data.steps[] | select(.class == "press")][0].step')

  run_env "$ZJ_D" step claim "$ZJ_D_STEP" --owner solo --json
  assert_exit "ZJ" "ZJ5_claim" 0
  ZJ_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')
  printf 'sheets\n' >"$ZJ_D/sheets.txt"
  DOCKET_TOKEN="$ZJ_TOKEN" run_env "$ZJ_D" step complete "$ZJ_D_STEP" \
    --artifact-file "$ZJ_D/sheets.txt" --json
  assert_exit "ZJ" "ZJ5_complete_no_usage" 0

  # THE DORMANCY ASSERTION: `next` answers rather than refusing. A step
  # completed with no usage in a run no relay ever drove has nobody owing
  # usage, so D2 does not fire.
  run_env "$ZJ_D" next --run RUN-1 --json
  assert_exit "ZJ" "ZJ5_no_refusal_without_dispatch" 0

  # `dispatches` is empty and `reap_acks` is empty: neither mechanism was
  # touched, so neither left a row.
  local ZJ_D_DISPATCHES ZJ_D_ACKS
  ZJ_D_DISPATCHES=$(sqlite3 "$ZJ_D/issues.db" "SELECT COUNT(*) FROM dispatches;")
  ZJ_D_ACKS=$(sqlite3 "$ZJ_D/issues.db" "SELECT COUNT(*) FROM reap_acks;")
  if [ "$ZJ_D_DISPATCHES" = "0" ] && [ "$ZJ_D_ACKS" = "0" ]; then
    check "ZJ" "ZJ5_tables_empty" "PASS"
  else
    check "ZJ" "ZJ5_tables_empty" "FAIL" \
      "$ZJ_D_DISPATCHES dispatches and $ZJ_D_ACKS reap_acks on a run that used neither"
  fi

  # And `next` is byte-identical to itself across the mechanism's existence:
  # the same call, before and after a completion, differs only by the step that
  # completed — never by a field this stage added.
  local ZJ_D_KEYS_BEFORE ZJ_D_KEYS_AFTER
  ZJ_D_KEYS_BEFORE=$(echo "$ZJ_D_BEFORE" | jq -S -c '[.[0] | keys] | .[0]')
  ZJ_D_KEYS_AFTER=$(echo "$CMD_STDOUT" | jq -S -c '[.data.steps[0] | keys] | .[0]')
  check_cond "ZJ" "ZJ5_row_shape_unchanged" "next row keys changed: $ZJ_D_KEYS_BEFORE -> $ZJ_D_KEYS_AFTER" [ "$ZJ_D_KEYS_BEFORE" = "$ZJ_D_KEYS_AFTER" ]

  rm -rf "$ZJ" "$ZJ_D"
}
