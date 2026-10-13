#!/usr/bin/env bash
#
# The genericity rule, as a scripted gate.
#
# Usage:
#   ./scripts/qa/genericity.sh [--verbose]
#
# The rule, binding on every workflow-engine feature:
#
#   Core Docket contains zero agent/LLM vocabulary — no node, model, prompt,
#   brief, severity, or review concepts. Executors are opaque string hints;
#   execution metadata is an opaque KV bag; ordered meaning comes from
#   user-registered schemas.
#
# CLAUDE.md states the consequence as a PR bar: a PR introducing such
# vocabulary into core surface fails review by definition. This script is that
# bar mechanized — the same discipline as the --token-flag guard test stage 2
# established. A rule that depends on a reviewer's memory is a rule that
# erodes; a rule that runs on every push does not. The part of the rule no
# script can check stays with the reviewer.

set -euo pipefail

VERBOSE=false
if [ "${1:-}" = "--verbose" ]; then
  VERBOSE=true
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

# --- What this checks, and what it deliberately does not ---------------------
#
# CORE SURFACE: flag names, JSON keys, column names, embedded templates, help
# text, and error strings. Those are what a user reads or a machine parses, and
# they are what the rule is about. So the check reads STRING LITERALS, STRUCT
# TAGS, AND TEMPLATES — the surface itself.
#
# Go INTERNALS are not surface, and scanning them yields only false positives
# with a strong training effect toward ignoring the check. The three a naive
# grep hits in this repo, each innocent:
#
#   - `internal/model` is the data model in the MVC sense. It predates the
#     workflow engine and names no LLM concept.
#   - `prompt` appears as an interactive CONFIRMATION prompt ("Delete label
#     X?") — a terminal UI affordance, not a model input.
#   - `node` appears as the DAG vertex variable in internal/planner, which is
#     graph vocabulary, one of the scheduling nouns the rule permits.
#
# Renaming any of those would remove no agent vocabulary from anything a user
# can see. A reviewer reads the rest.

BANNED=(
  model
  prompt
  llm
  agent
  brief
)

# `node` is NOT banned: it is admitted scheduling vocabulary, and internal/planner's DAG has used it since before the engine
# existed. `severity` and `review` are likewise absent — `review` is a
# pre-existing ISSUE STATUS in core surface (`docket issue move DKT-1 review`),
# and `severity` is legitimate as an opaque threshold field name (see the
# fixture exclusion below). What the rule bans is those words as ENGINE
# CONCEPTS, which is a judgment a reviewer makes. The words above are the ones
# with no innocent reading in a flag, key, or template.

# --- Admitted forms ----------------------------------------------------------
#
# Each entry is "<banned word> <ERE>": a form of that word core surface ships
# on purpose. Matching is case-sensitive on the surface text. The scan deletes every
# admitted match from a line before testing the banned word, so an admitted
# form never shields another use of the word on the same line. Anything not
# listed, including new prose such as "model routing" in an error string,
# still fails.
ADMITTED=(
  # The run policy's routing key: `model = "..."` in a variant spec, and its
  # key=value renderings in test and diagnostic text.
  'model model *= *'
  # The same key as a persisted JSON field and a TOML field.
  'model (json|toml):"model[",]'
  # The routing triple a seat resolves to.
  'model model(/effort/variant|, effort)'
  # The MODEL column header of the policy seat table.
  'model ^"MODEL"$'
  # Shipped policy help and error text naming the routing key.
  'model still-forbidden model redirected through'
  'model no permitted model reachable from'
  # Exported identifiers of the internal/model package (MVC data model).
  'model model\.[A-Z]'
  # The retired config link-farm layout, a design sense of the word.
  'model link-farm model'
  # A seat name: the security threat-model step.
  'model threat-model'
)

# --- Where core surface lives ------------------------------------------------
SEARCH_PATHS=(
  internal
  cmd
)

