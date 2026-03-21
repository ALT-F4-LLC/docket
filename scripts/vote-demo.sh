#!/usr/bin/env bash
#
# Docket Vote Subcommand — Interactive Demo Script
#
# Runs through all scenarios for `docket vote` with a 5-second pause between
# each command so you can review the output.
#
# Usage:
#   ./scripts/vote-demo.sh [path/to/docket-binary]
#
# If no binary path is given, builds from source with `go build`.

set -euo pipefail

# --- Configuration -----------------------------------------------------------

DOCKET="${1:-}"
DELAY=5

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

# --- Helpers -----------------------------------------------------------------

section() {
  echo ""
  echo -e "${BOLD}${CYAN}═══════════════════════════════════════════════════════════════${RESET}"
  echo -e "${BOLD}${CYAN}  $1${RESET}"
  echo -e "${BOLD}${CYAN}═══════════════════════════════════════════════════════════════${RESET}"
  echo ""
}

run() {
  local description="$1"
  shift
  echo -e "${YELLOW}▸ ${description}${RESET}"
  echo -e "${BOLD}\$ $*${RESET}"
  echo ""
  # Run the command — allow failures so the script continues
  "$@" 2>&1 || true
  echo ""
  echo -e "${GREEN}— pausing ${DELAY}s —${RESET}"
  sleep "$DELAY"
}

expect_error() {
  local description="$1"
  shift
  echo -e "${RED}▸ [EXPECT ERROR] ${description}${RESET}"
  echo -e "${BOLD}\$ $*${RESET}"
  echo ""
  "$@" 2>&1 || true
  echo ""
  echo -e "${GREEN}— pausing ${DELAY}s —${RESET}"
  sleep "$DELAY"
}

# --- Build -------------------------------------------------------------------

if [ -z "$DOCKET" ]; then
  echo -e "${CYAN}Building docket from source...${RESET}"
  go build -o /tmp/docket-vote-demo ./cmd/docket
  DOCKET="/tmp/docket-vote-demo"
  echo -e "${GREEN}Built: ${DOCKET}${RESET}"
fi

# --- Setup -------------------------------------------------------------------

TMPDIR=$(mktemp -d)
export DOCKET_PATH="$TMPDIR/.docket"
trap 'rm -rf "$TMPDIR"' EXIT

section "SETUP: Initialize fresh database"

run "Initialize docket (creates v1 schema + v2 migration)" \
  "$DOCKET" init

run "Create a test issue for linking later" \
  "$DOCKET" issue create -t "Implement auth middleware" --json

run "Create a second test issue" \
  "$DOCKET" issue create -t "Add rate limiting" --json

# =============================================================================
section "CREATE: Basic proposal creation"
# =============================================================================

run "Create proposal with 3 required voters" \
  "$DOCKET" vote create --json -d "Approve TDD for auth refactor" -n 3

run "Create critical proposal with custom threshold (4 voters, 75%)" \
  "$DOCKET" vote create --json -d "Security review: API key rotation" -c critical -n 4 --threshold 0.75

run "Create proposal with creator identity (2 voters)" \
  "$DOCKET" vote create --json -d "Scope change: add caching layer" -n 2 --created-by "team-lead"

run "Create low-criticality proposal (1 voter)" \
  "$DOCKET" vote create --json -d "Rename config field" -c low -n 1

run "Create proposal for zero-weight edge case (1 voter)" \
  "$DOCKET" vote create --json -d "Zero weight edge case test" -n 1

# =============================================================================
section "CREATE: Validation errors"
# =============================================================================

expect_error "Missing required --voters flag" \
  "$DOCKET" vote create --json -d "Missing voters"

expect_error "Invalid criticality value" \
  "$DOCKET" vote create --json -d "Bad criticality" -n 2 -c "extreme"

expect_error "Threshold > 1.0" \
  "$DOCKET" vote create --json -d "Bad threshold" -n 2 --threshold 1.5

