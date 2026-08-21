#!/usr/bin/env bash
# Section ZF: claims, leases, capability tokens, and engine config
#
# Covers stage 2 of the graph-engine design (docs/design/engine-spec.md §2
# "Steps, claims, capabilities", §4 token transport, §11.4 claim wire shape;
# TDD docs/tdd/claims-leases.md).
#
# ZF3 is the §9.3 concurrency race and ZF4 is the §9.4 kill-a-claimer proof.
# Both run real processes: db.Open caps the connection pool at one, so only
# separate processes exercise SQLite's cross-process locking — which is what a
# racing dispatcher actually hits.

test_zf_claims() {
  printf "Section ZF: Claims, Leases, and Capability Tokens"

  local ZF_ID ZF_NUM TOKEN

  run issue create --json -t "ZF lease subject"
  assert_exit "ZF" "ZF0_setup" 0
  ZF_NUM=$(extract_id)
  ZF_ID="DKT-$ZF_NUM"

  # ---------------------------------------------------------------------------
  # ZF1: claim mints a token and returns the §11.4 response shape.
  # ---------------------------------------------------------------------------

  run issue claim "$ZF_ID" --owner ci-runner --ttl 5m --json
  assert_exit "ZF" "ZF1_claim" 0
  assert_json_exists "ZF" "ZF1_issue" ".data.issue"
  assert_json_exists "ZF" "ZF1_token" ".data.token"
  assert_json_exists "ZF" "ZF1_expires" ".data.lease_expires_ms"
  assert_json "ZF" "ZF1_issue_id" ".data.issue" "$ZF_ID"

  TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  # The token is 32 bytes of crypto/rand, hex encoded.
  check_cond "ZF" "ZF1_token_length" "token length ${#TOKEN}, want 64 hex chars" [ "${#TOKEN}" -eq 64 ]

  # v2 additionally carries the CAS version and the attempt count.
  run issue claim "$ZF_ID" --owner other --json=v2
  assert_exit "ZF" "ZF1_v2_conflict" 4

  # ---------------------------------------------------------------------------
  # ZF2: the §9.3 refusal matrix, R1-R8. Every row proven by exit code AND by
  # the machine-readable code, then by a version-unchanged assertion.
  # ---------------------------------------------------------------------------

  local V_BEFORE V_AFTER
  run issue show "$ZF_ID" --json=v2
  V_BEFORE=$(echo "$CMD_STDOUT" | jq -r '.data.version')

  # R1: no token at all is a VALIDATION_ERROR naming both channels.
  run issue heartbeat "$ZF_ID" --json </dev/null
  assert_exit "ZF" "ZF2_R1_no_token" 3
  assert_json "ZF" "ZF2_R1_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZF" "ZF2_R1_names_env" "DOCKET_TOKEN"
  assert_stdout_contains "ZF" "ZF2_R1_names_stdin" "stdin"

  # R3: a wrong token is AUTH_ERROR.
  DOCKET_TOKEN=deadbeef run issue heartbeat "$ZF_ID" --json
  assert_exit "ZF" "ZF2_R3_wrong_token" 5
  assert_json "ZF" "ZF2_R3_code" ".code" "AUTH_ERROR"

  # R2: an unclaimed issue is AUTH_ERROR too — indistinguishable from a wrong
  # token, so no capability-less caller learns whether a lease exists.
  local UNCLAIMED UNCLAIMED_ID
  run issue create --json -t "ZF unclaimed"
  UNCLAIMED=$(extract_id)
  UNCLAIMED_ID="DKT-$UNCLAIMED"
  DOCKET_TOKEN="$TOKEN" run issue heartbeat "$UNCLAIMED_ID" --json
  assert_exit "ZF" "ZF2_R2_unclaimed" 5
  assert_json "ZF" "ZF2_R2_code" ".code" "AUTH_ERROR"

  # R5: claiming a live lease is CONFLICT.
  run issue claim "$ZF_ID" --owner interloper --json
  assert_exit "ZF" "ZF2_R5_claim_race" 4
  assert_json "ZF" "ZF2_R5_code" ".code" "CONFLICT"

  # R7: a non-holder cannot close a live-leased issue — the issue-level
  # "an unclaimed worker cannot record" proof.
  DOCKET_TOKEN=deadbeef run issue close "$ZF_ID" --json
  assert_exit "ZF" "ZF2_R7_close_nonholder" 5
  assert_json "ZF" "ZF2_R7_code" ".code" "AUTH_ERROR"

  # Refusals write nothing: the CAS version has not moved across all of them.
  run issue show "$ZF_ID" --json=v2
  V_AFTER=$(echo "$CMD_STDOUT" | jq -r '.data.version')
  check_cond "ZF" "ZF2_refusals_write_nothing" "version moved $V_BEFORE -> $V_AFTER" [ "$V_BEFORE" = "$V_AFTER" ]

  # The holder's token still works after all those refusals.
  DOCKET_TOKEN="$TOKEN" run issue heartbeat "$ZF_ID" --ttl 5m --json
  assert_exit "ZF" "ZF2_holder_ok" 0
  assert_json "ZF" "ZF2_holder_attempt" ".data.attempt" "1"

  # A token on stdin works identically to one in the environment.
  run_stdin "$TOKEN" issue heartbeat "$ZF_ID" --ttl 5m --json
  assert_exit "ZF" "ZF2_stdin_token" 0

  # The holder may always release; the released token then never works again.
  DOCKET_TOKEN="$TOKEN" run issue release "$ZF_ID" --json
  assert_exit "ZF" "ZF2_release" 0
  assert_json "ZF" "ZF2_release_attempt" ".data.attempt" "1"
  DOCKET_TOKEN="$TOKEN" run issue heartbeat "$ZF_ID" --json
  assert_exit "ZF" "ZF2_released_token_dead" 5

  # ---------------------------------------------------------------------------
  # ZF3: §9.3 racing dispatchers — N concurrent claims, exactly one winner,
  # N-1 CONFLICTs, and no lost updates.
  # ---------------------------------------------------------------------------

  local RACE_NUM RACE_ID RACE_DIR N
  run issue create --json -t "ZF race subject"
  RACE_NUM=$(extract_id)
  RACE_ID="DKT-$RACE_NUM"
  RACE_DIR=$(qa_mktemp_d)
  N=8

  # Background N real processes against one unclaimed issue.
  #
  # `set +e` inside the subshell is required, not stylistic: the suite runs
  # under `set -e`, and a losing claimant exits 4 — which would kill the
  # subshell before it could record that exit code, leaving the rc file absent
  # and the race silently unmeasured.
  local i
  for i in $(seq 1 "$N"); do
    (
      set +e
      "$DOCKET" issue claim "$RACE_ID" --owner "racer-$i" --ttl 5m --json \
        >"$RACE_DIR/out.$i" 2>"$RACE_DIR/err.$i"
      echo $? >"$RACE_DIR/rc.$i"
    ) &
  done
  wait

  # Every claimant must have recorded an exit code; a missing rc file means the
  # process died without reporting, and the counts below would undercount
  # rather than fail.
  local RECORDED
  RECORDED=$(ls "$RACE_DIR"/rc.* 2>/dev/null | wc -l | tr -d ' ')
  check_cond "ZF" "ZF3_all_reported" "$RECORDED of $N claimants recorded an exit code" [ "$RECORDED" -eq "$N" ]

  local WINNERS=0 CONFLICTS=0 OTHERS=0 RC
  for i in $(seq 1 "$N"); do
    RC=$(cat "$RACE_DIR/rc.$i" 2>/dev/null || echo 99)
    case "$RC" in
      0) WINNERS=$((WINNERS + 1)) ;;
      4) CONFLICTS=$((CONFLICTS + 1)) ;;
      *) OTHERS=$((OTHERS + 1)) ;;
    esac
  done

  check_cond "ZF" "ZF3_one_winner" "$WINNERS winners out of $N, want exactly 1" [ "$WINNERS" -eq 1 ]
  check_cond "ZF" "ZF3_losers_conflict" "$CONFLICTS CONFLICTs, want $((N - 1))" [ "$CONFLICTS" -eq $((N - 1)) ]
  check_cond "ZF" "ZF3_no_other_errors" "$OTHERS claims failed with neither 0 nor 4" [ "$OTHERS" -eq 0 ]

  # Exactly one distinct token was minted for a winner.
  local NTOKENS
  NTOKENS=$(cat "$RACE_DIR"/out.* 2>/dev/null | jq -r 'select(.ok == true) | .data.token' | sort -u | wc -l | tr -d ' ')
  check_cond "ZF" "ZF3_one_token" "$NTOKENS distinct winning tokens, want 1" [ "$NTOKENS" = "1" ]

  # The lost-update proof: one applied claim means attempt == 1. A lost update
  # would show a higher count against a single winner.
  run issue show "$RACE_ID" --json=v2
  assert_json "ZF" "ZF3_no_lost_updates" ".data.lease.attempt" "1"
  assert_json "ZF" "ZF3_lease_live" ".data.lease.live" "true"

  rm -rf "$RACE_DIR"

  # ---------------------------------------------------------------------------
  # ZF4: §9.4 half — kill a claimer mid-work. Lease expiry ALONE re-readies the
  # issue, with a complete attempt trail.
  # ---------------------------------------------------------------------------

  local KILL_NUM KILL_ID KILL_TOKEN
  run issue create --json -t "ZF kill subject"
  KILL_NUM=$(extract_id)
  KILL_ID="DKT-$KILL_NUM"

  # A claimer takes a 1-second lease in a subshell, then is killed without
  # ever releasing — the wedged-worker case.
  local CLAIM_OUT
  CLAIM_OUT=$(qa_mktemp)
  (
    "$DOCKET" issue claim "$KILL_ID" --owner doomed-worker --ttl 1s --json >"$CLAIM_OUT" 2>/dev/null
    # Simulate work that never finishes.
    sleep 60
  ) &
  local CLAIMER_PID=$!

  # Wait for the claim to land, then kill the claimer outright.
  local waited=0
  while [ ! -s "$CLAIM_OUT" ] && [ "$waited" -lt 50 ]; do
    perl -e 'select(undef,undef,undef,0.1)'
    waited=$((waited + 1))
  done
  kill -9 "$CLAIMER_PID" 2>/dev/null || true
  wait "$CLAIMER_PID" 2>/dev/null || true

  KILL_TOKEN=$(jq -r '.data.token' <"$CLAIM_OUT" 2>/dev/null || echo "")
  if [ -n "$KILL_TOKEN" ] && [ "$KILL_TOKEN" != "null" ]; then
    check "ZF" "ZF4_claimed" "PASS"
  else
    check "ZF" "ZF4_claimed" "FAIL" "claimer produced no token"
  fi
  rm -f "$CLAIM_OUT"

  # The lease is live immediately after the kill: nothing has expired yet, and
  # killing a holder does not itself release anything.
  run issue show "$KILL_ID" --json=v2
  assert_json "ZF" "ZF4_live_before_expiry" ".data.lease.live" "true"
  assert_json "ZF" "ZF4_attempt_1" ".data.lease.attempt" "1"

  # Wait out the TTL. NOTHING else runs in between — no reaper, no sweep, no
  # next. Expiry alone must re-ready the issue.
  perl -e 'select(undef,undef,undef,1.3)'

  # A read reports the lease dead without writing: effective status, computed
  # from expires_ms at read time (§2, §6).
  run issue show "$KILL_ID" --json=v2
  assert_json "ZF" "ZF4_expiry_alone" ".data.lease.live" "false"
  assert_json "ZF" "ZF4_owner_retained" ".data.lease.owner" "doomed-worker"

  # The dead holder's token is refused with STALE_LEASE, not AUTH_ERROR: the
  # token was right, time ran out.
  DOCKET_TOKEN="$KILL_TOKEN" run issue heartbeat "$KILL_ID" --json
  assert_exit "ZF" "ZF4_stale_lease" 6
  assert_json "ZF" "ZF4_stale_code" ".code" "STALE_LEASE"

  # A successor claims with no operator action beyond the claim itself.
  run issue claim "$KILL_ID" --owner successor --ttl 5m --json
  assert_exit "ZF" "ZF4_reclaim" 0

  # The attempt trail is complete: both the dead holder's claim and the
  # successor's are counted.
  run issue show "$KILL_ID" --json=v2
  assert_json "ZF" "ZF4_attempt_trail" ".data.lease.attempt" "2"
  assert_json "ZF" "ZF4_new_owner" ".data.lease.owner" "successor"

  # And the dead holder's token is now gone entirely.
  DOCKET_TOKEN="$KILL_TOKEN" run issue heartbeat "$KILL_ID" --json
  assert_exit "ZF" "ZF4_dead_token_gone" 5

  # ZF4b: reads never write (§6). The strongest available form of this: snapshot
  # the database file, run every read verb against an EXPIRED lease — the case
  # that would tempt an implementation to reap — and require the file to come
  # back byte-identical.
  local RO_NUM RO_ID RO_SNAP
  run issue create --json -t "ZF readonly subject"
  RO_NUM=$(extract_id)
  RO_ID="DKT-$RO_NUM"
  run issue claim "$RO_ID" --owner transient --ttl 1s --json
  assert_exit "ZF" "ZF4b_setup" 0
  perl -e 'select(undef,undef,undef,1.3)'

  RO_SNAP=$(qa_mktemp)
  cp "$DOCKET_PATH/issues.db" "$RO_SNAP"

  run issue show "$RO_ID" --json=v2
  run issue list --all --json=v2
  run issue log "$RO_ID" --json
  run issue graph "$RO_ID" --json
  run board --json
  run stats --json
  run next --json
  run export --json

  if cmp -s "$DOCKET_PATH/issues.db" "$RO_SNAP"; then
    check "ZF" "ZF4b_reads_never_write" "PASS"
  else
    check "ZF" "ZF4b_reads_never_write" "FAIL" "a read verb modified the database file"
  fi
  rm -f "$RO_SNAP"

  # And the expired lease still reads as dead, with its owner retained — the
  # row was not reaped, the liveness was computed.
  run issue show "$RO_ID" --json=v2
  assert_json "ZF" "ZF4b_still_dead" ".data.lease.live" "false"
  assert_json "ZF" "ZF4b_owner_retained" ".data.lease.owner" "transient"

  # ---------------------------------------------------------------------------
  # ZF5: tokens never leak. The token appears in the claim response and nowhere
  # else, at either JSON version.
  # ---------------------------------------------------------------------------

  run issue show "$KILL_ID" --json=v2
  if echo "$CMD_STDOUT" | grep -qF "$KILL_TOKEN"; then
    check "ZF" "ZF5_show_no_token" "FAIL" "issue show leaked a token"
  else
    check "ZF" "ZF5_show_no_token" "PASS"
  fi
  # No token_hash either — the hash is storage, not surface.
  assert_json_null "ZF" "ZF5_no_token_hash" ".data.lease.token_hash"

  run issue list --all --json=v2
  if echo "$CMD_STDOUT" | grep -qF "$KILL_TOKEN"; then
    check "ZF" "ZF5_list_no_token" "FAIL" "issue list leaked a token"
  else
    check "ZF" "ZF5_list_no_token" "PASS"
  fi

  run issue log "$KILL_ID" --json
  if echo "$CMD_STDOUT" | grep -qF "$KILL_TOKEN"; then
    check "ZF" "ZF5_log_no_token" "FAIL" "issue log leaked a token"
  else
    check "ZF" "ZF5_log_no_token" "PASS"
  fi

  # ---------------------------------------------------------------------------
  # ZF6: dormancy — an issue that is never claimed is unchanged everywhere.
  # ---------------------------------------------------------------------------

  # v1 never carries a lease, even for a claimed issue.
  run issue show "$KILL_ID" --json
  assert_json_null "ZF" "ZF6_v1_no_lease" ".data.lease"
  assert_json_null "ZF" "ZF6_v1_no_version" ".data.version"

  # An unclaimed issue emits no lease key at all under v2 — not "lease": null.
  run issue show "$UNCLAIMED_ID" --json=v2
  assert_json_null "ZF" "ZF6_unclaimed_no_lease" ".data.lease"
  local HAS_LEASE
  HAS_LEASE=$(echo "$CMD_STDOUT" | jq -r '.data | has("lease")')
  check_cond "ZF" "ZF6_lease_key_omitted" "unclaimed issue emits a lease key" [ "$HAS_LEASE" = "false" ]

  # An unclaimed issue closes with no token and no refusal.
  run issue close "$UNCLAIMED_ID" --json
  assert_exit "ZF" "ZF6_unclaimed_close" 0

  # ---------------------------------------------------------------------------
  # ZF7: closing a claimed issue as the holder ends the lease.
  # ---------------------------------------------------------------------------

  local CLOSE_NUM CLOSE_ID CLOSE_TOKEN
  run issue create --json -t "ZF close subject"
  CLOSE_NUM=$(extract_id)
  CLOSE_ID="DKT-$CLOSE_NUM"
  run issue claim "$CLOSE_ID" --owner closer --ttl 5m --json
  CLOSE_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  DOCKET_TOKEN="$CLOSE_TOKEN" run issue close "$CLOSE_ID" --json
  assert_exit "ZF" "ZF7_holder_close" 0

  run issue show "$CLOSE_ID" --json=v2
  assert_json "ZF" "ZF7_closed" ".data.status" "done"
  assert_json_null "ZF" "ZF7_lease_ended" ".data.lease"

  # ---------------------------------------------------------------------------
  # ZF8: engine config set|get.
  # ---------------------------------------------------------------------------

  # Defaults are reported as defaults, so "unset" is distinguishable from
  # "set to the same value".
  run config get lease.ttl.default --json
  assert_exit "ZF" "ZF8_get_default" 0
  assert_json "ZF" "ZF8_default_value" ".data.value" "15m"
  assert_json "ZF" "ZF8_default_source" ".data.source" "default"

  run config get attempt.max --json
  assert_json "ZF" "ZF8_attempt_default" ".data.value" "3"
  run config get budget.default --json
  assert_json "ZF" "ZF8_budget_default" ".data.value" "0"
  run config get context.warn_bytes --json
  assert_json "ZF" "ZF8_warn_default" ".data.value" "65536"
  run config get context.error_bytes --json
  assert_json "ZF" "ZF8_error_default" ".data.value" "131072"

  # A set value round-trips and reports source "set".
  run config set lease.ttl.default 30m --json
  assert_exit "ZF" "ZF8_set" 0
  run config get lease.ttl.default --json
  assert_json "ZF" "ZF8_roundtrip" ".data.value" "30m"
  assert_json "ZF" "ZF8_roundtrip_source" ".data.source" "set"

  # A per-class TTL falls back to the default until it is set.
  run config get lease.ttl.write --json
  assert_json "ZF" "ZF8_class_fallback" ".data.value" "30m"
  assert_json "ZF" "ZF8_class_fallback_src" ".data.source" "default"
  run config set lease.ttl.write 45m --json
  assert_exit "ZF" "ZF8_set_class" 0
  run config get lease.ttl.write --json
  assert_json "ZF" "ZF8_class_value" ".data.value" "45m"

  # An unknown key is a VALIDATION_ERROR naming the key — a typo must not
  # silently store something nothing reads.
  run config set lease.tt1.default 30m --json
  assert_exit "ZF" "ZF8_unknown_key" 3
  assert_json "ZF" "ZF8_unknown_code" ".code" "VALIDATION_ERROR"
  run config get nonsense.key --json
  assert_exit "ZF" "ZF8_unknown_get" 3

  # Values are validated at set time, per the key's type.
  run config set lease.ttl.default "not-a-duration" --json
  assert_exit "ZF" "ZF8_bad_duration" 3
  run config set attempt.max 0 --json
  assert_exit "ZF" "ZF8_bad_attempt" 3
  run config set context.warn_bytes not-a-number --json
  assert_exit "ZF" "ZF8_bad_bytes" 3

  # The refused writes did not apply.
  run config get lease.ttl.default --json
  assert_json "ZF" "ZF8_unchanged" ".data.value" "30m"

  # The class TTL drives an actual claim's lease length.
  local TTL_NUM TTL_ID EXPIRES NOW_MS DELTA
  run issue create --json -t "ZF ttl subject"
  TTL_NUM=$(extract_id)
  TTL_ID="DKT-$TTL_NUM"
  run issue claim "$TTL_ID" --owner ttl-probe --class write --json
  assert_exit "ZF" "ZF8_class_claim" 0
  EXPIRES=$(echo "$CMD_STDOUT" | jq -r '.data.lease_expires_ms')
  NOW_MS=$(perl -MTime::HiRes=time -e 'printf "%d\n", time*1000')
  DELTA=$(( (EXPIRES - NOW_MS) / 1000 ))
  # 45m = 2700s; allow generous slack for a slow CI box.
  if [ "$DELTA" -gt 2600 ] && [ "$DELTA" -le 2700 ]; then
    check "ZF" "ZF8_class_ttl_applied" "PASS"
  else
    check "ZF" "ZF8_class_ttl_applied" "FAIL" "lease lasts ${DELTA}s, want ~2700s"
  fi

  # Listing reports every key with its source.
  run config get --json=v2
  assert_exit "ZF" "ZF8_list" 0
  assert_json_array_min "ZF" "ZF8_list_items" ".data.items" 5
  assert_json_all "ZF" "ZF8_list_sourced" ".data.items" "has(\"source\")"

  # The bare `config` verb is untouched — it still reports the DB info and
  # still works without a database (skipDB), which section C also covers.
  run config --json
  assert_exit "ZF" "ZF8_bare_config" 0
  assert_json "ZF" "ZF8_bare_schema" ".data.schema_version" "$CURRENT_SCHEMA_VERSION"

  # ---------------------------------------------------------------------------
  # ZF9: claim input validation.
  # ---------------------------------------------------------------------------

  run issue claim "$UNCLAIMED_ID" --json
  assert_exit "ZF" "ZF9_owner_required" 3
  run issue claim DKT-999999 --owner x --json
  assert_exit "ZF" "ZF9_missing_issue" 2
  run issue claim "$UNCLAIMED_ID" --owner x --ttl 0s --json
  assert_exit "ZF" "ZF9_bad_ttl" 3
}
