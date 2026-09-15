package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// GateFingerprint is the content half of a gate failure's signature (DKT-1796):
// a SHA-256 over the gate's capture with run-varying text removed, so one
// operator ruling covers the failure the operator actually read and nothing
// else.
//
// (gate, exit, reason) alone is a GATE-WIDE waiver in practice. `Reason` is set
// only for a timeout or a refusal, so an ordinary failing gate's signature is
// (gate, exit, empty) and every later failure of that gate with that exit rides
// the same grant — including a real regression nobody looked at, which is the
// defect three DKT-V417 seats converged on.
//
// THE NORMALIZATION IS DELIBERATELY MINIMAL, and under-normalizing is the safe
// direction. Text that survives normalization can only make two failures look
// DIFFERENT — a grant that stops matching parks a step for a human, which is
// the pre-DKT-546 posture. Text that is stripped makes two failures look the
// SAME, which re-creates the gate-wide waiver inside the control meant to
// remove it. So the rules below strip only text that varies between two runs of
// one unchanged failure, and nothing that could name WHICH failure it is: test
// names, assertion text, file names and line numbers all survive.
//
// The exact rules, which docs/tdd/reliability-delta.md states as the contract:
//
//  1. ANSI CSI/OSC escape sequences are removed.
//  2. Carriage returns are dropped and trailing horizontal whitespace on each
//     line is removed, so CRLF and a progress redraw hash as their plain form.
//  3. Go-style durations (`1.234s`, `0.5ms`, `2m30.1s`) become `<dur>`.
//  4. Integer-with-unit durations (`in 42ms`, `after 3s`) are covered by the
//     same rule, which is why it accepts a bare integer mantissa.
//  5. RFC3339-ish timestamps and `HH:MM:SS(.fff)` clock times become `<time>`.
//  6. Absolute POSIX paths become `<path>/<tail>`: the leading directories are
//     replaced and the LAST TWO segments are kept. A scratch root or worktree
//     prefix varies per run; the tail is what names the file that failed, and
//     dropping it would let any two failures in different files collide.
//  7. Leading and trailing blank lines are removed.
//
// Nothing else is touched. In particular line ORDER is preserved and no line is
// deduplicated or sorted: a re-ordered failure set is a different failure set.
func GateFingerprint(output string) string {
	sum := sha256.Sum256([]byte(normalizeGateOutput(output)))
	return hex.EncodeToString(sum[:])
}

// shortFingerprintHex is how many hex characters an event payload carries. The
// event names a signature for a human comparing two ledger lines; the full
// value is on the row for anyone who needs to match exactly.
const shortFingerprintHex = 12

// shortFingerprint abbreviates a fingerprint for an event payload, leaving an
// empty one empty — an event must not print a signature where none was
// recorded.
func shortFingerprint(fingerprint string) string {
	if len(fingerprint) <= shortFingerprintHex {
		return fingerprint
	}
	return fingerprint[:shortFingerprintHex]
}

var (
	ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	// durationText matches a Go duration as `go test` and the engine print it,
	// optionally with a minute or hour component.
	durationText = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:h\d+(?:\.\d+)?m)?(?:m\d+(?:\.\d+)?s|[munµ]?s|h|m)\b`)
	timestampRFC = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	clockTime    = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b`)
	absolutePath = regexp.MustCompile(`/(?:[\w.@%+~-]+/)+[\w.@%+~-]+`)
)

func normalizeGateOutput(output string) string {
	s := ansiEscape.ReplaceAllString(output, "")
	s = strings.ReplaceAll(s, "\r", "")
	s = timestampRFC.ReplaceAllString(s, "<time>")
	s = clockTime.ReplaceAllString(s, "<time>")
	s = absolutePath.ReplaceAllStringFunc(s, elidePathPrefix)
	s = durationText.ReplaceAllString(s, "<dur>")

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// elidePathPrefix keeps a path's last two segments and replaces everything
// before them, so `/private/tmp/claude-501/w-6/internal/app/run.go` and
// `/Users/o/wt/a/internal/app/run.go` agree while `internal/tui/view.go` still
// differs from `internal/app/run.go`.
func elidePathPrefix(path string) string {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segments) <= 2 {
		return "<path>/" + strings.Join(segments, "/")
	}
	return "<path>/" + strings.Join(segments[len(segments)-2:], "/")
}
