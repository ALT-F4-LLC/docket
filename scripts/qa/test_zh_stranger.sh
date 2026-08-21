#!/usr/bin/env bash
#
# ZH — THE STRANGER TEST, as a script (gates-trust §9.4, engine-spec §9 item 1).
#
# §9 item 1, verbatim:
#
#   Stranger test: a human-only demo — a docs-review workflow with two steps,
#   one fenced-command gate, no agents anywhere — is definable, runnable, and
#   comprehensible from public docs alone, with zero references to AI concepts.
#
# S3 shipped the template and could not run its gate. THIS STAGE MAKES THE DEMO
# REAL: the run reaches `done` on exactly one `trust add`, and nothing in the
# fixture, the commands, or the printed output requires the reader to know what
# an LLM is.
#
# The script uses only what a stranger has: the shipped template, the ordinary
# verbs, and a fenced block holding a command a documentation team would
# actually write.

test_zh_stranger() {
  printf "Section ZH: The stranger test — a human-only run, one approval"

  # SB5 (§9.5): a hard abort, not a skip, if the trust sandbox is not in force.
  # This section WRITES trust entries, and doing that against the operator's
  # real store would modify the machine the suite is auditing.
  assert_trust_sandbox

  local ZH ZH_BANNED
  ZH=$(qa_mktemp_d)

  # ---------------------------------------------------------------------------
  # Step 1-2: the shipped template, registered. One command each.
  # ---------------------------------------------------------------------------
  run_env "$ZH" init
  assert_exit "ZH" "ZH1_init" 0

  run_env "$ZH" workflow init --template standard-dev --json
  assert_exit "ZH" "ZH1_template" 0

  local ZH_WF="$ZH/config/workflows/standard-dev.toml"
  check_cond "ZH" "ZH1_template_written" "no template at $ZH_WF" [ -f "$ZH_WF" ]

  run_env "$ZH" workflow register "$ZH_WF" --json
  assert_exit "ZH" "ZH2_register" 0
  assert_json "ZH" "ZH2_register_name" '.data.name' "standard-dev"

  # ---------------------------------------------------------------------------
  # Step 3: an issue whose body carries a ```checks fenced block holding a REAL,
  # ORDINARY command — a spell-check-shaped grep over the docs it changes.
  #
  # No agents, no models, nothing AI-shaped anywhere in the fixture. That is the
  # whole point of the section, and step 9 checks it mechanically.
  # ---------------------------------------------------------------------------
  local ZH_DOCS="$ZH/docs"
  mkdir -p "$ZH_DOCS"
  printf 'The quick brown fox.\nNo spelling mistakes here.\n' >"$ZH_DOCS/guide.md"

  # The witness: the command must be one whose EXECUTION is observable, so
  # "the gate ran" is proven by the filesystem rather than by a row the engine
  # wrote about itself.
  local ZH_SENTINEL="$ZH/checks-ran"
  local ZH_BODY
  ZH_BODY=$(printf 'Proof-read the new guide before it ships.\n\n```checks\n/usr/bin/touch %s\n```\n' "$ZH_SENTINEL")

  run_env "$ZH" issue create -t "Proof-read the guide" -d "$ZH_BODY" --json
  assert_exit "ZH" "ZH3_issue" 0

  # ---------------------------------------------------------------------------
  # Step 4: start and activate. The §7.7 report prints the fenced command
  # VERBATIM and marks it `unmatched` — the operator sees what a run would
  # invoke before it invokes anything.
  # ---------------------------------------------------------------------------
  run_env "$ZH" run start --issue DKT-1 --json
  assert_exit "ZH" "ZH4_start" 0

  run_env "$ZH" run activate RUN-1
  assert_exit "ZH" "ZH4_activate" 0

  # The report goes to stderr in human mode.
  if printf '%s' "$CMD_STDERR" | grep -q "touch"; then
    check "ZH" "ZH4_report_verbatim" "PASS"
  else
    check "ZH" "ZH4_report_verbatim" "FAIL" \
      "the activation report did not print the harvested command"
  fi
  if printf '%s' "$CMD_STDERR" | grep -q "unmatched"; then
    check "ZH" "ZH4_report_unmatched" "PASS"
  else
    check "ZH" "ZH4_report_unmatched" "FAIL" \
      "the harvested command is not reported unmatched before any trust add"
  fi

  # Nothing has executed. The gate has not even been reached yet, and the
  # report is a REPORT — activation succeeded despite the unmatched command.
  check_cond "ZH" "ZH4_nothing_executed_yet" "something executed during activation" [ ! -e "$ZH_SENTINEL" ]

  # ---------------------------------------------------------------------------
  # Step 5: THE ONE APPROVAL IN THE WHOLE SCRIPT.
  #
  # `docket trust add` is the only place a human authorizes execution, and the
  # argv and repo binding are printed even though nothing prompted.
  # ---------------------------------------------------------------------------
  run_env "$ZH" trust add checks --yes -- /usr/bin/touch "$ZH_SENTINEL"
  assert_exit "ZH" "ZH5_trust_add" 0

  if printf '%s' "$CMD_STDERR" | grep -q "touch"; then
    check "ZH" "ZH5_discloses_argv" "PASS"
  else
    check "ZH" "ZH5_discloses_argv" "FAIL" \
      "trust add did not disclose the argv it approved"
  fi
  # The binding is disclosed too, so the operator knows WHERE this applies.
  if printf '%s' "$CMD_STDERR" | grep -q "$ZH"; then
    check "ZH" "ZH5_discloses_binding" "PASS"
  else
    check "ZH" "ZH5_discloses_binding" "FAIL" \
      "trust add did not disclose the repository it bound to"
  fi

  # ---------------------------------------------------------------------------
  # Step 6: next -> claim -> complete. The gate RUNS.
  # ---------------------------------------------------------------------------
  run_env "$ZH" next --run RUN-1 --json
  assert_exit "ZH" "ZH6_next" 0
  assert_json "ZH" "ZH6_next_instance" '.data.steps[0].instance' "check@0"

  local ZH_TOKEN
  run_env "$ZH" step claim STEP-1 --owner reviewer --json
  assert_exit "ZH" "ZH6_claim" 0
  ZH_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')

  printf 'Proof-read; no changes needed.\n' >"$ZH/report.txt"
  DOCKET_TOKEN="$ZH_TOKEN" run_env "$ZH" step complete STEP-1 \
    --artifact-file "$ZH/report.txt" --json
  assert_exit "ZH" "ZH6_complete" 0

  # THE SENTINEL: a real process ran.
  check_cond "ZH" "ZH6_gate_executed" "the trusted gate did not execute — no sentinel" [ -e "$ZH_SENTINEL" ]

  # And the recorded result is REAL: a real argv, a real exit code, and `stub`
  # absent. `stub` marks an S3 pass-through result and nothing at this stage is.
  local ZH_GATE
  ZH_GATE=$(sqlite3 "$ZH/issues.db" \
    "SELECT verdict || '|' || COALESCE(exit,'NULL') || '|' || stub
       FROM gate_results ORDER BY id LIMIT 1;")
  if [ "$ZH_GATE" = "pass|0|0" ]; then
    check "ZH" "ZH6_gate_result_real" "PASS"
  else
    check "ZH" "ZH6_gate_result_real" "FAIL" \
      "verdict|exit|stub = $ZH_GATE, want pass|0|0"
  fi

  local ZH_ARGV
  ZH_ARGV=$(sqlite3 "$ZH/issues.db" \
    "SELECT COALESCE(argv,'NULL') FROM gate_results ORDER BY id LIMIT 1;")
  case "$ZH_ARGV" in
    *touch*) check "ZH" "ZH6_gate_argv_recorded" "PASS" ;;
    *) check "ZH" "ZH6_gate_argv_recorded" "FAIL" "argv = $ZH_ARGV" ;;
  esac

  # ---------------------------------------------------------------------------
  # Step 7: a person approves the human gate.
  # ---------------------------------------------------------------------------
  local ZH_APPROVE
  ZH_APPROVE=$(sqlite3 "$ZH/issues.db" \
    "SELECT id FROM steps WHERE instance='approve@0';")
  run_env "$ZH" step approve "STEP-$ZH_APPROVE" --json
  assert_exit "ZH" "ZH7_approve" 0

  # ---------------------------------------------------------------------------
  # Step 8: THE RUN REACHES `done`.
  # ---------------------------------------------------------------------------
  local ZH_RUN_STATUS
  ZH_RUN_STATUS=$(sqlite3 "$ZH/issues.db" "SELECT status FROM runs WHERE id=1;")
  check_cond "ZH" "ZH8_run_done" "run is $ZH_RUN_STATUS after one trust add and one approval; want done" [ "$ZH_RUN_STATUS" = "done" ]

  # ---------------------------------------------------------------------------
  # Step 9: THE COMPREHENSIBILITY HALF, MECHANIZED.
  #
  # Every string this section printed, and the script's own commands, contain
  # NONE of the genericity gate's banned terms — and the list is SOURCED FROM
  # THE GATE SCRIPT ITSELF rather than restated here. Restating it is how the
  # gate erodes: the two copies drift, and the weaker one is the one that runs.
  # ---------------------------------------------------------------------------
  local ZH_BANNED_SRC="$SCRIPT_DIR/qa/genericity.sh"
  if [ ! -f "$ZH_BANNED_SRC" ]; then
    check "ZH" "ZH9_banned_list_sourced" "FAIL" "no genericity.sh to source from"
  else
    # Pull the BANNED array out of the gate's own source. It is declared at
    # the section's scope, not this block's, because step 11 scans the ordered
    # demo against THE SAME list — sourced once, from the gate, so the two
    # scans cannot drift from each other or from the gate.
    ZH_BANNED=$(awk '/^BANNED=\(/{f=1;next} /^\)/{f=0} f{gsub(/[ \t]/,"");if($0!="")print}' \
      "$ZH_BANNED_SRC")
    if [ -z "$ZH_BANNED" ]; then
      check "ZH" "ZH9_banned_list_sourced" "FAIL" \
        "could not read BANNED from genericity.sh"
    else
      check "ZH" "ZH9_banned_list_sourced" "PASS"

      # What a stranger actually SEES: the workflow they registered, the
      # template's own text, and this run's rendered output.
      local ZH_SEEN="$ZH/seen.txt"
      : >"$ZH_SEEN"
      cat "$ZH_WF" >>"$ZH_SEEN"
      run_env "$ZH" run status RUN-1 >>"$ZH_SEEN" 2>&1 || true
      run_env "$ZH" workflow show standard-dev >>"$ZH_SEEN" 2>&1 || true
      run_env "$ZH" next --run RUN-1 >>"$ZH_SEEN" 2>&1 || true
      run_env "$ZH" trust list >>"$ZH_SEEN" 2>&1 || true

      local ZH_HITS=""
      local ZH_TERM
      while IFS= read -r ZH_TERM; do
        [ -z "$ZH_TERM" ] && continue
        if grep -qiw "$ZH_TERM" "$ZH_SEEN"; then
          ZH_HITS="$ZH_HITS $ZH_TERM"
        fi
      done <<<"$ZH_BANNED"

      check_cond "ZH" "ZH9_no_ai_vocabulary" "the stranger demo surfaces:$ZH_HITS" [ -z "$ZH_HITS" ]
    fi
  fi

  # ---------------------------------------------------------------------------
  # Step 11: THE ORDERED DEMO (§8.3). The stranger test re-earned over this
  # stage's headline verb.
  #
  # §1.1 states the sentence a stranger reads:
  #
  #   "I registered a file that says my `risk` field goes low, medium, high.
  #   Now my workflow can say `any(risk >= medium)` and Docket routes on it."
  #
  # No agent, no model, nothing AI-shaped — in the schema, in the workflow, in
  # the payload, or in anything the run printed. `risk` is an opaque token to
  # core; the ORDER is the author's, declared in two lines of JSON.
  # ---------------------------------------------------------------------------
  local ZH_O
  ZH_O=$(qa_mktemp_d)
  run_env "$ZH_O" init
  assert_exit "ZH" "ZH11_init" 0

  # The schema a stranger writes: one field, three values, one annotation.
  cat >"$ZH_O/risk.json" <<'ZHRISKEOF'
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "risk": {
        "type": "string",
        "enum": ["low", "medium", "high"],
        "ordered_enum": true
      }
    },
    "required": ["risk"]
  }
}
ZHRISKEOF

  run_env "$ZH_O" schema register risk-report@1 "$ZH_O/risk.json" --json
  assert_exit "ZH" "ZH11_schema_register" 0
  assert_json "ZH" "ZH11_ordered_field" '.data.ordered_fields | join(",")' "risk"

  # The workflow: a docs-review pipeline whose assessment routes to a HUMAN
  # APPROVAL step when the risk reaches `medium`.
  cat >"$ZH_O/review.toml" <<'ZHREVIEWEOF'