# --- Exclusions --------------------------------------------------------------
#
# internal/workflow/testdata/example-workflow.toml is EXCLUDED, and the
# exclusion is not an oversight to be tidied away later.
#
# The fixture legitimately contains `severity` and `status` as FIELD NAMES
# INSIDE AN INSTANCE'S THRESHOLD PREDICATE STRINGS — `any(severity >= high)`.
# The threshold grammar is `agg(field op literal)`, and ordered comparisons
# apply "only for fields whose registered schema declares ordered_enum". The
# parser therefore knows the SHAPE `agg(field op literal)` and never what a
# field MEANS: `severity` is an opaque token until a user registers a schema
# for it.
#
# That is the genericity line holding exactly where the threshold grammar
# draws it, not a violation of it. Removing this exclusion would "fix" a file
# that is correct and break the canonical register-test fixture — so it is
# written here with its reason.
EXCLUDE_PATHS=(
  internal/workflow/testdata/example-workflow.toml
)

is_excluded() {
  local file="$1" excluded
  for excluded in "${EXCLUDE_PATHS[@]}"; do
    [ "$file" = "$excluded" ] && return 0
  done
  return 1
}

# surface_lines extracts the surface out of a Go file: string literals — which
# carry flag names, help text, error strings, and SQL — and struct tags, which
# carry JSON keys and column names.
#
# Comments are excluded. A comment stating the rule is the rule being STATED,
# not broken, and a check that failed on its own rationale would make the
# rationale unwritable.
#
# The extraction tracks string state character by character, because the one
# shortcut that looks safe here is not: a line-oriented match for `...` spans
# never extracts the BODY of a multi-line raw string, and multi-line raw
# strings are where DDL lives — every CREATE TABLE column name in the schema
# ladder went unscanned that way, and moving a banned term from a single-line
# ALTER literal into a multi-line block turned the gate green (DKT-83). Block
# comments (/* */) are still not tracked; that crudeness has no known evasion
# shaped like the raw-string one.
#
# IMPORT PATHS are dropped. They are string literals syntactically, but a Go
# import path is a compile-time reference to a package on disk — no user reads
# one and no machine parses one out of Docket's output. Keeping them would
# report every one of the ~1000 files that import `internal/model` (the data
# model in the MVC sense) and drown the signal completely.
#
# REPOSITORY PATHS are dropped for the same reason — `"internal/model/…"` in a
# test's failure message names a file on disk, not a concept in the product.
surface_lines() {
  awk '
    # Three states: code, interpreted string ("...", one line, \-escapes),
    # raw string (`...`, may span lines). Only string contents are emitted,
    # one span per line, delimiters kept so the path filters below see the
    # same shapes the old extractor produced. A multi-line raw string emits
    # one backtick-wrapped fragment per source line, so every body line of a
    # DDL block is surface. Line comments are dropped only in code state: a
    # "//" inside a string is content, and a string inside a comment is
    # commentary.
    {
      line = $0
      i = 1
      n = length(line)
      if (inraw) buf = "`"
      while (i <= n) {
        c = substr(line, i, 1)
        if (inraw) {
          buf = buf c
          if (c == "`") { print buf; buf = ""; inraw = 0 }
        } else if (instr) {
          buf = buf c
          if (c == "\\") { i++; buf = buf substr(line, i, 1) }
          else if (c == "\"") { print buf; buf = ""; instr = 0 }
        } else {
          if (c == "/" && substr(line, i + 1, 1) == "/") break
          if (c == "\"") { instr = 1; buf = "\"" }
          else if (c == "`") { inraw = 1; buf = "`" }
        }
        i++
      }
      if (inraw) { print buf "`" }
      instr = 0 # an interpreted string cannot span lines; an unclosed one is not surface
      buf = ""
    }
  ' "$1" |
    grep -v 'github\.com/ALT-F4-LLC/docket/' |
    grep -vE '"[^"]*(internal|cmd|docs|scripts)/[A-Za-z0-9_/.-]+"' || true
}

