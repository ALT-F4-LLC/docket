package trust

import "fmt"

// Match is the result of a trust lookup (§7.2).
//
// THE TYPE IS T4'S CLOSURE. There is deliberately no API in this package that
// returns a bare boolean "is this trusted?", because such an API produces a
// PERMISSION that a caller then applies to an argv it holds separately — and
// between the permission and the spawn, that argv can be anything. A Match
// carries THE ARGV THAT MUST BE EXECUTED, so there is no second source for a
// caller to get wrong and no window for a concurrent rewrite to exploit.
//
// TestMatchReturnsTheExecutedArgv asserts exactly this shape.
type Match struct {
	// Matched reports whether an entry authorized the candidate. When false,
	// NOTHING MAY BE EXECUTED: the caller records verdict = "unmatched" and
	// never touches the process table (N1).
	Matched bool
	// Argv is the argv to execute, and the ONLY argv the caller may execute.
	// It is the matched entry's own argv for a named gate, or the candidate
	// argv the entry authorized for a fence gate. Empty when Matched is false.
	Argv []string
	// Entry is the entry that matched, carrying the flags the runner needs:
	// Timeout, Flaky, Tree, ReRunnable. Nil when Matched is false.
	Entry *Entry
	// Reason explains an unmatched result (§6.3). An unmatched verdict has at
	// least four distinct causes and each needs a DIFFERENT remedy — no entry
	// needs `trust add`; bound elsewhere needs `trust add` here or a moved repo
	// restored; prefix not opted in needs a different flag; an unparseable
	// fence line needs an issue-body edit. Without a reason all four render
	// identically and the operator is left hunting.
	Reason string
}

