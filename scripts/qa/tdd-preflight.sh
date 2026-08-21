#!/usr/bin/env bash
#
# tdd-preflight — a TDD exists before the design it describes is implemented.
#
# Wired as the `tdd-preflight` gate on spec-doc.toml's TDD-authoring step,
# alongside `doc-validate` and `reserved-name-check`.
#
# CLAUDE.md makes this the repo's stated convention:
#   "Conventions: design first as docs/tdd/<feature>.md (frontmatter per
#    existing files)"
#
# So this gate checks the ORDER the convention asserts: a changed TDD is in
# docs/tdd/, is named for a feature, and carries the frontmatter its siblings
# carry. Whether the design is GOOD is a reviewer's judgment, not a gate's.
#
# `doc-validate` checks the generic doc header (title + Status line). This
# checks the TDD-specific placement and naming that header cannot express.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

changed=$( { git diff --cached --name-only -- 'docs/tdd/*.md'
             git diff --name-only -- 'docs/tdd/*.md'
             git ls-files --others --exclude-standard -- 'docs/tdd/*.md'
           } | sort -u )

if [ -z "$changed" ]; then
  echo "tdd-preflight: no docs/tdd/ changes"
  exit 0
fi

status=0

# `while IFS= read -r`, not `for f in $changed` — a path with a space would
# otherwise split into names that are not files and skip past the checks
# silently (the same trap doc-validate.sh documents).
while IFS= read -r f; do
  [ -n "$f" ] || continue
  [ -f "$f" ] || continue

  base="${f##*/}"

  # Flat namespace, kebab-case feature name: docs/tdd/<feature>.md.
  if [ "$f" != "docs/tdd/$base" ]; then
    echo "tdd-preflight: nested path under docs/tdd/: $f" >&2
    echo "  the convention is flat: docs/tdd/<feature>.md" >&2
    status=1
    continue
  fi

  slug="${base%.md}"
  if ! printf '%s' "$slug" | grep -qE '^[a-z0-9]+(-[a-z0-9]+)*$'; then
    echo "tdd-preflight: $f is not named for a feature in kebab-case" >&2
    echo "  expected docs/tdd/<feature>.md, e.g. docs/tdd/graph-engine.md" >&2
    status=1
  fi

  # The frontmatter CLAUDE.md points at ("per existing files"). Existing TDDs
  # open with a '# ' title and a 'Status:' line; doc-validate enforces that
  # shape repo-wide, so here we check only that the file is not empty of it —
  # a TDD that is a stub has not preceded the design it claims to record.
  if ! head -20 "$f" | grep -qE '^[Ss]tatus:'; then
    echo "tdd-preflight: $f carries no Status: line in its opening block" >&2
    status=1
  fi
done <<< "$changed"

if [ "$status" -ne 0 ]; then
  cat >&2 <<'EOF'

tdd-preflight FAILED.

CLAUDE.md: "design first as docs/tdd/<feature>.md (frontmatter per existing
files)". A TDD is a flat, kebab-case markdown file under docs/tdd/ that opens
with a title and a Status line.
EOF
  exit 1
fi

echo "tdd-preflight: ok"
exit 0