expect_error "Threshold = 0.0" \
  "$DOCKET" vote create --json -d "Zero threshold" -n 2 --threshold 0.0

expect_error "Zero voters" \
  "$DOCKET" vote create --json -d "Zero voters" -n 0

# =============================================================================
section "LIST & SHOW: Query proposals"
# =============================================================================

run "List open proposals (default)" \
  "$DOCKET" vote list --json

run "List all proposals including resolved" \
  "$DOCKET" vote list --json --all

run "Filter by criticality" \
  "$DOCKET" vote list --json -c critical

run "Show proposal DKT-V1" \
  "$DOCKET" vote show DKT-V1 --json

run "Show proposal using bare integer ID" \
  "$DOCKET" vote show 1 --json

expect_error "Show nonexistent proposal" \
  "$DOCKET" vote show DKT-V999 --json

# =============================================================================
section "CAST: Happy path — votes on DKT-V1 (needs 3)"
# =============================================================================

run "First vote: security reviewer approves" \
  "$DOCKET" vote cast DKT-V1 --json \
    --voter "security-reviewer" --role "security" \
    -v approve --confidence 0.9 --domain-relevance 0.85 \
    --findings "No security concerns identified"

run "Second vote: architecture reviewer approves" \
  "$DOCKET" vote cast DKT-V1 --json \
    --voter "arch-reviewer" --role "architecture" \
    -v approve --confidence 0.95 --domain-relevance 0.9 \
    --findings "Clean separation of concerns"

run "Check result mid-vote (should be open, no score)" \
  "$DOCKET" vote result DKT-V1 --json

# =============================================================================
section "CAST: Quorum reached — auto-finalization"
# =============================================================================

run "Third vote triggers finalization (3/3)" \
  "$DOCKET" vote cast DKT-V1 --json \
    --voter "code-reviewer" --role "quality" \
    -v approve --confidence 0.8 --domain-relevance 0.7 \
    --findings "Code quality meets standards"

run "Result should show APPROVED with weighted score" \
  "$DOCKET" vote result DKT-V1 --json

run "Show reflects finalized status" \
  "$DOCKET" vote show DKT-V1 --json

# =============================================================================
section "CAST: Rejection scenario — DKT-V4 (1 voter)"
# =============================================================================

run "Single reject vote finalizes as rejected" \
  "$DOCKET" vote cast DKT-V4 --json \
    --voter "reviewer" --role "general" \
    -v reject --confidence 0.8 --domain-relevance 0.9 \
    --findings "Naming convention doesn't match project standards"

run "Result should show REJECTED" \
  "$DOCKET" vote result DKT-V4 --json

# =============================================================================
section "CAST: Mixed verdicts — DKT-V3 (2 voters)"
# =============================================================================

run "First vote: PM approves with moderate confidence" \
  "$DOCKET" vote cast DKT-V3 --json \
    --voter "pm-reviewer" --role "scope" \
    -v approve --confidence 0.7 --domain-relevance 0.6 \
    --findings "Scope is reasonable"

run "Second vote: engineer rejects with high confidence" \
  "$DOCKET" vote cast DKT-V3 --json \
    --voter "eng-reviewer" --role "engineering" \
    -v reject --confidence 0.9 --domain-relevance 0.8 \
    --findings "Too much complexity for the benefit"

run "Result shows weighted score (approve weight vs reject weight)" \
  "$DOCKET" vote result DKT-V3 --json

# =============================================================================
section "CAST: Edge case — all zero weights (DKT-V5)"
# =============================================================================

run "Vote with zero confidence and zero relevance" \
  "$DOCKET" vote cast DKT-V5 --json \
    --voter "zero-voter" -v approve \
    --confidence 0.0 --domain-relevance 0.0

run "Should be REJECTED with score 0.0 (zero-weight edge case)" \
  "$DOCKET" vote result DKT-V5 --json

# =============================================================================
section "CAST: Error cases"
# =============================================================================

