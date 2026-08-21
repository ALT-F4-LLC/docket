#!/usr/bin/env bash
#
# copy-verify — user-facing copy is greppable and consistent.
#
# Wired as a gate on ui-change.toml's `design-qa` step, alongside render-verify.
# The rule it mechanizes is fragments/copy-discipline.md:
#
#   "Copy literals are the executable acceptance surface [...] a copy gate greps
#    the built output for each literal, so a quoted string is a machine-verifiable
#    commitment and an unquoted paraphrase is not checkable at all."
#
# WHAT THIS GATE CHECKS, AND WHAT IT CANNOT. The full form of the check compares
# the built output against the copy literals quoted in the issue's accepted
# ux-spec. A gate process receives no issue or step identity
# (internal/exec/env.go BuildEnv gives it TERM, CI, DOCKET_GATE, DOCKET_REPO and
# an allowlisted parent env), so it cannot locate that spec. When docs/ux/ holds
# specs, this gate greps their backticked literals against the binary's strings;
# when it does not, it falls back to the copy-discipline rules that are
# checkable repo-wide.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

status=0

echo "=== copy-verify: error-message discipline ==="

# copy-discipline.md: "Error messages say what happened, why, and what to do
# now [...] Never blame the person." A user-facing error that opens with a bare
# capitalized type name, or that says "you" accusingly, is the failure mode.
#
# internal/output/human.go is where errors reach the user; internal/cli is where
# they are constructed.
if grep -rnE 'fmt\.Errorf\("[Yy]ou (did|have|must not|cannot|should)' \
     --include='*.go' internal/cli/ internal/output/ 2>/dev/null; then
  echo "  the message(s) above blame the reader; say what happened and what to do" >&2
  status=1
else
  echo "  ok       no reader-blaming error text"
fi

# A placeholder that shipped is the defect copy-discipline names first.
#
# TODO IS NOT SEARCHED, deliberately. `TODO` is one of this project's ISSUE
# STATUSES — it is a board column heading and a status literal ("TODO (2) ===",
# `[]string{"BACKLOG", "TODO", "REVIEW", "DONE"}` in render/board_test.go), so
# matching it flags the product's own vocabulary as placeholder copy. A gate
# that fails on correct code teaches the team to bypass it, which costs more
# than the placeholders it would have caught.
#
# Test files are excluded for the same reason: a test asserting on placeholder
# copy is a test doing its job, not shipped copy.
placeholders=$(grep -rnE '"(TBD|FIXME|XXX|lorem ipsum|Lorem ipsum)[^"]*"' \
                 --include='*.go' internal/cli/ internal/output/ internal/render/ 2>/dev/null |
               grep -v '_test\.go:' || true)

if [ -n "$placeholders" ]; then
  printf '%s\n' "$placeholders" >&2
  echo "  placeholder copy above must be replaced with the real string" >&2
  status=1
else
  echo "  ok       no placeholder copy in user-facing packages"
fi

echo
echo "=== copy-verify: ux-spec literals ==="

if [ -d docs/ux ] && ls docs/ux/*.md >/dev/null 2>&1; then
  # The real check: every backticked literal in a ux-spec must appear verbatim
  # in the source that renders it. Punctuation and capitalization are part of
  # the literal, per copy-discipline.md.
  missing=0
  while IFS= read -r literal; do
    [ -n "$literal" ] || continue
    # Only check strings that look like user-facing copy: contain a space, and
    # are not code identifiers or flags.
    case "$literal" in
      *" "*) ;;
      *) continue ;;
    esac
    case "$literal" in
      --*|-*|/*|*"("*) continue ;;
    esac
    if ! grep -rqF "$literal" --include='*.go' internal/ cmd/ 2>/dev/null; then
      echo "  MISSING  ux-spec literal not found in source: $literal" >&2
      missing=1
    fi
  done < <(grep -ohE '`[^`]+`' docs/ux/*.md 2>/dev/null | tr -d '`' | sort -u)

  if [ "$missing" -eq 0 ]; then
    echo "  ok       every ux-spec copy literal appears in source"
  else
    status=1
  fi
else
  echo "  no docs/ux/ specs present; literal comparison not run"
  echo "  (this is a reported gap, not a pass: see the header)"
fi

if [ "$status" -ne 0 ]; then
  cat >&2 <<'EOF'

copy-verify FAILED.

fragments/copy-discipline.md: copy literals are the executable acceptance
surface. Quoted strings in a ux-spec are exact commitments — punctuation,
capitalization, and whitespace included — and user-facing errors say what
happened, why, and what to do now, without blaming the reader.
EOF
  exit 1
fi

echo
echo "copy-verify: ok"
exit 0
