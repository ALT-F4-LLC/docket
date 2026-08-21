package exec

import (
	"strconv"
	"strings"
)

// Render escapes one attacker-influenced value for display on a TERMINAL
// (§5.7, T18). It is THE renderer: E2 requires exactly one, used by every
// human-mode print of the three value classes.
//
// THE ATTACK IT CLOSES is not exotic. Three value classes in this stage are
// attacker-influenced and printed to the operator: an argv (from a fence line
// an issue author wrote, or echoed back by `trust add`), a fence command, and a
// reason string (which quotes fence content). All three reach a TTY, and a TTY
// INTERPRETS CONTROL BYTES. A fence line of
//
//	make test\x1b[2K\r  make lint
//
// prints as `  make lint` — the escape clears the line and the carriage return
// resets the cursor, so the operator approving what they SEE approves something
// else. Variants: \x1b[1A to overwrite the line above, a bare \r for the same
// effect without CSI, \x1b]0;…\x07 to rewrite the window title, \x08 runs.
//
// The human backstop for the whole trust posture is the premise that WHAT IS
// DISPLAYED IS WHAT IS APPROVED, so any divergence between rendered text and
// stored bytes attacks the control directly rather than being a cosmetic bug.
//
// THE MECHANISM IS Go's %q — strconv.Quote semantics — NOT control-character
// stripping (E1). Chosen deliberately: %q is LOSSLESS AND REVERSIBLE, so the
// operator sees that something odd is present rather than seeing
// sanitized-but-plausible text with the hostile bytes silently removed.
// Stripping would render the attack line as `make test  make lint`, which is
// STILL misleading; escaping renders it as what it is. It is also one stdlib
// call with no hand-rolled character table to get wrong.
func Render(s string) string {
	return strconv.Quote(s)
}

// RenderArgv escapes an argv for display: each element individually quoted and
// space-joined (E2).
//
// Per-element quoting rather than quoting a joined string is what keeps the
// element boundaries visible — an argument containing a space renders as one
// quoted token, so the operator can see that ["make","a b"] is two arguments
// and not three.
func RenderArgv(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = Render(a)
	}
	return strings.Join(parts, " ")
}

// E5, stated where an implementer will read it: THE STORED BYTES ARE NEVER
// MODIFIED. Escaping is a RENDERING transform applied at the print boundary —
// run_fences.command, gate_results.argv, and a trust entry's argv all keep
// exactly the bytes that were harvested or approved. Mutating stored bytes
// would break the fence hash re-verification, which is the one thing that must
// compare unmodified content.
//
// E4, likewise: JSON MODE IS UNTOUCHED and is inherently safe. encoding/json
// escapes control bytes by contract, and the consumer is a program that does
// not interpret them. Quoting on top of JSON encoding would double-escape and
// corrupt the value a machine consumer reads, so --json output carries the RAW
// bytes exactly as stored. The asymmetry is the point: the escaping exists for
// terminals, and only terminals get it.
