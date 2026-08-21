package workflow

import (
	"fmt"
	"strings"
)

// PACKET COMPOSITION — the grammar half
// (docs/tdd/packet-composition.md §1.1).
//
// A step's `packet` names the files it needs in its rendered work packet. The
// engine reads their bytes against the hash activation pinned and hands them to
// the template; it never interprets what a file says.
//
// THE SUBSTITUTION IS THE ONLY CLEVERNESS, AND IT IS DELIBERATELY DUMB. Core
// replaces one token with one string. It does not know one directory from
// another, cannot fall back when a file is missing, and cannot collapse a
// family of hints onto a shared file. The INSTANCE authors the mapping rule as
// syntax, in its own workflow file; core performs a string replacement.
//
// That distinction is what keeps this generic. A convention baked into core —
// "look in contracts/ for <executor>.md" — would make core the arbiter of an
// instance's layout, and a family of hints needing one shared file would then
// require a prefix-stripping rule in core that only one instance's naming
// justifies. With the token, that case is the instance writing a literal.

// PacketExecutorToken is the one substitution token a `packet` entry may carry.
//
// It is the ONLY per-sibling identity a step row holds that varies across
// siblings of one step, which is why it is the only token. `{name}`,
// `{instance}`, and `{ordinal}` are deliberately absent: each would be a
// speculative surface with no motivating case, and every additional token is
// another chance for core to look like it is deriving meaning from an
// instance's filenames.
const PacketExecutorToken = "{executor}"

// SubstitutePacketEntry resolves one `packet` entry against a step's executor
// hint.
//
// An entry with no token returns unchanged, which is how a family of hints
// shares one file: the instance writes a literal and every sibling resolves to
// it, while each sibling's own identity still reaches the rendered packet
// through its executor hint.
//
// An UNKNOWN token is left alone rather than refused. Erroring on any brace
// would make core the arbiter of a filename grammar; leaving it literal means
// the pin check fails naming the exact path the author wrote, which is the more
// useful message.
func SubstitutePacketEntry(entry, executor string) string {
	if executor == "" {
		return entry
	}
	return strings.ReplaceAll(entry, PacketExecutorToken, executor)
}

// substitutePacket resolves a whole declared list for one sibling, preserving
// DECLARED ORDER — which is the order the assembler inlines them in.
//
// It returns a fresh slice rather than mutating the definition's: expansion is
// a pure function of the definition, and two siblings of one step must not
// share backing storage that the second would overwrite for the first.
func substitutePacket(entries []string, executor string) []string {
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = SubstitutePacketEntry(entry, executor)
	}
	return out
}

// hasPacketToken reports whether an entry carries the executor token. V33 uses
// it to refuse a token on a step that has no hint to substitute.
func hasPacketToken(entry string) bool {
	return strings.Contains(entry, PacketExecutorToken)
}

// validatePacket is V32 and V33.
//
// V32 is SHAPE ONLY, in the spirit of V25: a file's EXISTENCE is an
// activation-time fact, not a registration-time one. A workflow must stay
// registerable before the files it names are added — the zero-touch bootstrap
// registers a workflow and its corpus in whichever order it drafts them, and a
// register-time existence check would make one order fail for no reason.
// Activation is where a declared-but-unpinned entry is refused.
//
// V33 keeps the substitution honest: `{executor}` on a step with no per-sibling
// hint would substitute to nothing and yield a path like `checklists/.md` — a
// missing-pin error much later, naming a path no author wrote.
func validatePacket(step *Step) error {
	for _, entry := range step.Packet {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			return &Error{
				Rule: "V32", Step: step.Name, Field: "packet",
				Message: fmt.Sprintf(
					"step %q: a `packet` entry is empty; every entry names a file "+
						"relative to the instance-config directory", step.Name),
			}
		}

		if err := validatePacketPath(step, entry, trimmed); err != nil {
			return err
		}

		if hasPacketToken(entry) && !hasExecutorHint(step) {
			return &Error{
				Rule: "V33", Step: step.Name, Field: "packet",
				Message: fmt.Sprintf(
					"step %q: `packet` entry %q carries the %s token, but this step "+
						"declares no `executor` and no `fanout`, so there is no hint "+
						"to substitute; name the file directly",
					step.Name, entry, PacketExecutorToken),
			}
		}
	}
	return nil
}

// validatePacketPath is V32's containment half: an entry is RELATIVE and stays
// lexically inside the config directory.
//
// The check is lexical rather than filesystem-resolved because Validate is a
// pure function of bytes (§4.9.2) — it may not consult a disk. That is
// sufficient here: the pin check at render refuses anything activation did not
// pin, so a path that escapes lexically is refused at register with a clear
// message, and a path that would escape only via a symlink is refused later for
// having no pin.
func validatePacketPath(step *Step, entry, trimmed string) error {
	refuse := func(why string) error {
		return &Error{
			Rule: "V32", Step: step.Name, Field: "packet",
			Message: fmt.Sprintf(
				"step %q: `packet` entry %q %s; entries are relative paths inside "+
					"the instance-config directory", step.Name, entry, why),
		}
	}

	if strings.HasPrefix(trimmed, "/") {
		return refuse("is an absolute path")
	}
	if strings.HasPrefix(trimmed, "~") {
		return refuse("starts with `~`")
	}
	for _, segment := range strings.Split(filepathSlashes(trimmed), "/") {
		if segment == ".." {
			return refuse("escapes the config directory with `..`")
		}
	}
	return nil
}

// filepathSlashes normalizes a declared entry's separators so the containment
// check reads one form. Workflow files are authored with `/` regardless of the
// platform they run on — a definition is portable data, not a local path — and
// a backslash-separated entry is normalized rather than accepted as an opaque
// filename that would slip past the `..` scan.
func filepathSlashes(p string) string {
	return strings.ReplaceAll(p, `\`, "/")
}

// hasExecutorHint reports whether a step carries a per-sibling executor hint —
// directly (`executor`) or one per sibling (`fanout`).
func hasExecutorHint(step *Step) bool {
	return step.Executor != "" || len(step.Fanout) > 0
}
