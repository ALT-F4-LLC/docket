#!/usr/bin/env bash
# Section ZD: --json=v2 envelopes, truncation flags, and flag compatibility
#
# Covers the stage-1 reliability delta: the --json Bool→String surgery, the
# uniform {items,total,truncated} envelope, explicit truncation flags, and the
# hard VALIDATION_ERROR on the silent-drop cases.
#
# v2 is the only JSON dialect: bare --json selects it, and the retired
# --json=v1 spelling is a hard VALIDATION_ERROR.

test_zd_jsonv2() {
  printf "Section ZD: JSON v2 Envelopes"

  # ---------------------------------------------------------------------------
  # Flag compatibility matrix — the most compat-sensitive edit in the program.
  # ---------------------------------------------------------------------------

  # ZD1: bare --json emits the v2 envelope (NoOptDefVal="v2").
  run issue list --json
  assert_exit "ZD" "ZD1" 0
  assert_json "ZD" "ZD1_ok" ".ok" "true"
  assert_json_exists "ZD" "ZD1_items" ".data.items"
  assert_json "ZD" "ZD1_keys" '(.data | keys | sort | join(","))' "items,total,truncated"
  # The retired v1 key must be gone.
  assert_json_null "ZD" "ZD1_no_issues" ".data.issues"

  # ZD2: --json=true and --json=1 keep their Bool-era spelling and select v2.
  run issue list --json=true
  assert_exit "ZD" "ZD2_true" 0
  assert_json_exists "ZD" "ZD2_true_items" ".data.items"
  assert_json_null "ZD" "ZD2_true_no_issues" ".data.issues"
  run issue list --json=1
  assert_exit "ZD" "ZD2_one" 0
  assert_json_exists "ZD" "ZD2_one_items" ".data.items"
  assert_json_null "ZD" "ZD2_one_no_issues" ".data.issues"

  # ZD3: --json=false and --json=0 mean human output (no JSON on stdout).
  run issue list --json=false
  assert_exit "ZD" "ZD3_false" 0
  if echo "$CMD_STDOUT" | jq -e . >/dev/null 2>&1; then
    check "ZD" "ZD3_false_human" "FAIL" "--json=false emitted JSON"
  else
    check "ZD" "ZD3_false_human" "PASS"
  fi

  # ZD4: an unknown --json value is a hard VALIDATION_ERROR (exit 3), never a
  # silent fallback to human output.
  run issue list --json=v3
  assert_exit "ZD" "ZD4_exit" 3
  assert_stderr_contains "ZD" "ZD4_msg" "invalid value"

  # ZD4b: the retired --json=v1 spelling is refused the same way: exit 3 with
  # no JSON on stdout, never a silent fallback to another envelope.
  run issue list --json=v1
  assert_exit "ZD" "ZD4b_v1_exit" 3
  assert_stderr_contains "ZD" "ZD4b_v1_msg" "invalid value"
  assert_stderr_contains "ZD" "ZD4b_v1_names_v2" "want v2"
  check_cond "ZD" "ZD4b_v1_no_stdout" "--json=v1 wrote to stdout" [ -z "$CMD_STDOUT" ]

  # ---------------------------------------------------------------------------
  # Byte-identical v2 output across every spelling that selects it.
  # ---------------------------------------------------------------------------

  # ZD5: bare --json, --json=true, --json=1 and --json=v2 produce
  # byte-identical output.
  local V2_BARE V2_TRUE V2_ONE V2_EXPLICIT
  run issue list --json;      V2_BARE="$CMD_STDOUT"
  run issue list --json=true; V2_TRUE="$CMD_STDOUT"
  run issue list --json=1;    V2_ONE="$CMD_STDOUT"
  run issue list --json=v2;   V2_EXPLICIT="$CMD_STDOUT"
  if [ "$V2_BARE" = "$V2_TRUE" ] && [ "$V2_BARE" = "$V2_ONE" ] && \
     [ "$V2_BARE" = "$V2_EXPLICIT" ]; then
    check "ZD" "ZD5_identical" "PASS"
  else
    check "ZD" "ZD5_identical" "FAIL" "v2 spellings diverged"
  fi
  # Guard against a vacuous pass on empty output.
  check_cond "ZD" "ZD5_nonempty" "v2 output was empty" [ -n "$V2_BARE" ]

  # ---------------------------------------------------------------------------
  # v2 envelope shape: {items,total,truncated}.
  # ---------------------------------------------------------------------------

  # ZD6: issue list v2 carries exactly the three envelope keys.
  run issue list --json=v2
  assert_exit "ZD" "ZD6" 0
  assert_json "ZD" "ZD6_ok" ".ok" "true"
  assert_json_exists "ZD" "ZD6_items" ".data.items"
  assert_json_exists "ZD" "ZD6_total" ".data.total"
  assert_json "ZD" "ZD6_keys" '(.data | keys | sort | join(","))' "items,total,truncated"
  # The v1 key must be gone under v2.
  assert_json_null "ZD" "ZD6_no_issues" ".data.issues"

  # ZD7: truncated is false when nothing was dropped.
  assert_json "ZD" "ZD7_trunc_false" ".data.truncated" "false"

  # ---------------------------------------------------------------------------
  # Explicit truncation flags across every list verb.
  # ---------------------------------------------------------------------------

  # ZD8: issue list --limit 1 flags truncation and reports the true total.
  run issue list --json=v2 --limit 1
  assert_exit "ZD" "ZD8" 0
  assert_json "ZD" "ZD8_n" '(.data.items | length)' "1"
  assert_json "ZD" "ZD8_trunc" ".data.truncated" "true"
  assert_json "ZD" "ZD8_total_gt" '(.data.total > 1)' "true"

  # ZD9: `next` — the primary silent-drop case. v2 reports the true pre-limit
  # count and flags the drop, under bare --json as under --json=v2.
  run next --json --limit 1
  assert_exit "ZD" "ZD9_bare" 0
  assert_json "ZD" "ZD9_bare_n" '(.data.items | length)' "1"
  assert_json "ZD" "ZD9_bare_trunc" ".data.truncated" "true"
  assert_json "ZD" "ZD9_bare_total_gt" '(.data.total > 1)' "true"
  run next --json=v2 --limit 1
  assert_exit "ZD" "ZD9_v2" 0
  assert_json "ZD" "ZD9_v2_n" '(.data.items | length)' "1"
  assert_json "ZD" "ZD9_v2_trunc" ".data.truncated" "true"
  assert_json "ZD" "ZD9_v2_total_gt" '(.data.total > 1)' "true"

  # ZD10: `issue log` — the second silent-drop case, same shape.
  local ZD_ISSUE
  run issue create --json -t "ZD truncation probe"
  assert_exit "ZD" "ZD10_setup" 0
  ZD_ISSUE=$(extract_id)
  run issue edit "$ZD_ISSUE" --json -s todo
  run issue edit "$ZD_ISSUE" --json -p high
  run issue edit "$ZD_ISSUE" --json -a "zd-tester"

  run issue log "$ZD_ISSUE" --json --limit 1
  assert_exit "ZD" "ZD10_bare" 0
  assert_json "ZD" "ZD10_bare_n" '(.data.items | length)' "1"
  assert_json "ZD" "ZD10_bare_trunc" ".data.truncated" "true"
  assert_json "ZD" "ZD10_bare_total_gt" '(.data.total > 1)' "true"
  run issue log "$ZD_ISSUE" --json=v2 --limit 1
  assert_exit "ZD" "ZD10_v2" 0
  assert_json "ZD" "ZD10_v2_n" '(.data.items | length)' "1"
  assert_json "ZD" "ZD10_v2_trunc" ".data.truncated" "true"
  assert_json "ZD" "ZD10_v2_total_gt" '(.data.total > 1)' "true"

  # ZD11: doc list and vote list carry the envelope too.
  run doc list --json=v2
  assert_exit "ZD" "ZD11_doc" 0
  assert_json "ZD" "ZD11_doc_keys" '(.data | keys | sort | join(","))' "items,total,truncated"
  run vote list --json=v2 --all
  assert_exit "ZD" "ZD11_vote" 0
  assert_json "ZD" "ZD11_vote_keys" '(.data | keys | sort | join(","))' "items,total,truncated"

  # ---------------------------------------------------------------------------
  # Hard VALIDATION_ERROR on silent-drop input.
  # ---------------------------------------------------------------------------

  # ZD12: a negative --limit is rejected under v2 on every list verb.
  run issue list --json=v2 --limit -1
  assert_exit "ZD" "ZD12_list" 3
  assert_json "ZD" "ZD12_list_code" ".code" "VALIDATION_ERROR"
  run next --json=v2 --limit -1
  assert_exit "ZD" "ZD12_next" 3
  run issue log "$ZD_ISSUE" --json=v2 --limit -1
  assert_exit "ZD" "ZD12_log" 3
  run doc list --json=v2 --limit -1
  assert_exit "ZD" "ZD12_doc" 3
  run vote list --json=v2 --limit -1
  assert_exit "ZD" "ZD12_vote" 3

  # ZD13: bare --json is v2, so it refuses a negative limit too. The v1
  # reinterpretations (unlimited on `issue list` and `next`, clamped to 1 on
  # `issue log`) are no longer reachable.
  run issue list --json --limit -1
  assert_exit "ZD" "ZD13_list" 3
  assert_json "ZD" "ZD13_list_code" ".code" "VALIDATION_ERROR"
  run next --json --limit -1
  assert_exit "ZD" "ZD13_next" 3
  run issue log "$ZD_ISSUE" --json --limit -1
  assert_exit "ZD" "ZD13_log" 3

  # ZD14: --limit 0 means "no limit".
  run issue list --json=v2 --limit 0
  assert_exit "ZD" "ZD14_v2" 0
  assert_json "ZD" "ZD14_v2_trunc" ".data.truncated" "false"

  # ---------------------------------------------------------------------------
  # Scalar (non-collection) payloads.
  # ---------------------------------------------------------------------------

  # ZD15: `issue show` has no items/total/truncated to report, so v2 does NOT
  # wrap it in a collection envelope. Bare --json and --json=v2 select the
  # same dialect, so the payload is byte-identical, including the CAS
  # `version` field (see section ZE) and `scope`.
  local SHOW_BARE SHOW_V2
  run issue show "$ZD_ISSUE" --json;    SHOW_BARE="$CMD_STDOUT"
  run issue show "$ZD_ISSUE" --json=v2; SHOW_V2="$CMD_STDOUT"
  if [ -n "$SHOW_BARE" ] && [ "$SHOW_BARE" = "$SHOW_V2" ]; then
    check "ZD" "ZD15_scalar_same" "PASS"
  else
    check "ZD" "ZD15_scalar_same" "FAIL" "issue show differed between bare --json and --json=v2"
  fi
  assert_json "ZD" "ZD15_version" '(.data | has("version"))' "true"

  # ZD15b: v2 carries the `scope` key even on an issue with NO declared scope,
  # under bare --json as under --json=v2.
  run issue show "$ZD_ISSUE" --json
  if echo "$CMD_STDOUT" | jq -e '.data | has("scope")' >/dev/null 2>&1; then
    check "ZD" "ZD15b_scope_bare" "PASS"
  else
    check "ZD" "ZD15b_scope_bare" "FAIL" "bare --json payload is missing the scope key"
  fi
  run issue show "$ZD_ISSUE" --json=v2
  if echo "$CMD_STDOUT" | jq -e '.data | has("scope")' >/dev/null 2>&1; then
    check "ZD" "ZD15b_scope_in_v2" "PASS"
  else
    check "ZD" "ZD15b_scope_in_v2" "FAIL" "v2 payload is missing the scope key"
  fi

  # ZD15c: a DECLARED scope reaches the default --json surface on both the
  # detail view and list rows (DKT-55).
  local ZD_SCOPED
  run issue create --json -t "zd scoped" --scope 'internal/db/**'
  ZD_SCOPED=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  run issue show "$ZD_SCOPED" --json
  if [ "$(echo "$CMD_STDOUT" | jq -cr '.data.scope')" = '["internal/db/**"]' ]; then
    check "ZD" "ZD15c_scope_declared" "PASS"
  else
    check "ZD" "ZD15c_scope_declared" "FAIL" \
      "declared scope absent from issue show: $(echo "$CMD_STDOUT" | jq -c '.data.scope')"
  fi
  run issue list --json --all
  if echo "$CMD_STDOUT" | jq -e --arg id "$ZD_SCOPED" \
      '.data.items[] | select(.id == $id) | .scope == ["internal/db/**"]' >/dev/null 2>&1; then
    check "ZD" "ZD15c_scope_list" "PASS"
  else
    check "ZD" "ZD15c_scope_list" "FAIL" "declared scope absent from issue list"
  fi
  # ZD15d: DKT-452 — an issue's id is served under BOTH `id` and `issue`, on
  # the detail view and on list rows. `issue` is the noun every other verb
  # keys its primary entity by (`run status` -> run, `step show` -> step,
  # `dispatch open` -> dispatch), and the issue read verbs were the lone
  # exception, so a caller that had just parsed a run reached for
  # `.data.issue` and got null. The alias is UNCONDITIONAL: a conditional one
  # would be absent in exactly the case it exists to serve.
  # `$ZD_ISSUE` is the bare number extract_id leaves behind; the wire carries
  # the rendered id, so the expectation is spelled out rather than reused.
  local ZD_ISSUE_REF="DKT-$ZD_ISSUE"
  run issue show "$ZD_ISSUE" --json
  assert_json "ZD" "ZD15d_show_bare_alias" ".data.issue" "$ZD_ISSUE_REF"
  assert_json "ZD" "ZD15d_show_bare_pair" "(.data.issue == .data.id)" "true"
  run issue show "$ZD_ISSUE" --json=v2
  assert_json "ZD" "ZD15d_show_v2_alias" ".data.issue" "$ZD_ISSUE_REF"
  assert_json "ZD" "ZD15d_show_v2_pair" "(.data.issue == .data.id)" "true"
  run issue list --json --all
  assert_json "ZD" "ZD15d_list_bare_alias" \
    "[.data.items[] | select(.id == \"$ZD_ISSUE_REF\") | .issue] | first" "$ZD_ISSUE_REF"
  run issue list --json=v2 --all
  assert_json "ZD" "ZD15d_list_v2_alias" \
    "[.data.items[] | select(.id == \"$ZD_ISSUE_REF\") | .issue] | first" "$ZD_ISSUE_REF"

  # v2 must not turn a scalar payload into {items,total,truncated}.
  run issue show "$ZD_ISSUE" --json=v2
  assert_json_null "ZD" "ZD15_not_collection" ".data.items"

  # ---------------------------------------------------------------------------
  # Error envelope: extended taxonomy, existing codes never renumbered.
  # ---------------------------------------------------------------------------

  # ZD16: the four pre-existing codes keep their exit numbers under every
  # spelling. NOT_FOUND=2, VALIDATION_ERROR=3, CONFLICT=4.
  run issue show 999999 --json
  assert_exit "ZD" "ZD16_bare_notfound" 2
  assert_json "ZD" "ZD16_bare_code" ".code" "NOT_FOUND"
  run issue show 999999 --json=v2
  assert_exit "ZD" "ZD16_v2_notfound" 2
  assert_json "ZD" "ZD16_v2_code" ".code" "NOT_FOUND"

  run issue move "$ZD_ISSUE" not-a-status --json=v2
  assert_exit "ZD" "ZD16_validation" 3
  assert_json "ZD" "ZD16_validation_code" ".code" "VALIDATION_ERROR"

  # ZD17: the v2 error envelope keeps the v1 shape {ok,error,code}.
  run issue show 999999 --json=v2
  assert_json "ZD" "ZD17_keys" '(. | keys | sort | join(","))' "code,error,ok"
  assert_json "ZD" "ZD17_ok" ".ok" "false"
}
