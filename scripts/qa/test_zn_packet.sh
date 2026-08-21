#!/usr/bin/env bash
#
# ZN — PACKET COMPOSITION: DECLARED FILES REACH THE PACKET
# (docs/tdd/packet-composition.md).
#
# THIS SECTION IS THE LITERAL INVERSE OF THE ORIGINAL OBSERVATION, which
# recorded that the shipped template emits
#
#   == PINNED
#   <path>  <sha256>
#
# — A PATH AND A CHECKSUM, NEVER THE BYTES AT THAT PATH. An executor received a
# filename for the document it was supposed to work from, and engine-core §8's
# own property ("nothing in it is optional — no pointers, no 'read if needed'")
# was inverted by the thing that shipped.
#
# So the headline assertion is that a rendered packet CONTAINS THE FILE'S BODY
# TEXT. The Go tests prove the resolution ladder and the frontmatter rules; this
# proves the wire, through the same verbs an instance uses.
#
# IT ALSO PROVES F1 — the defect the first design draft could not express. A
# `fanout` step is ONE step whose siblings carry different hints, so a literal
# entry serves them all identically. Both variants are executed here:
#
#   divergent — `{executor}` substitution, one file per sibling
#   shared    — a literal entry, every sibling reading the same file
#
# THE STRANGER TEST (§1.1). The fixture is a print shop: proofreaders working
# from checklists, one of which declares a shared house-style fragment. No
# agents anywhere; the packet is "here is your job, and here is the checklist
# you work from" — which is what a person would want too.

