package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// CanonicalArgv returns the canonical encoding of an argv (§3.3): the JSON
// encoding of the string list, with no whitespace.
//
// JSON RATHER THAN A DELIMITER-JOINED STRING, because NO DELIMITER IS SAFE. An
// argument containing a NUL is impossible, but one containing a newline, a
// space, or any chosen separator is trivial — and a join-then-hash scheme makes
// ["a b"] and ["a","b"] collide. That collision is an argv-injection primitive
// in the matcher itself, which is exactly the class of bug T2 is about: it
// would let an operator who approved one command find a different one
// authorized. TestCanonicalArgvHashDoesNotCollide is the test that matters.
func CanonicalArgv(argv []string) string {
	// A nil argv and an empty argv canonicalize identically, to "[]", which is
	// correct: neither names a program and neither can ever match a candidate.
	if argv == nil {
		argv = []string{}
	}
	b, err := json.Marshal(argv)
	if err != nil {
		// json.Marshal of []string cannot fail; the encoder has no path to an
		// error for this type. Returning the empty canonical form rather than
		// panicking keeps the failure closed: it can never equal a real hash.
		return "[]"
	}
	return string(b)
}

// ArgvSHA256 is the hex-encoded SHA-256 of the canonical argv. It is what an
// entry stores and what a candidate is compared against.
func ArgvSHA256(argv []string) string {
	sum := sha256.Sum256([]byte(CanonicalArgv(argv)))
	return hex.EncodeToString(sum[:])
}
