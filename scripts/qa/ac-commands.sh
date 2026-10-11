#!/usr/bin/env bash
#
# ac-commands — run the repo's checks and print their results for the verifier.
#
# Wired as a PRE-GATE on `verify`: it runs at claim, and its output
# lands in the step's context bundle. That is the whole point. `verify-ac`
# judges acceptance criteria against the diff, and an AC of the form "the tests
# pass" is one it cannot settle by reading code — without recorded command
# results it must return `unverifiable`, which parks the step on the operator.
#
# It reports AND propagates: it prints what ran and what each exited, and its
# own exit status is the WORST status any check recorded. The engine derives
# the gate verdict from the exit status (gate_exec.go), so a failing check
# records `fail` instead of a `pass` verdict sitting on top of a captured body
# full of nonzero exits. A failing pre-gate still cannot refuse the claim —
# pregate.go's PG2/PG3 carry the failure into the bundle as data — so the
# verifier sees an honest verdict and the step still runs.

set -euo pipefail

# Root resolution respects the caller's cwd: the engine spawns gates with Dir
# set to the step's worktree, and trust entries invoke this script by absolute
# path into the shared checkout — a script-relative cd would re-point the gate
# at the shared tree. Outside any git repo, fall back to the script's own root.
cd "$(git rev-parse --show-toplevel 2>/dev/null || echo "$(dirname "${BASH_SOURCE[0]}")/../..")"

# Worst status seen so far — a max, never a sum: summed exits wrap at 256
# back to 0, which would mask a failure as success.
worst=0

run() {
  local label="$1"; shift
  echo "--- $label: $* ---"
  local status=0
  # Keep the last 20 lines plus every failure-identifying line wherever it
  # occurs, in original order, each once: a package that fails early in a long
  # `go test ./...` run would otherwise lose its test names to a bare tail.
  # `--- FAIL:` may be indented because go test nests subtest failures;
  # `FAIL` and `panic:` are always printed at column 0. awk reads to EOF, so
  # it never cuts the check short with SIGPIPE.
  # Capture PIPESTATUS[0] in the `||` itself: any intervening command would
  # clobber it, and a bare $? here would be awk's status, not the check's.
  "$@" 2>&1 | awk '
    { line[NR] = $0 }
    /^[ \t]*--- FAIL:|^FAIL|^panic:/ { keep[NR] = 1 }
    END { for (i = 1; i <= NR; i++) if ((i in keep) || i > NR - 20) print line[i] }
  ' || status=${PIPESTATUS[0]}
  echo "[$label] exit $status"
  if [ "$status" -gt "$worst" ]; then
    worst=$status
  fi
  echo
}

echo "=== ac-commands: recorded results for the verifier ==="
echo "commit: $(git rev-parse --short HEAD)"
echo "tree:   $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
echo

# go1.26.9 rather than `go`: that is the binary this toolchain provides, and a
# gate that names a command the environment lacks records "unmatched" and
# teaches the verifier nothing.
run build go1.26.9 build ./...
# Acceptance criteria worded with -count=1 must be settled by a fresh run, not a cached result.
# -timeout 60m: internal/engine alone took 1380-1570s under concurrent run load
# (RUN-135), within minutes of the former 1800s cap; 60m leaves headroom over
# that range. The trust entry's own timeout for this script must exceed 60m
# plus the qa checks below, or it kills the gate before they finish: one
# docket build, then each named qa.sh section (every run re-runs sections B
# through its target, about 45s for Z and 110s for ZG), then genericity.sh.
run tests go1.26.9 test -count=1 -timeout 60m ./...

# Run the scripts/qa checks the issue's acceptance criteria name, so an AC like
# "`bash scripts/qa.sh <built docket> ZG` reports PASS" is settled by execution.
# Criterion text is untrusted: nothing from it reaches a command line except a
# qa.sh section token found in qa.sh's own SECTIONS registry, and every other
# script runs by a literal path from this allow-list, never by the text's.
# Anything off the list is printed as skipped and never executed.
qa_sections=""
qa_scripts=""
if [ -z "${DOCKET_ISSUE:-}" ]; then
  echo "qa checks: DOCKET_ISSUE unset; no criteria read, none added"
  echo