// Lookup matches a candidate argv for a gate name against an immutable store
// snapshot, in the current repo (§7.2 M1–M6).
//
// Purity is the point: (snapshot, repo identity, gate name, candidate argv) →
// (matched entry | unmatched reason), with no I/O. The store was read ONCE into
// the snapshot at the start of the gate stage, so there is no second read to
// race.
//
// candidateArgv is nil for a NAMED gate — the entry's own argv is the command,
// so the match is by name and binding. It is the tokenized fence line for a
// FENCE gate, which must additionally hash-equal the entry (or be element-wise
// prefixed by it, when and only when the entry opted in).
func (s *Store) Lookup(repoIdentity, gate string, candidateArgv []string) Match {
	if s == nil {
		return Match{Reason: unmatchedNoEntry(gate)}
	}

	// M2: candidate entries are those whose name equals the gate name AND
	// whose binding matches the current repo identity.
	var boundElsewhere []string
	var prefixNotOptedIn bool
	// boundHere records that SOME entry of this name is trusted in THIS repo,
	// which is what separates "you have no entry" from "your entry approves a
	// different command" (DKT-64).
	var boundHere bool

	for i := range s.Entries {
		e := &s.Entries[i]
		if e.Name != gate {
			continue
		}

		if !bindingMatches(e, repoIdentity) {
			// Remembered for the diagnostic: §3.4's moved-repo case is the one
			// an operator actually hits, and "unmatched" with no explanation
			// sends them hunting. Naming the bound path tells them at once
			// whether they moved the repo or are standing in a clone.
			boundElsewhere = append(boundElsewhere, e.Repo)
			continue
		}
		boundHere = true

		// M1: THE MATCHED ENTRY'S OWN ARGV IS WHAT EXECUTES.
		if candidateArgv == nil {
			// A named gate. The entry IS the command. The stored hash is
			// verified against the stored argv to catch a corrupted or
			// hand-edited file (M3) — validateEntry already did this at parse
			// time, so reaching here with a mismatch is impossible; the check
			// is repeated because the cost is one hash and the failure mode it
			// guards is executing an argv the operator did not approve.
			if e.ArgvSHA256 != "" && ArgvSHA256(e.Argv) != e.ArgvSHA256 {
				continue
			}
			return Match{Matched: true, Argv: append([]string(nil), e.Argv...), Entry: e}
		}

		// A fence gate: the candidate argv is the tokenized fence line.
		// M3: it must hash-equal the entry's argv_sha256 ...
		if ArgvSHA256(candidateArgv) == ArgvSHA256(e.Argv) {
			return Match{Matched: true, Argv: append([]string(nil), e.Argv...), Entry: e}
		}

		// ... OR, when and ONLY when the entry opted in, be element-wise
		// prefixed by the entry's argv (M4).
		if isElementWisePrefix(e.Argv, candidateArgv) {
			if !e.Prefix {
				// M4: a full-argv entry NEVER matches by prefix. Recorded for
				// the diagnostic rather than silently skipped, because "prefix
				// not opted in" has its own remedy.
				prefixNotOptedIn = true
				continue
			}
			return Match{Matched: true, Argv: append([]string(nil), candidateArgv...), Entry: e}
		}
	}

	// M5: NO ENTRY ⇒ UNMATCHED. Never a fallback, never a default-allow, never
	// a "the command looks safe" heuristic. Core has no opinion about which
	// commands are safe; that is the operator's decision and the entire point
	// of the file.
	// THE DIAGNOSTIC LEADS WITH THE OPERATOR'S ACTUAL SITUATION (DKT-64).
	//
	// The bound-elsewhere message used to say "restore the repo to that path if
	// it was moved" whenever an entry of this name existed in another repo —
	// which is every time this branch is reached, including when nothing was
	// moved and this repo simply never had an entry. Five projects hit it on
	// one gate whose only entry is bound to a different repository, and the
	// message sent every one of them hunting for a path problem that did not
	// exist.
	//
	// So the remedy an operator needs comes FIRST — approve it here — and the
	// other repository's binding rides along as the aside it is, with the
	// moved-path reading offered conditionally rather than asserted. The
	// `boundHere` case is new for the same reason: an entry trusted here whose
	// argv is not this command used to fall through to "no trust entry", which
	// is false and points at the wrong verb.
	switch {
	case prefixNotOptedIn:
		return Match{Reason: fmt.Sprintf("gate %q: an entry with this name matches as a prefix, but the entry did not opt in to prefix matching; re-add it with --prefix if that is what you intend", gate)}
	case boundHere:
		return Match{Reason: fmt.Sprintf("gate %q: an entry with this name is trusted in this repo, but the argv it approves is not this command; re-add it with the argv you intend to run", gate)}
	case len(boundElsewhere) > 0:
		return Match{Reason: fmt.Sprintf("gate %q: no trust entry for this repo; approve it with `docket trust add` if you intend to run it. An entry of this name exists but is bound to a different repo (%s) — if this repo was moved from there, restoring the path resolves it too", gate, boundElsewhere[0])}
	default:
		return Match{Reason: unmatchedNoEntry(gate)}
	}
}

func unmatchedNoEntry(gate string) string {
	return fmt.Sprintf("gate %q: no trust entry; approve it with `docket trust add` if you intend to run it", gate)
}

// bindingMatches is P2: an entry matches the current repo iff entry.repo
// STRING-EQUALS the current identity, or entry.global is true.
//
// Both sides are already symlink-resolved absolute paths — RepoIdentity
// resolves the current one and Add resolves the stored one — so string equality
// is the whole comparison. M6 applies: byte-exact, no normalization. Every
// match-widening operation is an authorization decision, and one made by a
// helper function nobody reviewed as one is the worst kind.
func bindingMatches(e *Entry, repoIdentity string) bool {
	if e.Global {
		return true
	}
	return e.Repo != "" && e.Repo == repoIdentity
}

// isElementWisePrefix reports whether prefix is an ELEMENT-WISE prefix of argv
// (§3.3).
//
// ELEMENT-WISE, NEVER STRING-PREFIX, is stated in the spec because the wrong
// reading is both easy and dangerous: a string-prefix comparison would let
// ["make"] match ["make-release","--prod"], authorizing a command the operator
// never saw. Here, ["make"] matches ["make","test"] and ["make","anything"] but
// NOT ["makefoo"] — the comparison is per element, never per character.
func isElementWisePrefix(prefix, argv []string) bool {
	if len(prefix) == 0 || len(prefix) > len(argv) {
		return false
	}
	for i, p := range prefix {
		// M6: case-sensitive and byte-exact on every element.
		if argv[i] != p {
			return false
		}
	}
	return true
}
