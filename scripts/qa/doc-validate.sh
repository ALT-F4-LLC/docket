#!/usr/bin/env bash
#
# doc-validate — a changed doc carries the header this repo's docs carry.
#
# Wired as the `doc-validate` gate on docs-only's author step.
# CLAUDE.md: "design first as docs/tdd/<feature>.md (frontmatter per existing
# files)". Every doc under docs/ opens with a title line and a `Status:` line
# naming a state and a date — that is the frontmatter the convention means, and
# a doc without it is the drift this gate catches.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

changed=$( { git diff --cached --name-only -- 'docs/*.md'
             git diff --name-only -- 'docs/*.md'
             git ls-files --others --exclude-standard -- 'docs/*.md'
           } | sort -u )

if [ -z "$changed" ]; then
  echo "doc-validate: no docs changed"
  exit 0
fi

# `while IFS= read -r`, never `for f in $changed`: word splitting turns
# `docs/my design.md` into two names that are not files, and the `[ -f ]` guard
# below — written to be defensive — then `continue`s past both. The result is a
# malformed doc reported as OK. A guard that converts a bug into a silent pass
# is worse than no guard.
failed=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  [ -f "$f" ] || continue

  if ! head -1 "$f" | grep -q '^# '; then
    echo "doc-validate: $f does not open with a '# ' title" >&2
    failed=1
  fi

  # `Status: <state> — <date>` in the opening block. The date is what makes a
  # stale doc visible; a state with no date ages invisibly.
  if ! head -8 "$f" | grep -qE '^Status: .+[0-9]{4}-[0-9]{2}-[0-9]{2}'; then
    echo "doc-validate: $f has no 'Status: ... YYYY-MM-DD' line in its first 8 lines" >&2
    failed=1
  fi
done <<< "$changed"

if [ "$failed" -ne 0 ]; then
  echo "" >&2
  echo "doc-validate FAILED. Existing docs under docs/ show the shape." >&2
  exit 1
fi

echo "doc-validate: ok"
