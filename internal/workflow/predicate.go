package workflow

import (
	"fmt"
	"strconv"
	"strings"
)

// Threshold predicates — the §11.2 grammar, PARSED.
//
// V21 validates the shape at register time; this decomposes the same string
// into its parts for evaluation. Both go through predicateShape, so the grammar
// a definition is validated against and the grammar it is evaluated under are
// ONE regex. Two spellings of the same grammar would drift on the first
// operator someone added.
//
// Nothing here knows what a field MEANS. `severity`, `status`, and `x` are
// opaque tokens; the aggregation and the operator are core mechanics and the
// field and literal are instance data (docs/design/genericity.md). That is the
// genericity line exactly where §11.2 draws it.

// Aggregations of §11.2: `agg ∈ {any, all, count>=n}`.
const (
	AggAny   = "any"
	AggAll   = "all"
	AggCount = "count"
)

// Operators of §11.2: `op ∈ {==, !=, >=, >, <=, <}`.
const (
	OpEQ = "=="
	OpNE = "!="
	OpGE = ">="
	OpGT = ">"
	OpLE = "<="
	OpLT = "<"
)

// Predicate is one parsed `agg(field op literal)`.
type Predicate struct {
	// Agg is one of AggAny, AggAll, AggCount.
	Agg string
	// Count is n for `count>=n`, and 0 otherwise.
	Count int
	// Field is the payload field name — an opaque token until its schema
	// registers at S5.
	Field string
	// Op is one of the six operators.
	Op string
	// Literal is the right-hand value, verbatim as written.
	Literal string
	// Source is the predicate string as the author wrote it, kept so a refusal
	// can quote it back verbatim rather than re-render it (§6.14 T3 requires
	// the predicate in the reason string).
	Source string
}

// Ordered reports whether the operator is an ORDERED comparison — the ones
// §11.2 defines "only for fields whose registered schema declares
// `ordered_enum`". Equality is not ordered: knowing whether two values are the
// same value requires no schema.
func (p Predicate) Ordered() bool {
	switch p.Op {
	case OpGE, OpGT, OpLE, OpLT:
		return true
	}
	return false
}

// The reserved `diff.*` family (DKT-2063): the engine's own measurement of the
// change a tree-holding step recorded, addressable from a threshold predicate
// like a payload field but never declared by a payload schema.
//
// The names live HERE rather than in the engine because the validator must
// know them (V45-V47 and the V21a exemption) and internal/engine imports this
// package, not the reverse — the same reason VoteCastFields is here. The engine
// evaluates over them; this package decides at register time that a predicate
// naming them can mean something.
const (
	DiffFieldLines = "diff.lines"
	DiffFieldFiles = "diff.files"
	DiffFieldEmpty = "diff.empty"
)

// DiffFields is the closed reserved vocabulary — exactly these three names. A
// lookalike such as `diff.bogus` is an ordinary undeclared field, so a match
// here is by name and never by prefix.
var DiffFields = []string{DiffFieldLines, DiffFieldFiles, DiffFieldEmpty}

// IsDiffField reports whether a predicate field is one of the reserved
// `diff.*` names.
func IsDiffField(field string) bool {
	switch field {
	case DiffFieldLines, DiffFieldFiles, DiffFieldEmpty:
		return true
	}
	return false
}

// ParsePredicate decomposes a §11.2 predicate string.
//
// It returns an error only for a string V21 would already have rejected, so a
// registered definition never produces one — but the check stays, because a
// definition can reach the engine through a database restored from elsewhere.
func ParsePredicate(src string) (Predicate, error) {
	m := predicateShape.FindStringSubmatch(src)
	if m == nil {
		return Predicate{}, fmt.Errorf(
			"predicate %q must have the shape `agg(field op literal)` with agg in "+
				"{any, all, count>=n} and op in {==, !=, >=, >, <=, <}", src)
	}

	p := Predicate{Field: m[2], Op: m[3], Literal: m[4], Source: src}

	agg := m[1]
	switch {
	case agg == AggAny, agg == AggAll:
		p.Agg = agg
	case strings.HasPrefix(agg, "count>="):
		n, err := strconv.Atoi(strings.TrimPrefix(agg, "count>="))
		if err != nil {
			return Predicate{}, fmt.Errorf("predicate %q: %q is not a count", src, agg)
		}
		p.Agg, p.Count = AggCount, n
	default:
		return Predicate{}, fmt.Errorf("predicate %q: unknown aggregation %q", src, agg)
	}

	return p, nil
}
