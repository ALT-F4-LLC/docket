#!/usr/bin/env bash
#
# reserved-name-check — the docs/spec/ namespace gate.
#
# Wired as a gate on the doc-authoring steps of spec-doc.toml and
# spec-project.toml. It enforces the split that contracts/spec-author.md calls
# "the authority for the reserved names": seven file names under docs/spec/
# belong to `spec-author`, and every OTHER docs/spec/{slug}.md is a PRD
# belonging to `prd-author`.
#
# The failure it exists to catch is a PRD author claiming a reserved name — an
# `architecture.md` written as a product requirements document silently
# replaces the standing description of what this project IS, and every
# downstream reader treats it as settled fact.
#
# It checks NAMES ONLY. Whether a spec's contents are well-formed is
# `doc-validate`'s question, and citation grounding is `citation-check`'s.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

# The reserved seven, verbatim from contracts/spec-author.md's table.
readonly RESERVED=(
  architecture
  security
  operations
  performance
  code-quality
  review-strategy
  testing
)

# Staged + unstaged + untracked, for the same reason self-hygiene.sh does it:
# this gate runs on a write step before any commit, so a staged-only file
# appears in neither a bare working-tree diff nor the untracked list.
changed=$( { git diff --cached --name-only -- docs/spec
             git diff --name-only -- docs/spec
             git ls-files --others --exclude-standard -- docs/spec
           } | sort -u )

if [ -z "$changed" ]; then
  echo "reserved-name-check: no docs/spec/ changes"
  exit 0
fi

is_reserved() {
  local slug="$1"
  local r
  for r in "${RESERVED[@]}"; do
    [ "$slug" = "$r" ] && return 0
  done
  return 1
}

status=0

while IFS= read -r path; do
  [ -n "$path" ] || continue

  base="${path##*/}"

  # Only .md files under docs/spec/ are in this namespace.
  case "$path" in
    docs/spec/*.md) ;;
    docs/spec/*)
      echo "reserved-name-check: non-markdown file in docs/spec/: $path" >&2
      status=1
      continue
      ;;
    *) continue ;;
  esac

  slug="${base%.md}"

  # A nested path under docs/spec/ is outside the flat namespace the contract
  # describes; both authors write docs/spec/<name>.md directly.
  if [ "$path" != "docs/spec/$base" ]; then
    echo "reserved-name-check: nested path under docs/spec/: $path" >&2
    echo "  the namespace is flat: docs/spec/<slug>.md" >&2
    status=1
    continue
  fi

  if is_reserved "$slug"; then
    echo "reserved-name-check: $path -> reserved (spec-author)"
  else
    echo "reserved-name-check: $path -> PRD namespace (prd-author)"
  fi
done <<< "$changed"

if [ "$status" -ne 0 ]; then
  cat >&2 <<'EOF'

reserved-name-check FAILED.

docs/spec/ holds exactly two kinds of file:
  - the reserved seven (architecture, security, operations, performance,
    code-quality, review-strategy, testing), written by spec-author; and
  - any other docs/spec/<slug>.md, which is a PRD written by prd-author.

The namespace is flat and markdown-only.
EOF
  exit 1
fi

echo "reserved-name-check: ok"
exit 0
