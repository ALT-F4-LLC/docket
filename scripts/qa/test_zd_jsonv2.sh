#!/usr/bin/env bash
# Section ZD: --json=v2 envelopes, truncation flags, and flag compatibility
#
# Covers the stage-1 reliability delta (docs/design/engine-spec.md §5, TDD
# docs/tdd/reliability-delta.md): the --json Bool→String surgery, the uniform
# {items,total,truncated} envelope, explicit truncation flags, and the hard
# VALIDATION_ERROR on the silent-drop cases.
#
# The dormancy bar (engine-spec.md §9 item 8) is the point of most of these
# checks: every existing verb stays byte-compatible without --json=v2.

test_zd_jsonv2() {
  printf "Section ZD: JSON v2 Envelopes"

  # ---------------------------------------------------------------------------
  # Flag compatibility matrix — the most compat-sensitive edit in the program.
  # ---------------------------------------------------------------------------

  # ZD1: bare --json still emits the v1 envelope (NoOptDefVal="v1").
  run issue list --json
  assert_exit "ZD" "ZD1" 0
  assert_json "ZD" "ZD1_ok" ".ok" "true"
  assert_json_exists "ZD" "ZD1_issues" ".data.issues"
  # v1 must NOT carry the v2 keys.
  assert_json_null "ZD" "ZD1_no_items" ".data.items"
  assert_json_null "ZD" "ZD1_no_trunc" ".data.truncated"

  # ZD2: --json=true and --json=1 retain their Bool-era meaning (v1).
  run issue list --json=true
  assert_exit "ZD" "ZD2_true" 0
  assert_json_exists "ZD" "ZD2_true_issues" ".data.issues"
  run issue list --json=1
  assert_exit "ZD" "ZD2_one" 0
  assert_json_exists "ZD" "ZD2_one_issues" ".data.issues"

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

  # ---------------------------------------------------------------------------
  # Byte-identical v1 output: bare --json vs --json=true vs --json=1.
  # ---------------------------------------------------------------------------

  # ZD5: all three v1 spellings produce byte-identical output.
  local V1_BARE V1_TRUE V1_ONE
  run issue list --json;      V1_BARE="$CMD_STDOUT"
  run issue list --json=true; V1_TRUE="$CMD_STDOUT"
  run issue list --json=1;    V1_ONE="$CMD_STDOUT"
  if [ "$V1_BARE" = "$V1_TRUE" ] && [ "$V1_BARE" = "$V1_ONE" ]; then
    check "ZD" "ZD5_identical" "PASS"
  else
    check "ZD" "ZD5_identical" "FAIL" "v1 spellings diverged"
  fi
  # Guard against a vacuous pass on empty output.
  check_cond "ZD" "ZD5_nonempty" "v1 output was empty" [ -n "$V1_BARE" ]

  # ---------------------------------------------------------------------------
  # v2 envelope shape: {items,total,truncated} (engine-spec.md §5).
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

  # ZD9: `next` — the primary silent-drop case. v1 reports a POST-limit total
  # (indistinguishable from "that is all there is"); v2 reports the true
  # pre-limit count and flags the drop.
  run next --json --limit 1
  assert_exit "ZD" "ZD9_v1" 0
  assert_json "ZD" "ZD9_v1_total" ".data.total" "1"
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
  assert_exit "ZD" "ZD10_v1" 0
  assert_json "ZD" "ZD10_v1_total" ".data.total" "1"
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
  # Hard VALIDATION_ERROR on silent-drop input; v1 behavior preserved exactly.
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

  # ZD13: under v1 the three legacy reinterpretations survive untouched —
  # `issue list` and `next` treat a negative limit as unlimited, `issue log`
  # clamps it to 1. This is the dormancy guarantee for the silent-drop fix.
  run issue list --json --limit -1
  assert_exit "ZD" "ZD13_list" 0
  assert_json "ZD" "ZD13_list_unlimited" '(.data.issues | length > 1)' "true"
  run next --json --limit -1
  assert_exit "ZD" "ZD13_next" 0
  run issue log "$ZD_ISSUE" --json --limit -1
  assert_exit "ZD" "ZD13_log" 0
  assert_json "ZD" "ZD13_log_clamped" '(.data.entries | length)' "1"

  # ZD14: --limit 0 means "no limit" and is valid under both versions.
  run issue list --json=v2 --limit 0
  assert_exit "ZD" "ZD14_v2" 0
  assert_json "ZD" "ZD14_v2_trunc" ".data.truncated" "false"

  # ---------------------------------------------------------------------------
  # Scalar (non-collection) payloads are identical across versions.
  # ---------------------------------------------------------------------------

  # ZD15: `issue show` has no items/total/truncated to report, so v2 does NOT
  # wrap it in a collection envelope. From schema v5 onward v2 additionally
  # carries the CAS `version` field (see section ZE), and a later change added `scope`
  # — the declared scope globs, a v2-only read surface — so the invariant here
  # is "identical apart from the v2-only fields", not byte-equality.
  #
  # The deletions are ENUMERATED rather than filtered by a rule, so adding a
  # v2-only field stays a deliberate edit to this line: a version of this check
  # that ignored every key absent from v1 would stop noticing v2 additions
  # altogether, which is the drift it exists to catch.
  local SHOW_V1 SHOW_V2
  run issue show "$ZD_ISSUE" --json;    SHOW_V1="$CMD_STDOUT"
  run issue show "$ZD_ISSUE" --json=v2; SHOW_V2="$CMD_STDOUT"
  if [ -n "$SHOW_V1" ] && \
     [ "$(echo "$SHOW_V1" | jq -Sc '.data')" = "$(echo "$SHOW_V2" | jq -Sc '.data | del(.version, .scope)')" ]; then
    check "ZD" "ZD15_scalar_same" "PASS"
  else
    check "ZD" "ZD15_scalar_same" "FAIL" "issue show differed beyond the v2-only fields"
  fi

  # ZD15b: `scope` under v1 appears WHEN DECLARED and only then (the DKT-55
  # amendment to the freeze). An issue with NO declared scope keeps the
  # byte-identical frozen shape — no `scope` key — so dormancy holds for every
  # repo that never used --scope; a declared scope is visible on the default
  # surface rather than hidden behind --json=v2.
  run issue show "$ZD_ISSUE" --json
  if echo "$CMD_STDOUT" | jq -e '.data | has("scope")' >/dev/null 2>&1; then
    check "ZD" "ZD15b_scope_v1_dormant" "FAIL" "v1 payload gained a scope key on an issue with NO declared scope"
  else
    check "ZD" "ZD15b_scope_v1_dormant" "PASS"
  fi
  run issue show "$ZD_ISSUE" --json=v2
  if echo "$CMD_STDOUT" | jq -e '.data | has("scope")' >/dev/null 2>&1; then
    check "ZD" "ZD15b_scope_in_v2" "PASS"
  else
    check "ZD" "ZD15b_scope_in_v2" "FAIL" "v2 payload is missing the scope key"
  fi

  # ZD15c: a DECLARED scope reaches the v1 default surface (DKT-55).
  local ZD_SCOPED
  run issue create --json -t "zd scoped" --scope 'internal/db/**'
  ZD_SCOPED=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  run issue show "$ZD_SCOPED" --json
  if [ "$(echo "$CMD_STDOUT" | jq -cr '.data.scope')" = '["internal/db/**"]' ]; then
    check "ZD" "ZD15c_scope_v1_declared" "PASS"
  else
    check "ZD" "ZD15c_scope_v1_declared" "FAIL" \
      "declared scope absent from v1 issue show: $(echo "$CMD_STDOUT" | jq -c '.data.scope')"
  fi
  run issue list --json --all
  if echo "$CMD_STDOUT" | jq -e --arg id "$ZD_SCOPED" \
      '.data.issues[] | select(.id == $id) | .scope == ["internal/db/**"]' >/dev/null 2>&1; then
    check "ZD" "ZD15c_scope_v1_list" "PASS"
  else
    check "ZD" "ZD15c_scope_v1_list" "FAIL" "declared scope absent from v1 issue list"
  fi
  # ZD15d: DKT-452 — an issue's id is served under BOTH `id` and `issue`, on
  # v1 and v2 alike, on the detail view and on list rows. `issue` is the noun
  # every other verb keys its primary entity by (`run status` -> run, `step
  # show` -> step, `dispatch open` -> dispatch), and the issue read verbs were
  # the lone exception, so a caller that had just parsed a run reached for
  # `.data.issue` and got null.
  #
  # Unlike ZD15b's scope, this alias is UNCONDITIONAL: a conditional one would
  # be absent in exactly the case it exists to serve. It is therefore the one
  # amendment that moves v1's bytes — additively, which no reader of `.data.id`
  # can notice.
  # `$ZD_ISSUE` is the bare number extract_id leaves behind; the wire carries
  # the rendered id, so the expectation is spelled out rather than reused.
  local ZD_ISSUE_REF="DKT-$ZD_ISSUE"
  run issue show "$ZD_ISSUE" --json
  assert_json "ZD" "ZD15d_show_v1_alias" ".data.issue" "$ZD_ISSUE_REF"
  assert_json "ZD" "ZD15d_show_v1_pair" "(.data.issue == .data.id)" "true"
  run issue show "$ZD_ISSUE" --json=v2
  assert_json "ZD" "ZD15d_show_v2_alias" ".data.issue" "$ZD_ISSUE_REF"
  assert_json "ZD" "ZD15d_show_v2_pair" "(.data.issue == .data.id)" "true"
  run issue list --json --all
  assert_json "ZD" "ZD15d_list_v1_alias" \
    "[.data.issues[] | select(.id == \"$ZD_ISSUE_REF\") | .issue] | first" "$ZD_ISSUE_REF"
  run issue list --json=v2 --all
  assert_json "ZD" "ZD15d_list_v2_alias" \
    "[.data.items[] | select(.id == \"$ZD_ISSUE_REF\") | .issue] | first" "$ZD_ISSUE_REF"

  # v2 must not turn a scalar payload into {items,total,truncated}.
  run issue show "$ZD_ISSUE" --json=v2
  assert_json_null "ZD" "ZD15_not_collection" ".data.items"

  # ---------------------------------------------------------------------------
  # Error envelope: extended taxonomy, existing codes never renumbered.
  # ---------------------------------------------------------------------------

  # ZD16: the four pre-existing codes keep their exit numbers under both
  # versions. NOT_FOUND=2, VALIDATION_ERROR=3, CONFLICT=4.
  run issue show 999999 --json
  assert_exit "ZD" "ZD16_v1_notfound" 2
  assert_json "ZD" "ZD16_v1_code" ".code" "NOT_FOUND"
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
