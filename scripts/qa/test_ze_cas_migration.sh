#!/usr/bin/env bash
# Section ZE: v5 migration, CAS --if-version, and idempotency keys
#
# Covers the storage half of the reliability delta (docs/design/engine-spec.md
# §5; TDD docs/tdd/reliability-delta.md): the v4→v5 migration, version CAS
# columns, --if-version on mutating verbs, versions in .data under v2, and
# idempotency keys on create verbs.
#
# ZE1 is the engine-spec §9.8 fixture proof — it re-runs at every future stage.

test_ze_cas_migration() {
  printf "Section ZE: CAS and v5 Migration"

  local FIXTURE="$SCRIPT_DIR/qa/fixtures/v4-baseline.db"

  # ---------------------------------------------------------------------------
  # ZE1: §9.8 — a v4 DB opens, migrates to v5, and reads identically.
  # ---------------------------------------------------------------------------

  if [ ! -f "$FIXTURE" ]; then
    check "ZE" "ZE1_fixture_exists" "FAIL" "missing fixture $FIXTURE"
    return
  fi
  check "ZE" "ZE1_fixture_exists" "PASS"

  local FXDIR
  FXDIR=$(qa_mktemp_d)
  mkdir -p "$FXDIR"
  cp "$FIXTURE" "$FXDIR/issues.db"

  # The committed fixture must genuinely be v4, else the proof is vacuous.
  local FX_BEFORE
  FX_BEFORE=$(sqlite3 "$FXDIR/issues.db" "SELECT value FROM meta WHERE key='schema_version';")
  check_cond "ZE" "ZE1_fixture_is_v4" "fixture schema_version=$FX_BEFORE, want 4" [ "$FX_BEFORE" = "4" ]

  # Capture pre-migration reads, open the DB (which migrates), compare.
  local BEFORE_LIST BEFORE_SHOW AFTER_LIST AFTER_SHOW
  run_env "$FXDIR" issue list --all --json
  BEFORE_LIST="$CMD_STDOUT"
  assert_exit "ZE" "ZE1_open_v4" 0

  run_env "$FXDIR" issue show DKT-1 --json
  BEFORE_SHOW="$CMD_STDOUT"
  assert_exit "ZE" "ZE1_show_v4" 0

  # Non-empty guard: a diff of two empty strings proves nothing.
  if [ -n "$BEFORE_LIST" ] && [ -n "$BEFORE_SHOW" ]; then
    check "ZE" "ZE1_nonempty" "PASS"
  else
    check "ZE" "ZE1_nonempty" "FAIL" "fixture reads were empty"
  fi

  # After the first open the DB must be at the CURRENT version: a v4 DB opened
  # by a v6 binary migrates all the way forward in one pass. This assertion
  # tracks currentSchemaVersion deliberately — pinning it to 5 would make the
  # fixture proof stop testing the migration path every later stage adds to.
  run_env "$FXDIR" config --json
  assert_json "ZE" "ZE1_migrated_to_current" ".data.schema_version" "$CURRENT_SCHEMA_VERSION"

  # v5 structures present.
  local NCOL NTBL
  NCOL=$(sqlite3 "$FXDIR/issues.db" "SELECT COUNT(*) FROM pragma_table_info('issues') WHERE name='version';")
  check_cond "ZE" "ZE1_version_column" "issues.version missing" [ "$NCOL" = "1" ]
  NTBL=$(sqlite3 "$FXDIR/issues.db" "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='idempotency_keys';")
  check_cond "ZE" "ZE1_idempotency_table" "idempotency_keys missing" [ "$NTBL" = "1" ]

  # v6 structures present. These are asserted BEFORE the golden diff is
  # trusted: a diff against a database that failed to migrate passes
  # vacuously and would prove nothing.
  local NLEASE MISSING_LEASE=""
  for col in owner token_hash expires_ms attempt; do
    NLEASE=$(sqlite3 "$FXDIR/issues.db" "SELECT COUNT(*) FROM pragma_table_info('issues') WHERE name='$col';")
    if [ "$NLEASE" != "1" ]; then
      MISSING_LEASE="$MISSING_LEASE $col"
    fi
  done
  check_cond "ZE" "ZE1_lease_columns" "missing issues columns:$MISSING_LEASE" [ -z "$MISSING_LEASE" ]

  local NIDX
  NIDX=$(sqlite3 "$FXDIR/issues.db" "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_issues_expires_ms';")
  check_cond "ZE" "ZE1_lease_index" "idx_issues_expires_ms missing" [ "$NIDX" = "1" ]

  # Every migrated row is unclaimed: a v4 repo has never claimed anything, so
  # the whole lease mechanism must be dormant on it.
  local NCLAIMED
  NCLAIMED=$(sqlite3 "$FXDIR/issues.db" "SELECT COUNT(*) FROM issues WHERE owner IS NOT NULL OR attempt != 0;")
  check_cond "ZE" "ZE1_migrated_rows_unclaimed" "$NCLAIMED migrated rows carry lease state" [ "$NCLAIMED" = "0" ]

  # Golden diff: identical output before and after the migration.
  run_env "$FXDIR" issue list --all --json
  AFTER_LIST="$CMD_STDOUT"
  run_env "$FXDIR" issue show DKT-1 --json
  AFTER_SHOW="$CMD_STDOUT"

  check_cond "ZE" "ZE1_golden_list" "issue list changed across migration" [ "$BEFORE_LIST" = "$AFTER_LIST" ]
  check_cond "ZE" "ZE1_golden_show" "issue show changed across migration" [ "$BEFORE_SHOW" = "$AFTER_SHOW" ]

  # Migration is idempotent — a second open must not move the version.
  run_env "$FXDIR" config --json
  assert_json "ZE" "ZE1_idempotent" ".data.schema_version" "$CURRENT_SCHEMA_VERSION"

  rm -rf "$FXDIR"

  # ---------------------------------------------------------------------------
  # Versions in .data (v2 only).
  # ---------------------------------------------------------------------------

  local ZE_ID
  run issue create --json -t "ZE CAS probe"
  assert_exit "ZE" "ZE2_setup" 0
  ZE_ID=$(extract_id)

  # ZE2: v1 carries no version field; v2 does.
  run issue show "$ZE_ID" --json
  assert_json_null "ZE" "ZE2_v1_no_version" ".data.version"
  run issue show "$ZE_ID" --json=v2
  assert_json "ZE" "ZE2_v2_version" ".data.version" "1"

  # ZE3: the version advances on every mutation.
  run issue move "$ZE_ID" todo --json=v2
  assert_exit "ZE" "ZE3_move" 0
  assert_json "ZE" "ZE3_version_2" ".data.version" "2"

  # ZE4: list items carry versions under v2 only.
  run issue list --json=v2
  assert_json_all "ZE" "ZE4_all_versioned" ".data.items" "has(\"version\")"
  run issue list --json
  assert_json_all "ZE" "ZE4_v1_unversioned" ".data.issues" "has(\"version\") | not"

  # ---------------------------------------------------------------------------
  # CAS: --if-version.
  # ---------------------------------------------------------------------------

  # ZE5: a correct --if-version succeeds.
  run issue move "$ZE_ID" review --json=v2 --if-version 2
  assert_exit "ZE" "ZE5_correct" 0
  assert_json "ZE" "ZE5_bumped" ".data.version" "3"

  # ZE6: a stale --if-version is a CONFLICT (exit 4) — the existing code,
  # not a renumbered one.
  run issue move "$ZE_ID" todo --json=v2 --if-version 2
  assert_exit "ZE" "ZE6_conflict" 4
  assert_json "ZE" "ZE6_code" ".code" "CONFLICT"

  # ZE7: the refused write did not apply.
  run issue show "$ZE_ID" --json=v2
  assert_json "ZE" "ZE7_unchanged" ".data.status" "review"
  assert_json "ZE" "ZE7_version" ".data.version" "3"

  # ZE8: --if-version on a missing issue is NOT_FOUND (2), not CONFLICT.
  run issue move 999999 todo --json=v2 --if-version 1
  assert_exit "ZE" "ZE8_notfound" 2

  # ZE9: --if-version < 1 is a VALIDATION_ERROR.
  run issue move "$ZE_ID" todo --json=v2 --if-version 0
  assert_exit "ZE" "ZE9_zero" 3
  run issue move "$ZE_ID" todo --json=v2 --if-version -1
  assert_exit "ZE" "ZE9_negative" 3

  # ZE10: CAS is available on edit/close/reopen too.
  run issue edit "$ZE_ID" --json=v2 --if-version 3 -p high
  assert_exit "ZE" "ZE10_edit" 0
  run issue edit "$ZE_ID" --json=v2 --if-version 3 -p low
  assert_exit "ZE" "ZE10_edit_conflict" 4
  run issue close "$ZE_ID" --json=v2 --if-version 99
  assert_exit "ZE" "ZE10_close_conflict" 4
  run issue close "$ZE_ID" --json=v2
  assert_exit "ZE" "ZE10_close_nocas" 0
  run issue reopen "$ZE_ID" --json=v2 --if-version 99
  assert_exit "ZE" "ZE10_reopen_conflict" 4

  # ZE11: without --if-version the legacy path is unchanged (no CONFLICT).
  run issue move "$ZE_ID" todo --json
  assert_exit "ZE" "ZE11_no_cas" 0

  # ---------------------------------------------------------------------------
  # Idempotency keys on create verbs.
  # ---------------------------------------------------------------------------

  # ZE12: repeating a create with the same key returns the original entity.
  local FIRST SECOND COUNT_BEFORE COUNT_AFTER
  run issue list --json --all
  COUNT_BEFORE=$(echo "$CMD_STDOUT" | jq '.data.total')

  run issue create --json -t "ZE idempotent" --idempotency-key ze-key-1
  assert_exit "ZE" "ZE12_first" 0
  FIRST=$(echo "$CMD_STDOUT" | jq -r '.data.id')

  run issue create --json -t "ZE idempotent" --idempotency-key ze-key-1
  assert_exit "ZE" "ZE12_replay_exit" 0
  SECOND=$(echo "$CMD_STDOUT" | jq -r '.data.id')

  check_cond "ZE" "ZE12_same_id" "replay returned $SECOND, want $FIRST" [ "$FIRST" = "$SECOND" ]

  # Exactly one issue was created by the two calls.
  run issue list --json --all
  COUNT_AFTER=$(echo "$CMD_STDOUT" | jq '.data.total')
  check_cond "ZE" "ZE12_no_duplicate" "count went $COUNT_BEFORE -> $COUNT_AFTER, want +1" [ "$COUNT_AFTER" = "$((COUNT_BEFORE + 1))" ]

  # ZE13: a different key creates a distinct entity.
  run issue create --json -t "ZE idempotent" --idempotency-key ze-key-2
  assert_exit "ZE" "ZE13_other_key" 0
  local THIRD
  THIRD=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  check_cond "ZE" "ZE13_distinct" "different key reused $FIRST" [ "$THIRD" != "$FIRST" ]

  # ZE14: an empty --idempotency-key is a VALIDATION_ERROR.
  run issue create --json -t "ZE empty key" --idempotency-key ""
  assert_exit "ZE" "ZE14_empty" 3

  # ZE15: keys are scoped per verb — the same key on doc create is independent.
  run doc create --json -t "ZE doc" --idempotency-key ze-key-1
  assert_exit "ZE" "ZE15_doc_scope" 0
  local DOC_FIRST DOC_SECOND
  DOC_FIRST=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  run doc create --json -t "ZE doc" --idempotency-key ze-key-1
  DOC_SECOND=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  check_cond "ZE" "ZE15_doc_replay" "doc replay returned $DOC_SECOND, want $DOC_FIRST" [ "$DOC_FIRST" = "$DOC_SECOND" ]

  # ZE16: vote create and both comment verbs replay too.
  run vote create --json -d "ZE proposal" -n 2 --idempotency-key ze-vote
  assert_exit "ZE" "ZE16_vote" 0
  local VOTE_FIRST VOTE_SECOND
  VOTE_FIRST=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  run vote create --json -d "ZE proposal" -n 2 --idempotency-key ze-vote
  VOTE_SECOND=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  check_cond "ZE" "ZE16_vote_replay" "vote replay returned $VOTE_SECOND" [ "$VOTE_FIRST" = "$VOTE_SECOND" ]

  run issue comment add "$FIRST" --json -m "ze comment" --idempotency-key ze-cmt
  assert_exit "ZE" "ZE16_comment" 0
  local CMT_FIRST CMT_SECOND
  CMT_FIRST=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  run issue comment add "$FIRST" --json -m "ze comment" --idempotency-key ze-cmt
  CMT_SECOND=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  check_cond "ZE" "ZE16_comment_replay" "comment replay returned $CMT_SECOND" [ "$CMT_FIRST" = "$CMT_SECOND" ]

  # ZE17: without a key, creates are never deduplicated.
  run issue create --json -t "ZE no key"
  local NK1
  NK1=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  run issue create --json -t "ZE no key"
  local NK2
  NK2=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  check_cond "ZE" "ZE17_no_dedup" "keyless creates collapsed into $NK1" [ "$NK1" != "$NK2" ]
}