test_zn_packet() {
  printf "Section ZN: Packet composition (DKT-70)"

  # SB5: this section activates runs against a config directory, so the sandbox
  # is asserted before anything runs.
  assert_trust_sandbox

  # DOCKET_PATH is the temp dir itself, so the instance-config directory is
  # `$ZN/config/` — the same shape ZK's own config fixtures use.
  local ZN ZN_CFG
  ZN=$(qa_mktemp_d)
  ZN_CFG="$ZN/config"
  mkdir -p "$ZN_CFG/workflows" "$ZN_CFG/checklists" "$ZN_CFG/fragments"

  # ---------------------------------------------------------------------------
  # THE CORPUS. `packet_includes` is the ONE frontmatter key the engine reads;
  # every other key is ignored entirely, which is what keeps the concession
  # bounded. `title:` below is present precisely to prove it is ignored.
  # ---------------------------------------------------------------------------
  cat >"$ZN_CFG/checklists/proofreader-copy.md" <<'MD'
---
title: Copy proofing
packet_includes:
  - fragments/house-style.md
---
Check spelling, grammar, and the caption on every plate.
MD

  cat >"$ZN_CFG/checklists/proofreader-colour.md" <<'MD'
---
title: Colour proofing
---
Check ink density against the reference swatch.
MD

  cat >"$ZN_CFG/checklists/shared-proofing.md" <<'MD'
Every proofreader initials the sheet before it leaves the desk.
MD

  cat >"$ZN_CFG/fragments/house-style.md" <<'MD'
House style: serial commas, no widows, captions sentence-case.
MD

  cat >"$ZN_CFG/workflows/printshop.toml" <<'TOML'
[pipeline]
name = "print-shop-packet"
version = 1
description = "Proof a run of sheets, each desk working from its own checklist."

[match]
kind = ["task"]

[[step]]
name = "proof"
fanout = ["proofreader-copy", "proofreader-colour"]
emits = "notes"
after = []
packet = ["checklists/{executor}.md"]

[[step]]
name = "sign-off"
fanout = ["proofreader-copy", "proofreader-colour"]
emits = "notes"
after = ["proof"]
packet = ["checklists/shared-proofing.md"]
TOML

  # Auto-registration picks the workflow up and pins the corpus; no `workflow
  # register` is invoked, which is item 11's rule and this section obeys it.
  run_env "$ZN" init
  assert_exit "ZN" "ZN0_init" 0

  run_env "$ZN" issue create -t "Proof the spring catalogue" -d "500 copies" --json
  assert_exit "ZN" "ZN0_issue" 0
  run_env "$ZN" run start --issue DKT-1 --json
  assert_exit "ZN" "ZN0_start" 0
  run_env "$ZN" run activate RUN-1 --json
  assert_exit "ZN" "ZN0_activate" 0

  # ===========================================================================
  # N1 — THE HEADLINE: a rendered packet carries the FILE'S TEXT
  # ===========================================================================

  run_env "$ZN" step render STEP-1
  assert_exit "ZN" "ZN1_render" 0

  # The body, not the path. This is the assertion the section exists for.
  assert_stdout_contains "ZN" "ZN1_body_inlined" \
    "Check spelling, grammar, and the caption on every plate."

  # Item 3: the file its frontmatter declared, inlined too.
  assert_stdout_contains "ZN" "ZN1_include_inlined" \
    "House style: serial commas"

  # Provenance survives into the packet: the path and hash still ride along.
  assert_stdout_contains "ZN" "ZN1_provenance" "checklists/proofreader-copy.md"

  # The frontmatter itself is STRIPPED — it is engine-directed metadata, not
  # content, and an executor should not have to read around it.
  if echo "$CMD_STDOUT" | grep -q "packet_includes"; then
    check "ZN" "ZN1_frontmatter_stripped" "FAIL" \
      "the frontmatter block leaked into the rendered packet"
  else
    check "ZN" "ZN1_frontmatter_stripped" "PASS"
  fi

  # An ignored key stays ignored — it is neither read nor surfaced.
  if echo "$CMD_STDOUT" | grep -q "Copy proofing"; then
    check "ZN" "ZN1_other_keys_ignored" "FAIL" \
      "a frontmatter key the engine does not read reached the packet"
  else
    check "ZN" "ZN1_other_keys_ignored" "PASS"
  fi

  # ===========================================================================
  # N2 — F1, ARM A: divergent siblings resolve to DIFFERENT files
  # ===========================================================================
  #
  # One step, two siblings, two hints, two files. A literal entry could not do
  # this, which is the whole reason the substitution token exists.

  run_env "$ZN" step render STEP-2
  assert_exit "ZN" "ZN2_render" 0
  assert_stdout_contains "ZN" "ZN2_own_file" \
    "Check ink density against the reference swatch."

  # And it must NOT carry the other sibling's checklist.
  if echo "$CMD_STDOUT" | grep -q "Check spelling"; then
    check "ZN" "ZN2_no_cross_contamination" "FAIL" \
      "a sibling received another sibling's file"
  else
    check "ZN" "ZN2_no_cross_contamination" "PASS"
  fi

  # The colour checklist declares no includes, so the shared fragment must not
  # appear — includes are per-file, not per-run.
  if echo "$CMD_STDOUT" | grep -q "House style"; then
    check "ZN" "ZN2_includes_are_per_file" "FAIL" \
      "a fragment appeared in a packet whose file never declared it"
  else
    check "ZN" "ZN2_includes_are_per_file" "PASS"
  fi

  # ===========================================================================
  # N3 — F1, ARM B: a hint family SHARES one literal entry
  # ===========================================================================
  #
  # The `spec-author-<axis>` shape, genericized. Both siblings of `sign-off`
  # read the SAME file — no per-hint file, and no prefix-stripping rule
  # anywhere in core. Identity still reaches each packet through `target:`.
  #
  # `sign-off` is gated behind `proof`, so its steps are rendered by id rather
  # than driven to ready: rendering is a read verb and needs no claim.

  run_env "$ZN" step render STEP-3
  assert_exit "ZN" "ZN3_render_a" 0
  assert_stdout_contains "ZN" "ZN3_shared_a" \
    "Every proofreader initials the sheet"
  assert_stdout_contains "ZN" "ZN3_identity_a" "proofreader-copy"

  run_env "$ZN" step render STEP-4
  assert_exit "ZN" "ZN3_render_b" 0
  assert_stdout_contains "ZN" "ZN3_shared_b" \
    "Every proofreader initials the sheet"
  assert_stdout_contains "ZN" "ZN3_identity_b" "proofreader-colour"

  # ===========================================================================
  # N4 — THE LADDER: an edit is a CONFLICT, a deletion is NOT_FOUND
  # ===========================================================================
  #
  # Snapshot-pinning, enforced. A post-activation edit must never produce a
  # silently different packet: same step => same packet, or an explicit refusal
  # naming both hashes.

  printf 'Check spelling, grammar, and EVERYTHING ELSE TOO.\n' \
    >"$ZN_CFG/checklists/proofreader-copy.md"

  run_env "$ZN" step render STEP-1
  assert_exit "ZN" "ZN4_edited_conflicts" 4

  # BOTH hashes, named. An operator needs to know whether to restore the file
  # or start a new run, and neither is decidable from "it changed".
  assert_stderr_contains "ZN" "ZN4_conflict_explains" "has changed since this run pinned it"
  assert_stderr_contains "ZN" "ZN4_conflict_pinned_hash" "pinned "
  assert_stderr_contains "ZN" "ZN4_conflict_disk_hash" "on disk "

  rm "$ZN_CFG/checklists/proofreader-copy.md"
  run_env "$ZN" step render STEP-1
  assert_exit "ZN" "ZN4_deleted_not_found" 2

  # The OTHER sibling is unaffected: one broken file does not poison the run.
  run_env "$ZN" step render STEP-2
  assert_exit "ZN" "ZN4_sibling_unaffected" 0

  # ===========================================================================
  # N5 — A WORKFLOW DECLARING NO `packet` RENDERS AS IT ALWAYS DID
  # ===========================================================================
  #
  # The no-regression assertion, at the wire. Every workflow that existed before
  # `packet` was added declares none, and none may change.

  local ZN2 ZN2_CFG
  ZN2=$(qa_mktemp_d)
  ZN2_CFG="$ZN2/config"
  mkdir -p "$ZN2_CFG/workflows"
  cat >"$ZN2_CFG/workflows/plain.toml" <<'TOML'
[pipeline]
name = "print-shop-plain"
version = 1

[match]
kind = ["task"]

[[step]]
name = "proof"
executor = "proofreader"
emits = "notes"
after = []
TOML

  run_env "$ZN2" init
  assert_exit "ZN" "ZN5_init" 0
  run_env "$ZN2" issue create -t "Proof the summer catalogue" -d "200 copies" --json
  assert_exit "ZN" "ZN5_issue" 0
  run_env "$ZN2" run start --issue DKT-1 --json
  assert_exit "ZN" "ZN5_start" 0
  run_env "$ZN2" run activate RUN-1 --json
  assert_exit "ZN" "ZN5_activate" 0

  run_env "$ZN2" step render STEP-1
  assert_exit "ZN" "ZN5_renders" 0

  # No FILE section at all — the packet is exactly what it was before.
  if echo "$CMD_STDOUT" | grep -q "== FILE"; then
    check "ZN" "ZN5_no_file_section" "FAIL" \
      "a workflow declaring no packet grew a FILE section"
  else
    check "ZN" "ZN5_no_file_section" "PASS"
  fi

  # ===========================================================================
  # N6 — INPUT PAYLOADS REACH THE PACKET (docs/tdd/input-payloads.md)
  # ===========================================================================
  #
  # An artifact has two halves: `body` is prose, `payload` is the structured
  # half. Both were recorded and resolved; only the prose reached the packet.
  # A step whose contract requires its inputs' structure had to recover it by
  # opening a copy of the database directly — which voids the pinning, the
  # reproducibility, and §6.6's no-live-state rule the packet exists to give.
  #
  # THE STRANGER TEST: the proofreader records notes AND a count of defects per
  # page. The binder downstream needs the counts, not the prose about them.

  local ZN3 ZN3_WF ZN3_TOKEN
  ZN3=$(qa_mktemp_d)
  ZN3_WF="$ZN3/printshop.toml"
  cat >"$ZN3_WF" <<'TOML'
[pipeline]
name = "print-shop-payload"
version = 1

[match]
kind = ["task"]

[[step]]
name = "proof"
executor = "proofreader"
emits = "notes"
after = []

[[step]]
name = "bind"
executor = "binder"
emits = "volumes"
after = ["proof"]
inputs = ["proof.notes"]
TOML

  run_env "$ZN3" init
  assert_exit "ZN" "ZN6_init" 0
  run_env "$ZN3" workflow register "$ZN3_WF" --json
  assert_exit "ZN" "ZN6_register" 0
  run_env "$ZN3" issue create -t "Proof the winter catalogue" -d "400 copies" --json
  assert_exit "ZN" "ZN6_issue" 0
  run_env "$ZN3" run start --issue DKT-1 --json
  assert_exit "ZN" "ZN6_start" 0
  run_env "$ZN3" run activate RUN-1 --json
  assert_exit "ZN" "ZN6_activate" 0

  printf 'three smudged pages near the gutter\n' >"$ZN3/notes.txt"
  printf '[{"page":4,"defects":3}]' >"$ZN3/counts.json"

  run_env "$ZN3" step claim STEP-1 --owner proof-a --json
  assert_exit "ZN" "ZN6_claim" 0
  ZN3_TOKEN=$(echo "$CMD_STDOUT" | jq -r '.data.token')

  DOCKET_TOKEN="$ZN3_TOKEN" run_env "$ZN3" step complete STEP-1 \
    --artifact-file "$ZN3/notes.txt" --payload-file "$ZN3/counts.json" --json
  assert_exit "ZN" "ZN6_complete_with_payload" 0

  # THE ASSERTION THIS BLOCK EXISTS FOR: the binder's packet carries the
  # STRUCTURE, not only the prose about it.
  run_env "$ZN3" step render STEP-2
  assert_exit "ZN" "ZN6_renders" 0
  assert_stdout_contains "ZN" "ZN6_packet_has_prose" "smudged"
  assert_stdout_contains "ZN" "ZN6_packet_has_payload" "defects"

  # And the bundle carries it as its own field — the two halves arrive
  # SEPARATELY, never concatenated.
  run_env "$ZN3" step context STEP-2 --json
  assert_exit "ZN" "ZN6_context" 0
  assert_json "ZN" "ZN6_payload_is_its_own_field" \
    '.data.context.inputs[0].payload' '[{"page":4,"defects":3}]'
  # The prose half arrives SEPARATELY — never concatenated into the payload.
  assert_json "ZN" "ZN6_body_is_still_its_own_field" \
    '.data.context.inputs[0].body' 'three smudged pages near the gutter'

  # A payload-less input omits the key entirely, so every workflow that does
  # not use payloads sees a byte-identical bundle. STEP-1 has no inputs, so
  # ZN5's plain packet above is the packet-level half of this proof.
  rm -rf "$ZN" "$ZN2" "$ZN3"
}