# --- Extractor self-test -----------------------------------------------------
#
# A negative control run before every scan: a banned-shaped word in the body
# of a MULTI-LINE raw string — the DDL shape the extractor was blind to
# (DKT-83) — must come out as surface, or the gate would pass while unable to
# see the very lines schema DDL lives on. FATAL rather than a silent pass,
# per the same vacuity rule as the empty-surface guard below.
extractor_selftest() {
  local fixture extracted
  fixture=$(mktemp "${TMPDIR:-/tmp}/genericity-selftest.XXXXXX")
  cat > "$fixture" <<'FIXTURE'
package fixture

const ddl = `CREATE TABLE IF NOT EXISTS widgets (
    model TEXT NOT NULL,
    name  TEXT
)`

const quoted = "an interpreted \"string\"" // and a comment "not surface"
FIXTURE
  extracted=$(surface_lines "$fixture")
  rm -f "$fixture"
  if ! printf '%s\n' "$extracted" | grep -q 'model TEXT'; then
    printf 'FATAL: extractor self-test — the body of a multi-line raw string\n'
    printf 'was not extracted; the scan would be blind to DDL (DKT-83)\n'
    exit 1
  fi
  if ! printf '%s\n' "$extracted" | grep -q 'an interpreted'; then
    printf 'FATAL: extractor self-test — a plain interpreted string was not\n'
    printf 'extracted; the extractor is broken, not merely crude\n'
    exit 1
  fi
  if printf '%s\n' "$extracted" | grep -q 'not surface'; then
    printf 'FATAL: extractor self-test — comment text was extracted as\n'
    printf 'surface; the rationale-stays-writable rule no longer holds\n'
    exit 1
  fi
}
extractor_selftest

# banned_hits prints "<file>: <surface text>" for each line of a surface file
# ("<file>\t<surface text>" per line) that still contains WORD once every
# ADMITTED form of WORD is deleted from it. Only the text column is tested:
# every file under internal/model/ would otherwise match `model` on its own
# path and the check would report itself.
banned_hits() {
  local word="$1" surface="$2"
  awk -F'\t' -v word="$word" '
    BEGIN { pattern = "(^|[^A-Za-z0-9_])" tolower(word) "([^A-Za-z0-9_]|$)" }
    FNR == NR {
      sep = index($0, " ")
      if (substr($0, 1, sep - 1) == word) admitted[++n] = substr($0, sep + 1)
      next
    }
    {
      text = $2
      for (i = 1; i <= n; i++) gsub(admitted[i], " ", text)
      if (tolower(text) ~ pattern) printf "%s: %s\n", $1, $2
    }
  ' <(printf '%s\n' "${ADMITTED[@]}") "$surface" || true
}

# --- Admission self-test -----------------------------------------------------
#
# A shipped form must be admitted, and a prose use of the word must stay a hit
# even on a line that also carries an admitted form. FATAL otherwise: an
# admission list that swallows prose turns the gate green while blind.
admission_selftest() {
  local fixture hits
  fixture=$(mktemp "${TMPDIR:-/tmp}/genericity-admit.XXXXXX")
  printf '%s\t%s\n' \
    fixture.go '`json:"model,omitempty"`' \
    fixture.go '"model=%q has no model routing"' > "$fixture"
  hits=$(banned_hits model "$fixture")
  rm -f "$fixture"
  if printf '%s\n' "$hits" | grep -q 'omitempty'; then
    printf 'FATAL: admission self-test — the shipped json:"model" tag was\n'
    printf 'reported; the admitted forms are not being applied\n'
    exit 1
  fi
  if ! printf '%s\n' "$hits" | grep -q 'model routing'; then
    printf 'FATAL: admission self-test — prose "model routing" was admitted;\n'
    printf 'an admitted form is shielding the rest of its line\n'
    exit 1
  fi
}
admission_selftest

# template_lines treats an embedded template as surface in its entirety: every
# byte is read by the first user to run `workflow init`, comments included.
#
# `*.json` gets the SAME treatment as of stage 5, and it is not optional. That
# stage ships the first embedded JSON
# core surface — the `aggregate@1` schema document — and every byte of a shipped
# schema is read by the first user who runs `docket schema show aggregate@1`. A
# schema document is surface in exactly the sense this comment already gives for
# templates.
#
# `scripts/qa/fixtures/**` is untouched by that extension, because this script
# has never scanned outside internal/ and cmd/: QA fixture schemas are instance
# data BY LOCATION, which is the cheapest possible enforcement. It is written
# here so nobody later "tidies" those fixtures into internal/.
template_lines() {
  cat "$1"
}

