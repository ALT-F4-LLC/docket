#!/usr/bin/env bash
#
# sdet-abuse — abuse-case tests exist and pass for a security-load-bearing change.
#
# Wired as a gate on security-load-bearing.toml's `implement` step, whose header
# describes the intent as "named abuse cases from the threat model actually
# executed".
#
# WHAT THIS GATE CANNOT DO, STATED PLAINLY. It cannot verify that the abuse
# cases NAMED BY THIS RUN'S THREAT MODEL were the ones executed. A gate process
# receives an allowlisted environment plus TERM, CI, DOCKET_GATE, and
# DOCKET_REPO (internal/exec/env.go BuildEnv) and nothing else — no run id, no
# issue id, no step identity — so it cannot locate the `threat-model` step's
# artifact to read the case names out of it. Closing that gap needs the engine
# to pass step identity into the gate, not a change here.
#
# So this checks the weaker property it CAN check honestly: that the repo holds
# abuse/adversarial tests, and that they pass. A reviewer still has to confirm
# the cases match the threat model. The gate name is kept as the corpus names
# it, and this header is the record of what it does and does not cover.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

echo "=== sdet-abuse: adversarial/abuse-case tests ==="

# THE PATTERN IS THIS REPO'S OWN IDIOM, not a generic abuse-test vocabulary.
# An earlier version of this gate matched Abuse|Attack|Malicious|Tamper|... and
# hit exactly ONE test in the whole tree (TestMaliciousCloneExecutesNothing),
# passing vacuously while the project's real adversarial suite — hundreds of
# tests named for what the system REFUSES — went unrun. The repo names an abuse
# case after the refusal it asserts: TestActivateRefusesAnUnregisteredSchema,
# TestChangedBytesAtSameVersionRefusesActivation,
# TestDocketTokenNeverReachesAGateChild, TestCastVoteDuplicateVoterRejected.
#
# A gate that passes because it found nothing is the failure this repo's QA
# history calls a vacuous pass, and it is worse than no gate.
pattern='Refuse|Refus|Reject|Denied|Malicious|Abuse|Tamper|Untrusted|Unsafe|Forbidden|NeverReach|Never[A-Z]|Leak|Escape|Traversal|Symlink|Outside|Hostile|Stranger'

# The verb sits in the MIDDLE of this repo's test names, not right after
# `Test`: TestActivateRefusesAnUnregisteredSchema, TestAckOfABogusSeqIsRefused.
# Anchoring the pattern at the front matched 5 tests instead of 150+.
matches=$(grep -rlE "func Test[A-Za-z0-9_]*($pattern)" --include='*_test.go' . 2>/dev/null | sort -u || true)

if [ -z "$matches" ] && [ ! -f scripts/qa/test_zh_stranger.sh ]; then
  cat >&2 <<'EOF'
sdet-abuse FAILED: no abuse-case tests found.

A security-load-bearing change is gated on adversarial tests existing. Add Go
tests named for the abuse case they exercise (TestAbuse..., TestMalicious...,
TestTampered..., TestUntrusted...), or a scripts/qa/ section that drives the
hostile input path.

This gate fails rather than skipping: a security change whose abuse cases were
never run has not been checked, and recording that as a pass would misreport
the change's coverage.
EOF
  exit 1
fi

count=$(grep -rhoE "func Test[A-Za-z0-9_]*($pattern)[A-Za-z0-9_]*" --include='*_test.go' . 2>/dev/null | sort -u | wc -l | tr -d ' ')

# THE ANTI-VACUITY FLOOR. The tree carries well over a hundred refusal tests
# today. If this count collapses, the pattern has drifted away from the naming
# the suite actually uses and the gate is about to start passing on nothing —
# which is the failure mode this floor exists to convert into a loud one.
readonly MIN_ABUSE_TESTS=25

if [ -n "$matches" ]; then
  echo "abuse-case test files: $(printf '%s\n' $matches | wc -l | tr -d ' ')"
  echo "abuse-case tests:      $count"
else
  echo "no Go abuse-case tests matched; relying on the QA harness section below"
fi

if [ "$count" -lt "$MIN_ABUSE_TESTS" ]; then
  cat >&2 <<EOF

sdet-abuse FAILED: only $count abuse-case test(s) matched, below the floor of
$MIN_ABUSE_TESTS.

This is almost certainly the GATE being wrong, not the suite: the pattern has
drifted from how this repo names its refusal tests. Fix the pattern in this
script rather than lowering the floor — a floor lowered to match a broken
pattern is how a gate starts passing on nothing.
EOF
  exit 1
fi

if [ -f scripts/qa/test_zh_stranger.sh ]; then
  echo "  scripts/qa/test_zh_stranger.sh (stranger/untrusted-input section)"
fi

echo
echo "--- running the matched abuse-case tests ---"

# Run ONLY the abuse-case tests, not the whole suite: `tests` is its own gate
# and re-running it here would make one failure surface as two.
if [ -n "$matches" ]; then
  if ! go1.26.6 test -run "Test[A-Za-z0-9_]*($pattern)" ./... 2>&1; then
    cat >&2 <<'EOF'

sdet-abuse FAILED: an abuse-case test did not pass.

The failing case is named above. On a security-load-bearing change this is the
signal the gate exists for.
EOF
    exit 1
  fi
fi

echo "sdet-abuse: ok"
exit 0
