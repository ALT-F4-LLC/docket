#!/usr/bin/env bash
#
# render-verify — the TUI render surface stays covered and stays clean.
#
# Wired as a gate on ui-change.toml's `design-qa` step, alongside copy-verify.
#
# The surface this gates is real and lives in internal/render: RenderBoard,
# RenderTable, RenderDetail, RenderDocList, RenderStepRows, RenderProposalTable
# and their siblings, built on lipgloss/glamour (go.mod names both as direct
# dependencies). `docket --watch`, `docket board`, and `docket graph` are its
# users.
#
# Two properties, both checkable without a terminal:
#
#   1. COVERAGE — every internal/render file that exports a Render* function has
#      a _test.go beside it. A render change with no test is a change nobody can
#      review by diff, because what it produces is a picture.
#
#   2. NO RAW ESCAPES — styling goes through lipgloss, never a hand-written
#      "\033[" literal. Hand-rolled escapes bypass termenv's profile detection,
#      so they leak ANSI into piped output and into --json. internal/output
#      already decides colour by TTY detection (human.go); a raw escape in
#      render/ defeats that decision.
#
# It does NOT screenshot or diff rendered frames. Whether the layout LOOKS right
# is design-qa's judgment (that is the step this gate runs on); this gate keeps
# the surface reviewable.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

if [ ! -d internal/render ]; then
  echo "render-verify: no internal/render package" >&2
  exit 1
fi

status=0
untested=""

echo "=== render-verify: coverage of the render surface ==="

for f in internal/render/*.go; do
  case "$f" in
    *_test.go) continue ;;
  esac

  # Only files that actually export a renderer need a test beside them.
  if ! grep -qE '^func Render' "$f"; then
    continue
  fi

  base="${f%.go}"
  if [ -f "${base}_test.go" ]; then
    echo "  ok       $f"
  else
    # REPORTED, NOT FAILED. internal/render/markdown.go and step.go carry
    # exported renderers with no test today, and that predates this gate. A
    # gate that fails on a pre-existing gap blocks every UI change until an
    # unrelated backlog item is done, so it reports instead — the design-qa
    # step this gate runs on is where a reviewer weighs it.
    #
    # A change that ADDS an untested renderer is caught below, where it should
    # be: against the diff, not against the whole tree.
    echo "  no test  $f has exported Render* but no ${base##*/}_test.go"
    untested="$untested $f"
  fi
done

# The failing condition: a renderer file CHANGED by this step and still
# untested. That is a new gap, and it is the step's own.
#
# VACUOUS IN CI, DELIBERATELY. "Changed" is read from working-tree,
# staged, and untracked state — none of which a `pull_request` checkout has,
# so on that path `changed` is empty and this loop can never set status=1.
# The coverage half therefore reports (the `no test` lines above) but cannot
# fail in CI; only the ANSI-escape half below is load-bearing there.
#
# Left this way on purpose rather than switched to a base-ref diff. The gate's
# subject is "the change this STEP made", and a step is a working-tree edit —
# that is what the `no test` carve-out above is calibrated against. A base-ref
# reading would widen it to the whole PR range and fail on the pre-existing
# markdown.go/step.go gaps, which is what that carve-out exists to prevent.
# CI's copy of this check is a reporter; the design-qa step is where it bites.
changed=$( { git diff --cached --name-only -- internal/render
             git diff --name-only -- internal/render
             git ls-files --others --exclude-standard -- internal/render
           } | sort -u )

for f in $untested; do
  if printf '%s\n' "$changed" | grep -qx "$f"; then
    echo "  MISSING  $f was changed by this step and has no test" >&2
    status=1
  fi
done

echo
echo "=== render-verify: no hand-written ANSI escapes ==="

# \033 / \x1b / \e written as a literal in source. lipgloss emits these itself;
# the gate is about render/ not doing it by hand.
if grep -rnE '\\033\[|\\x1b\[|\\e\[' internal/render/*.go 2>/dev/null | grep -v '_test.go'; then
  echo "  raw ANSI escape(s) above bypass lipgloss/termenv profile detection" >&2
  status=1
else
  echo "  ok       no raw escapes in internal/render"
fi

if [ "$status" -ne 0 ]; then
  cat >&2 <<'EOF'

render-verify FAILED.

The TUI render surface (internal/render) must stay reviewable:
  - a file exporting Render* carries a _test.go beside it, because a rendered
    frame cannot be reviewed from a diff alone; and
  - styling goes through lipgloss, never a hand-written escape sequence, so
    piped and --json output stay clean.
EOF
  exit 1
fi

echo
echo "render-verify: ok"
exit 0