else
  issue_status=0
  description=$(docket issue show "$DOCKET_ISSUE" --json | jq -r '.data.description // empty') || issue_status=$?
  if [ "$issue_status" -ne 0 ]; then
    # Name the store this child resolved: gate children are denied
    # DOCKET_PATH, so it can differ from the engine's.
    store=$(docket config --json | jq -r '"\(.data.db_path) (\(.data.store_source))"') || store="unresolved"
    echo "qa checks: reading $DOCKET_ISSUE's criteria from store $store failed (exit $issue_status); none added"
    [ "$issue_status" -gt "$worst" ] && worst=$issue_status
    description=""
  fi
  # Scan only the criteria block when the issue has one: a qa.sh mention in
  # the narrative above it is background, not a criterion to verify.
  criteria=$(printf '%s\n' "$description" | awk '/^Acceptance criteria/ { found = 1 } found')
  [ -n "$criteria" ] || criteria=$description

  # A qa.sh invocation is the span from `scripts/qa.sh` to the closing
  # backtick or end of line; its LAST field is the section, since the binary
  # placeholder varies (`<bin>`, `<built docket>`) and may contain spaces.
  qa_spans=$(printf '%s\n' "$criteria" | grep -oE 'scripts/qa\.sh[^`]*' || true)

  # The allow-list of section names is qa.sh's SECTIONS array itself, so a
  # name qa.sh does not define is skipped here rather than reaching its
  # "unknown section" exit after a full run. It is read only when a criterion
  # names qa.sh, so an issue without one never depends on qa.sh's layout.
  qa_known_sections=""
  if [ -n "$qa_spans" ]; then
    qa_known_sections=" $(sed -nE 's/^  ([A-Z]+):test_.*/\1/p' scripts/qa.sh | tr '\n' ' ' || true)"
  fi

  while IFS= read -r span; do
    [ -n "$span" ] || continue
    # An unbackticked span ends at end of line, so it can carry the
    # sentence's closing punctuation.
    span=${span%[.,:]}
    case "$span" in
      *[!A-Za-z0-9\ \<\>._/-]*)
        echo "qa checks: skipped '$span': shell metacharacter in invocation"
        continue ;;
    esac
    set -- $span
    if [ "$#" -lt 3 ]; then
      echo "qa checks: skipped '$span': no binary and section arguments"
      continue
    fi
    section=${!#}
    # An empty read is a broken registry parse, not a criterion to skip quietly.
    if [ -z "${qa_known_sections// /}" ]; then
      echo "qa checks: skipped '$span': no section names read from scripts/qa.sh SECTIONS"
      [ "$worst" -gt 1 ] || worst=1
      continue
    fi
    case "$qa_known_sections" in
      *" $section "*) ;;
      *)
        echo "qa checks: skipped '$span': section '$section' is not a qa.sh section name"
        continue ;;
    esac
    case " $qa_sections " in *" $section "*) ;; *) qa_sections="$qa_sections $section" ;; esac
  done <<< "$qa_spans"

  while IFS= read -r path; do
    path=${path%.}
    case "$path" in
      scripts/qa/genericity.sh)
        case " $qa_scripts " in *" genericity "*) ;; *) qa_scripts="$qa_scripts genericity" ;; esac ;;
      *)
        echo "qa checks: skipped '$path': not on the qa script allow-list" ;;
    esac
  done < <(printf '%s\n' "$criteria" | grep -oE 'scripts/qa/[A-Za-z0-9_./-]+' | sort -u || true)
  echo
fi

if [ -n "$qa_sections" ]; then
  qa_bin_dir=$(mktemp -d "${TMPDIR:-/tmp}/ac-commands-qa.XXXXXX")
  trap 'rm -rf "$qa_bin_dir"' EXIT
  run qa-binary go1.26.9 build -o "$qa_bin_dir/docket" ./cmd/docket
  for section in $qa_sections; do
    if [ -x "$qa_bin_dir/docket" ]; then
      run "qa.sh-$section" bash scripts/qa.sh "$qa_bin_dir/docket" "$section"
    else
      echo "qa checks: skipped qa.sh section $section: docket binary build failed"
    fi
  done
fi
for script in $qa_scripts; do
  run "$script" bash "scripts/qa/$script.sh"
done

echo "=== end ac-commands ==="
exit "$worst"