expect_error "Duplicate voter on DKT-V1 (already voted)" \
  "$DOCKET" vote cast DKT-V1 --json \
    --voter "security-reviewer" --role "security" \
    -v approve --confidence 0.9 --domain-relevance 0.85 \
    --findings "Second attempt"

expect_error "Vote on finalized proposal DKT-V1" \
  "$DOCKET" vote cast DKT-V1 --json \
    --voter "new-reviewer" --role "late" \
    -v approve --confidence 0.5 --domain-relevance 0.5 \
    --findings "Too late"

expect_error "Confidence out of range (1.5)" \
  "$DOCKET" vote cast DKT-V2 --json \
    --voter "test" -v approve \
    --confidence 1.5 --domain-relevance 0.5

expect_error "Domain relevance out of range (-0.1)" \
  "$DOCKET" vote cast DKT-V2 --json \
    --voter "test" -v approve \
    --confidence 0.5 --domain-relevance -0.1

expect_error "Invalid verdict value" \
  "$DOCKET" vote cast DKT-V2 --json \
    --voter "test" -v "maybe" \
    --confidence 0.5 --domain-relevance 0.5

expect_error "Nonexistent proposal" \
  "$DOCKET" vote cast DKT-V999 --json \
    --voter "test" -v approve \
    --confidence 0.5 --domain-relevance 0.5

# =============================================================================
section "LINK: Proposal-issue linking"
# =============================================================================

run "Link issue DKT-1 to proposal DKT-V1" \
  "$DOCKET" vote link DKT-V1 --issue DKT-1 --json

run "Link issue DKT-2 to proposal DKT-V1" \
  "$DOCKET" vote link DKT-V1 --issue DKT-2 --json

run "Show DKT-V1 — should include linked_issues" \
  "$DOCKET" vote show DKT-V1 --json

expect_error "Duplicate link (DKT-1 already linked)" \
  "$DOCKET" vote link DKT-V1 --issue DKT-1 --json

expect_error "Link nonexistent issue" \
  "$DOCKET" vote link DKT-V1 --issue DKT-999 --json

expect_error "Link to nonexistent proposal" \
  "$DOCKET" vote link DKT-V999 --issue DKT-1 --json

# =============================================================================
section "UNLINK: Remove proposal-issue links"
# =============================================================================

run "Unlink issue DKT-1 from proposal DKT-V1" \
  "$DOCKET" vote unlink DKT-V1 --issue DKT-1 --json

run "Show DKT-V1 — should have only DKT-2 linked" \
  "$DOCKET" vote show DKT-V1 --json

expect_error "Unlink again (already removed)" \
  "$DOCKET" vote unlink DKT-V1 --issue DKT-1 --json

# =============================================================================
section "LIST: Filter resolved proposals"
# =============================================================================

run "List only approved proposals" \
  "$DOCKET" vote list --json -s approved

run "List only rejected proposals" \
  "$DOCKET" vote list --json -s rejected

run "List only open proposals" \
  "$DOCKET" vote list --json -s open

run "List all proposals" \
  "$DOCKET" vote list --json --all

# =============================================================================
section "HUMAN-READABLE OUTPUT (no --json)"
# =============================================================================

run "Vote list — table view" \
  "$DOCKET" vote list --all

run "Vote show — detail view (approved proposal with votes)" \
  "$DOCKET" vote show DKT-V1

run "Vote result — approved banner" \
  "$DOCKET" vote result DKT-V1

run "Vote result — rejected banner" \
  "$DOCKET" vote result DKT-V4

run "Vote result — mixed verdict result" \
  "$DOCKET" vote result DKT-V3

run "Vote result — zero-weight edge case" \
  "$DOCKET" vote result DKT-V5

# =============================================================================
section "DONE"
# =============================================================================

echo -e "${GREEN}${BOLD}All scenarios executed successfully!${RESET}"
echo -e "Temporary database was at: ${TMPDIR}"
echo -e "It will be cleaned up automatically."
