#!/usr/bin/env bash
#
# citation-check — an amendment cites its evidence.
#
# Wired as the `citation-check` gate on docs-only.
# docs/design/amendments.md, the rule this mechanizes:
#
#   "an amendment that moves work across the AC boundary (model <-> engine)
#    must cite evidence per the upstream evidence standard (01 §3)"
#   "Decisions D1-D16 live upstream; cite by number."
#
# So a changed design doc must point at something outside itself: a spec
# section, a decision, a tracker issue, or a source location. A design claim
# with no citation is the assertion the standard exists to prevent.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

changed=$( { git diff --cached --name-only -- 'docs/design/*.md'
             git diff --name-only -- 'docs/design/*.md'
             git ls-files --others --exclude-standard -- 'docs/design/*.md'
           } | sort -u )

if [ -z "$changed" ]; then
  echo "citation-check: no design docs changed"
  exit 0
fi

# A citation is any of: a spec section (§N), a decision (D1-D16), a tracker
# issue (DKT-N), or a source location (path.go:NN). Deliberately broad — the
# gate asks whether the author pointed at evidence AT ALL, and only a human
# can judge whether the evidence supports the claim.
CITATION='§[0-9]|\bD1[0-6]?\b|\bD[1-9]\b|\bDKT-[0-9]+\b|\.go:[0-9]+'

# `while IFS= read -r`, never `for f in $changed`: word splitting turns
# `docs/design/my amendment.md` into two names that are not files, and the
# `[ -f ]` guard then skips both — an uncited doc reported as OK.
failed=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  [ -f "$f" ] || continue
  if ! grep -qE "$CITATION" "$f"; then
    echo "citation-check: $f cites nothing — no §section, D-number, DKT issue, or source location" >&2
    failed=1
  fi
done <<< "$changed"

if [ "$failed" -ne 0 ]; then
  echo "" >&2
  echo "citation-check FAILED. docs/design/amendments.md requires an amendment" >&2
  echo "to cite evidence, and decisions D1-D16 by number." >&2
  exit 1
fi

echo "citation-check: ok"