[pipeline]
name    = "docs-review"
version = 1

[match]
kind = ["task"]

[[step]]
name     = "assess"
after    = []
executor = "editor"
emits    = "risk-report"
payload  = "risk-report@1"
threshold = { "sign-off" = "any(risk >= medium)" }

[[step]]
name    = "sign-off"
after   = ["assess"]
type    = "human"
on_fail = "skip"

[[step]]
name     = "publish"
after    = ["sign-off"]
executor = "publisher"
emits    = "publication"
ZHREVIEWEOF

  run_env "$ZH_O" workflow register "$ZH_O/review.toml" --json
  assert_exit "ZH" "ZH11_register" 0

  run_env "$ZH_O" issue create -t "Publish the release notes" -d "a body" --json
  assert_exit "ZH" "ZH11_issue" 0
  run_env "$ZH_O" run start --issue DKT-1 --json >/dev/null
  run_env "$ZH_O" run activate RUN-1 --json
  assert_exit "ZH" "ZH11_activate" 0

  # THE ORDERED COMPARISON, EVALUATED. `high` is at position 2 and `medium` at
  # position 1 because the registered file said so, and for no other reason.
  printf 'The notes are ready.\n' >"$ZH_O/art.txt"
  printf '[{"risk":"low"},{"risk":"high"}]\n' >"$ZH_O/report.json"

  run_env "$ZH_O" step claim STEP-1 --owner editor --json
  assert_exit "ZH" "ZH11_claim" 0
  local ZH_O_TOKEN
  ZH_O_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  DOCKET_TOKEN="$ZH_O_TOKEN" run_env "$ZH_O" step complete STEP-1 \
    --artifact-file "$ZH_O/art.txt" --payload-file "$ZH_O/report.json" --json
  assert_exit "ZH" "ZH11_complete" 0

  local ZH_O_ROUTE
  ZH_O_ROUTE=$(sqlite3 "$ZH_O/issues.db" "SELECT routing FROM steps WHERE instance='assess@0';")
  if [ "$ZH_O_ROUTE" = "sign-off" ]; then
    check "ZH" "ZH11_ordered_routes" "PASS"
  else
    check "ZH" "ZH11_ordered_routes" "FAIL" \
      "assess@0 routed '$ZH_O_ROUTE', want sign-off — any(risk >= medium) over " \
      "a payload containing `high`, ordered by the file the stranger registered"
  fi

  # The NEGATIVE half, so the routing above is a decision rather than a default:
  # a report whose risks are all below `medium` does NOT route to the approval.
  local ZH_O2
  ZH_O2=$(qa_mktemp_d)
  run_env "$ZH_O2" init >/dev/null
  run_env "$ZH_O2" schema register risk-report@1 "$ZH_O/risk.json" --json >/dev/null
  run_env "$ZH_O2" workflow register "$ZH_O/review.toml" --json >/dev/null
  run_env "$ZH_O2" issue create -t "A quiet change" -d "a body" --json >/dev/null
  run_env "$ZH_O2" run start --issue DKT-1 --json >/dev/null
  run_env "$ZH_O2" run activate RUN-1 --json >/dev/null

  printf '[{"risk":"low"},{"risk":"low"}]\n' >"$ZH_O2/report.json"
  run_env "$ZH_O2" step claim STEP-1 --owner editor --json
  local ZH_O2_TOKEN
  ZH_O2_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  DOCKET_TOKEN="$ZH_O2_TOKEN" run_env "$ZH_O2" step complete STEP-1 \
    --artifact-file "$ZH_O/art.txt" --payload-file "$ZH_O2/report.json" --json
  assert_exit "ZH" "ZH11_low_complete" 0

  local ZH_O2_ROUTE
  ZH_O2_ROUTE=$(sqlite3 "$ZH_O2/issues.db" "SELECT routing FROM steps WHERE instance='assess@0';")
  check_cond "ZH" "ZH11_ordered_negative" "assess@0 routed '$ZH_O2_ROUTE' over all-low risks, want pass" [ "$ZH_O2_ROUTE" = "pass" ]

  # And a value the stranger's file does NOT declare is refused at `complete`,
  # path-precisely — the schema is doing real work, not decorating.
  printf '[{"risk":"catastrophic"}]\n' >"$ZH_O2/bad.json"
  run_env "$ZH_O2" step claim STEP-2 --owner editor --json >/dev/null 2>&1 || true
  run_env "$ZH_O2" step complete STEP-1 \
    --artifact-file "$ZH_O/art.txt" --payload-file "$ZH_O2/bad.json" --json
  assert_exit_nonzero "ZH" "ZH11_undeclared_value_refused"

  # THE COMPREHENSIBILITY HALF, over the ordered demo, with the banned list
  # SOURCED FROM THE GATE ITSELF exactly as step 9 sources it.
  if [ -n "${ZH_BANNED:-}" ]; then
    local ZH_O_SEEN="$ZH_O/seen.txt"
    : >"$ZH_O_SEEN"
    cat "$ZH_O/risk.json" "$ZH_O/review.toml" "$ZH_O/report.json" >>"$ZH_O_SEEN"
    run_env "$ZH_O" schema show risk-report@1 >>"$ZH_O_SEEN" 2>&1 || true
    run_env "$ZH_O" schema list >>"$ZH_O_SEEN" 2>&1 || true
    run_env "$ZH_O" workflow show docs-review >>"$ZH_O_SEEN" 2>&1 || true
    run_env "$ZH_O" run status RUN-1 >>"$ZH_O_SEEN" 2>&1 || true
    run_env "$ZH_O" next --run RUN-1 >>"$ZH_O_SEEN" 2>&1 || true

    local ZH_O_HITS="" ZH_O_TERM
    while IFS= read -r ZH_O_TERM; do
      [ -z "$ZH_O_TERM" ] && continue
      if grep -qiw "$ZH_O_TERM" "$ZH_O_SEEN"; then
        ZH_O_HITS="$ZH_O_HITS $ZH_O_TERM"
      fi
    done <<<"$ZH_BANNED"

    check_cond "ZH" "ZH11_no_ai_vocabulary" "the ordered demo surfaces:$ZH_O_HITS" [ -z "$ZH_O_HITS" ]
  else
    check "ZH" "ZH11_no_ai_vocabulary" "FAIL" \
      "the banned list was not sourced from genericity.sh"
  fi

  rm -rf "$ZH_O" "$ZH_O2"

  # ---------------------------------------------------------------------------
  # Step 10: THE NEGATIVE HALF — §9 item 6 folded into the stranger demo, since
  # it is the same script with one step removed.
  #
  # A COPY of the repo at a different path, with no `trust add` there. The
  # entry above is bound to the original, so the gate is unmatched and nothing
  # executes.
  # ---------------------------------------------------------------------------
  local ZH_CLONE
  ZH_CLONE=$(qa_mktemp_d)
  local ZH_CLONE_SENTINEL="$ZH_CLONE/checks-ran"

  run_env "$ZH_CLONE" init
  assert_exit "ZH" "ZH10_clone_init" 0

  # ---------------------------------------------------------------------------
  # THE HOSTILE CONFIG DIRECTORY (runs-dispatch §9.5), which is why this check
  # is EXTENDED at S6 rather than left alone.
  #
  # Auto-registration means activation now READS AND REGISTERS a workflow it
  # found in the repo — and "activation registers a workflow it found in the
  # repo" is precisely the sentence that SOUNDS like drive-by execution. It is
  # not, and this is the proof: the clone ships a definition the operator never
  # wrote, declaring a gate whose fenced command would touch a sentinel. Core
  # reads it, hashes it, and REGISTERS it; then every one of its gates is
  # reported `unmatched` and NOT EXECUTED, because the trust entry is bound to
  # the ORIGINAL repo and this is a different path.
  #
  # Without this extension, S6 would widen the attack surface and the existing
  # check would not notice — it would keep passing over a repo whose only
  # workflow arrived through a verb the operator typed.
  # ---------------------------------------------------------------------------
  # The clone ships ONLY its own definition — no scaffolded template beside it.
  # That is both the more honest scenario (a hostile repo carries what its author
  # put there) and the one with an unambiguous binding: a shipped template
  # matching the same issue would make activation fail for having two candidates,
  # which is a different refusal from the one under test.
  local ZH_EVIL_SENTINEL="$ZH_CLONE/evil-ran"
  mkdir -p "$ZH_CLONE/config/workflows"
  cat >"$ZH_CLONE/config/workflows/evil.toml" <<TOML