# SURFACE_EXTENSIONS are the whole-file surfaces: embedded templates and
# embedded schema documents. Go sources are handled separately, by
# surface_lines, because only their literals and tags are surface.
SURFACE_EXTENSIONS=(
  '*.toml'
  '*.json'
)

fail=0
pass=0
found_any=false

printf "=== Genericity check ===\n"
printf "The genericity rule, over core surface (strings, tags, templates)\n\n"

# Collect the surface once, so each banned term is a cheap scan over it.
#
# Each entry is "<file>\t<surface text>": the path is a SEPARATE COLUMN, not a
# prefix on the text, so a scan can test the text alone. A prefixed path would
# make every file under internal/model/ match `model` on its own name.
# Template form, deliberately: bare mktemp on macOS ignores TMPDIR and uses the
# confstr per-user temp dir, which sandboxed environments may deny. TMPDIR is on
# the gate env allowlist (internal/exec/env.go), so honor it when set.
SURFACE_FILE=$(mktemp "${TMPDIR:-/tmp}/genericity-surface.XXXXXX")
trap 'rm -f "$SURFACE_FILE"' EXIT

for path in "${SEARCH_PATHS[@]}"; do
  [ -d "$path" ] || continue

  while IFS= read -r file; do
    is_excluded "$file" && continue
    surface_lines "$file" | awk -v f="$file" '{ printf "%s\t%s\n", f, $0 }' >> "$SURFACE_FILE"
  done < <(find "$path" -name '*.go' -type f)

  for ext in "${SURFACE_EXTENSIONS[@]}"; do
    while IFS= read -r file; do
      is_excluded "$file" && continue
      template_lines "$file" | awk -v f="$file" '{ printf "%s\t%s\n", f, $0 }' >> "$SURFACE_FILE"
    done < <(find "$path" -name "$ext" -type f)
  done
done

# A check that extracted nothing passes vacuously and proves nothing — the
# same non-empty guard the byte-compat sweep applies to its diffs.
if [ ! -s "$SURFACE_FILE" ]; then
  printf "FATAL: no core surface was extracted; the check would pass vacuously\n"
  exit 1
fi

SURFACE_COUNT=$(grep -c '' "$SURFACE_FILE")
printf "scanning %d lines of core surface\n\n" "$SURFACE_COUNT"

for word in "${BANNED[@]}"; do
  hits=$(banned_hits "$word" "$SURFACE_FILE")

  if [ -n "$hits" ]; then
    found_any=true
    fail=$((fail + 1))
    printf "FAIL  %-8s appears in core surface:\n" "$word"
    printf '%s\n' "$hits" | head -20 | sed 's/^/        /'
    hit_count=$(printf '%s\n' "$hits" | grep -c '' || true)
    if [ "$hit_count" -gt 20 ]; then
      printf "        ... and %d more\n" "$((hit_count - 20))"
    fi
  else
    pass=$((pass + 1))
    if [ "$VERBOSE" = true ]; then
      printf "PASS  %-8s absent from core surface\n" "$word"
    fi
  fi
done

printf "\n%d/%d banned terms absent from core surface\n" "$pass" "$((pass + fail))"

if [ "$found_any" = true ]; then
  cat <<'EOF'

The genericity rule (CLAUDE.md PR bar):

  Core Docket contains zero agent/LLM vocabulary. A PR introducing `model`,
  `prompt`, or `llm` into core surface fails review by definition; metadata KV
  exists precisely so such things ride through opaquely.

If a term above is genuinely necessary, it belongs in an opaque `metadata` or
`params` bag — which the engine stores and returns verbatim and never reads a
key inside — not in a flag name, JSON key, column name, error string, embedded
template, or help text.
EOF
  exit 1
fi

exit 0