[pipeline]
name        = "evil"
version     = 1
description = "A definition the operator never wrote, shipped by a clone."

[match]
labels_any = ["clone-bait"]

[[step]]
name     = "payload"
after    = []
executor = "whoever"
emits    = "out"
gates    = [{ name = "evil-check", source = "fence:evil" }]
TOML

  # The clone's definition matches on a LABEL the shipped templates do not
  # claim, so exactly one workflow binds. Matching on `kind` would collide with
  # `standard-dev` — which the scaffold above also auto-registers — and the
  # activation would fail for having two candidates rather than for anything
  # this check is about.
  local ZH_EVIL_BODY
  ZH_EVIL_BODY=$(printf 'A chore the clone wants run.\n\n```evil\n/usr/bin/touch %s\n```\n' "$ZH_EVIL_SENTINEL")
  run_env "$ZH_CLONE" issue create -t "A clone chore" -d "$ZH_EVIL_BODY" \
    --type chore --label clone-bait --json
  assert_exit "ZH" "ZH10_evil_issue" 0
  run_env "$ZH_CLONE" run start --issue DKT-1 --json >/dev/null
  run_env "$ZH_CLONE" run activate RUN-1 --json
  assert_exit "ZH" "ZH10_evil_activates" 0

  # IT WAS REGISTERED — the honest half. Core does not refuse a definition for
  # being unfamiliar; refusing would make a cloned repo unusable and would put
  # core in the business of judging content it is specified not to read.
  local ZH_EVIL_ROWS
  ZH_EVIL_ROWS=$(sqlite3 "$ZH_CLONE/issues.db" \
    "SELECT COUNT(*) FROM workflows WHERE name = 'evil';")
  check_cond "ZH" "ZH10_evil_registered" "the clone's definition was not auto-registered; the inertness proof below is vacuous" [ "$ZH_EVIL_ROWS" = "1" ]

  # AND IT IS INERT. The gate is `unmatched` and NOTHING EXECUTED: registration
  # is reading and hashing, and the trust posture is unchanged by it.
  run_env "$ZH_CLONE" next --run RUN-1 --json
  local ZH_EVIL_STEP
  ZH_EVIL_STEP=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.steps[0].step')
  run_env "$ZH_CLONE" step claim "$ZH_EVIL_STEP" --owner whoever --json
  local ZH_EVIL_TOKEN
  ZH_EVIL_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  printf 'nothing to see\n' >"$ZH_CLONE/evil-report.txt"
  DOCKET_TOKEN="$ZH_EVIL_TOKEN" run_env "$ZH_CLONE" step complete "$ZH_EVIL_STEP" \
    --artifact-file "$ZH_CLONE/evil-report.txt" --json >/dev/null 2>&1 || true

  if [ ! -e "$ZH_EVIL_SENTINEL" ]; then
    check "ZH" "ZH10_evil_executed_nothing" "PASS"
  else
    check "ZH" "ZH10_evil_executed_nothing" "FAIL" \
      "an AUTO-REGISTERED workflow from a clone EXECUTED its fenced command; " \
      "auto-registration must read and hash, never execute"
  fi

  local ZH_EVIL_VERDICT
  ZH_EVIL_VERDICT=$(sqlite3 "$ZH_CLONE/issues.db" \
    "SELECT COALESCE(verdict,'none') FROM gate_results
      WHERE gate = 'evil-check' ORDER BY id LIMIT 1;")
  if [ "$ZH_EVIL_VERDICT" = "unmatched" ] || [ "$ZH_EVIL_VERDICT" = "none" ]; then
    check "ZH" "ZH10_evil_unmatched" "PASS"
  else
    check "ZH" "ZH10_evil_unmatched" "FAIL" \
      "the clone's gate verdict is '$ZH_EVIL_VERDICT', want unmatched"
  fi

  # The clone is reset for the ORIGINAL ZH10 check below, which is about the
  # operator's OWN workflow in a copied repo rather than a smuggled one. The
  # hostile definition and the database both go, so the check that follows runs
  # against a clean copy exactly as it did before this extension.
  rm -f "$ZH_CLONE/config/workflows/evil.toml"
  rm -f "$ZH_CLONE/issues.db" "$ZH_CLONE/issues.db-wal" "$ZH_CLONE/issues.db-shm"
  run_env "$ZH_CLONE" init >/dev/null
  assert_exit "ZH" "ZH10_clone_reset" 0
  run_env "$ZH_CLONE" workflow init --template standard-dev --json
  assert_exit "ZH" "ZH10_clone_scaffold" 0

  local ZH_CLONE_BODY
  ZH_CLONE_BODY=$(printf 'Proof-read the new guide before it ships.\n\n```checks\n/usr/bin/touch %s\n```\n' "$ZH_CLONE_SENTINEL")
  run_env "$ZH_CLONE" issue create -t "Proof-read the guide" -d "$ZH_CLONE_BODY" --json
  run_env "$ZH_CLONE" run start --issue DKT-1 --json
  run_env "$ZH_CLONE" run activate RUN-1 --json
  assert_exit "ZH" "ZH10_clone_activate" 0

  run_env "$ZH_CLONE" step claim STEP-1 --owner reviewer --json
  assert_exit "ZH" "ZH10_clone_claim" 0
  local ZH_CLONE_TOKEN
  ZH_CLONE_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')

  DOCKET_TOKEN="$ZH_CLONE_TOKEN" run_env "$ZH_CLONE" step complete STEP-1 \
    --artifact-file "$ZH/report.txt" --json
  assert_exit "ZH" "ZH10_clone_complete" 0

  # NOTHING EXECUTED in the copy.
  check_cond "ZH" "ZH10_clone_executed_nothing" "the gate EXECUTED in a copy with no trust entry of its own" [ ! -e "$ZH_CLONE_SENTINEL" ]

  local ZH_CLONE_VERDICT
  ZH_CLONE_VERDICT=$(sqlite3 "$ZH_CLONE/issues.db" \
    "SELECT verdict || '|' || COALESCE(argv,'NULL') || '|' || COALESCE(exit,'NULL')
       FROM gate_results ORDER BY id LIMIT 1;")
  if [ "$ZH_CLONE_VERDICT" = "unmatched|NULL|NULL" ]; then
    check "ZH" "ZH10_clone_unmatched" "PASS"
  else
    check "ZH" "ZH10_clone_unmatched" "FAIL" \
      "verdict|argv|exit = $ZH_CLONE_VERDICT, want unmatched|NULL|NULL"
  fi

  # ---------------------------------------------------------------------------
  # The `trust` refusal matrix, by EXIT CODE and by `.code` — the §6.9-style
  # table the ZG extensions ask for, placed here because this is the section
  # that already holds the trust sandbox.
  #
  # Each refusal is followed by a STORE-UNCHANGED assertion, for the same reason
  # ZG14 asserts version-unchanged: A REFUSAL NEVER WRITES. A validation error
  # that also mutated the allowlist would be the worst of both.
  # ---------------------------------------------------------------------------
  local ZH_STORE="$XDG_CONFIG_HOME/docket/trust.toml"
  local ZH_BEFORE ZH_AFTER
  zh_store_hash() {
    if [ -f "$ZH_STORE" ]; then shasum "$ZH_STORE" | cut -d' ' -f1; else echo "absent"; fi
  }

  # R1: no argv after the `--` boundary.
  ZH_BEFORE=$(zh_store_hash)
  run_env "$ZH" trust add lonely --yes --json
  assert_exit "ZH" "ZHR1_no_argv_exit" 3
  assert_json "ZH" "ZHR1_no_argv_code" ".code" "VALIDATION_ERROR"
  ZH_AFTER=$(zh_store_hash)
  check_cond "ZH" "ZHR1_no_write" "the store changed on a refusal" [ "$ZH_BEFORE" = "$ZH_AFTER" ]

  # R2: a different argv at an existing name+repo is a CONFLICT, never a
  # silent overwrite — a trusted name's meaning must not change unseen.
  ZH_BEFORE=$(zh_store_hash)
  run_env "$ZH" trust add checks --yes --json -- /usr/bin/true
  assert_exit "ZH" "ZHR2_conflict_exit" 4
  assert_json "ZH" "ZHR2_conflict_code" ".code" "CONFLICT"
  ZH_AFTER=$(zh_store_hash)
  check_cond "ZH" "ZHR2_no_write" "a CONFLICT overwrote the entry" [ "$ZH_BEFORE" = "$ZH_AFTER" ]

  # R3: removing an entry that does not exist here is NOT_FOUND.
  run_env "$ZH" trust rm nonexistent --json
  assert_exit "ZH" "ZHR3_rm_missing_exit" 2
  assert_json "ZH" "ZHR3_rm_missing_code" ".code" "NOT_FOUND"

  # R4: an identical re-add is IDEMPOTENT — success, nothing written.
  ZH_BEFORE=$(zh_store_hash)
  run_env "$ZH" trust add checks --yes --json -- /usr/bin/touch "$ZH_SENTINEL"
  assert_exit "ZH" "ZHR4_idempotent_exit" 0
  assert_json "ZH" "ZHR4_idempotent_flag" ".data.idempotent" "true"
  ZH_AFTER=$(zh_store_hash)
  check_cond "ZH" "ZHR4_no_write" "an idempotent add rewrote the store" [ "$ZH_BEFORE" = "$ZH_AFTER" ]

  # R5: the --prefix warning is emitted EVEN UNDER --yes, and rides in the
  # JSON response. Suppressing it is exactly what would make the
  # conversational posture unsafe.
  run_env "$ZH" trust add anything --yes --prefix --json -- /usr/bin/true
  assert_exit "ZH" "ZHR5_prefix_exit" 0
  assert_json_exists "ZH" "ZHR5_prefix_warning" ".data.warnings[0]"
  if printf '%s' "$CMD_STDOUT" | grep -qi "authorizes ANY command"; then
    check "ZH" "ZHR5_warning_text" "PASS"
  else
    check "ZH" "ZHR5_warning_text" "FAIL" \
      "the over-authorization warning is not in the response"
  fi

  # ---------------------------------------------------------------------------
  # DORMANCY (§9.3, group 2). "A repo with no trusted commands and no workflows
  # behaves byte-identically on every pre-existing verb."
  #
  # This stage's dormancy claim is DISTINCT from S3's, and the distinction is
  # the point: the mechanism is inert BY DEFAULT because the allowlist is empty
  # by default. A stranger who installs docket and never runs `trust add` has a
  # tool that cannot execute anything.
  # ---------------------------------------------------------------------------
  local ZH_DORMANT
  ZH_DORMANT=$(qa_mktemp_d)
  run_env "$ZH_DORMANT" init
  assert_exit "ZH" "ZHD_init" 0

  # v8's two tables exist — the migration ran — and both are EMPTY.
  local ZH_GR ZH_TC
  ZH_GR=$(sqlite3 "$ZH_DORMANT/issues.db" "SELECT COUNT(*) FROM gate_results;" 2>/dev/null || echo ERR)
  ZH_TC=$(sqlite3 "$ZH_DORMANT/issues.db" "SELECT COUNT(*) FROM trust_cache;" 2>/dev/null || echo ERR)
  if [ "$ZH_GR" = "0" ] && [ "$ZH_TC" = "0" ]; then
    check "ZH" "ZHD_v8_tables_empty" "PASS"
  else
    check "ZH" "ZHD_v8_tables_empty" "FAIL" \
      "gate_results=$ZH_GR trust_cache=$ZH_TC in a fresh repo; want 0 and 0"
  fi

  # The schema really is at the current version, so the emptiness above is
  # dormancy rather than a migration that never ran. The number is read from the
  # source of truth rather than restated here, so a stage that bumps the schema
  # does not leave this proof asserting a stale one.
  local ZH_SCHEMA
  ZH_SCHEMA=$(sqlite3 "$ZH_DORMANT/issues.db" \
    "SELECT value FROM meta WHERE key='schema_version';")
  check_cond "ZH" "ZHD_schema_current" "schema_version=$ZH_SCHEMA, want $CURRENT_SCHEMA_VERSION" [ "$ZH_SCHEMA" = "$CURRENT_SCHEMA_VERSION" ]

  # A GATE-FREE WORKFLOW RUN SPAWNS ZERO PROCESSES.
  #
  # The witness is a directory that stays empty: the workflow declares no gate,
  # so nothing in the run has anything to execute. A run that spawned would
  # have to spawn SOMETHING, and there is nothing here for it to spawn.
  local ZH_NOSPAWN="$ZH_DORMANT/spawned"
  mkdir -p "$ZH_NOSPAWN"
  cat >"$ZH_DORMANT/gateless.toml" <<'WFEOF'
[pipeline]
name    = "gateless"
version = 1

[match]
kind = ["task"]

[[step]]
name     = "work"
after    = []
executor = "author"
emits    = "note"
WFEOF
  run_env "$ZH_DORMANT" workflow register "$ZH_DORMANT/gateless.toml" --json
  assert_exit "ZH" "ZHD_register" 0
  run_env "$ZH_DORMANT" issue create -t "no gates here" -d "a body" --json
  run_env "$ZH_DORMANT" run start --issue DKT-1 --json
  run_env "$ZH_DORMANT" run activate RUN-1 --json
  assert_exit "ZH" "ZHD_activate" 0

  run_env "$ZH_DORMANT" step claim STEP-1 --owner w --json
  assert_exit "ZH" "ZHD_claim" 0
  local ZH_D_TOKEN
  ZH_D_TOKEN=$(printf '%s' "$CMD_STDOUT" | jq -r '.data.token')
  printf 'done\n' >"$ZH_DORMANT/art.txt"
  DOCKET_TOKEN="$ZH_D_TOKEN" run_env "$ZH_DORMANT" step complete STEP-1 \
    --artifact-file "$ZH_DORMANT/art.txt" --json
  assert_exit "ZH" "ZHD_complete" 0

  # No gate ⇒ no gate_results row, and nothing was executed.
  local ZH_D_GR
  ZH_D_GR=$(sqlite3 "$ZH_DORMANT/issues.db" "SELECT COUNT(*) FROM gate_results;")
  check_cond "ZH" "ZHD_gateless_run_spawns_nothing" "a gate-free run recorded $ZH_D_GR gate results" [ "$ZH_D_GR" = "0" ]

  # And the run still completed: dormancy is "nothing executes", not
  # "nothing works".
  local ZH_D_RUN
  ZH_D_RUN=$(sqlite3 "$ZH_DORMANT/issues.db" "SELECT status FROM runs WHERE id=1;")
  check_cond "ZH" "ZHD_gateless_run_completes" "the gate-free run is $ZH_D_RUN, want done" [ "$ZH_D_RUN" = "done" ]

  rm -rf "$ZH" "$ZH_CLONE" "$ZH_DORMANT"
}
