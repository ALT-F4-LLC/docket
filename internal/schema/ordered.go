package schema

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// annotation is the §4.2 annotation key. It is a plain JSON key in the author's
// document and is NEVER registered as a library keyword (O4): core extracts the
// order itself, by walking the document, so swapping validators changes which
// documents are accepted and never which values are ordered.
const annotation = "ordered_enum"

// conservativeAnnotation is the §4.2 direction annotation: it names WHICH END of
// the sibling `enum`'s ascending order the author considers the cautious one.
//
// It exists because core cannot know which end of a declared order is the bad
// end — that is precisely the reasoning `reductionIndex` documents for taking
// the lower median — but AN ORDER CAN KNOW ITS OWN BAD END. A severity order
// declares `"upper"` and its even-count median ties resolve upward; a
// confidence or a ripeness order declares nothing and behaves exactly as it did
// before this key existed. The opinion is the author's, written in the author's
// document, and core still holds none.
//
// Like `ordered_enum` it is a plain JSON key, never a registered library
// keyword (O4), so swapping validators changes which documents are accepted and
// never which end is conservative.
const conservativeAnnotation = "conservative_end"

// The two ends a `conservative_end` may name, as positions in the ascending
// declared order rather than as domain words. `"upper"` is the LAST enum value,
// `"lower"` is the FIRST. Positional vocabulary is deliberate: `"high"` and
// `"low"` would collide with the very values a severity enum is made of, and a
// reader could not tell the key's vocabulary from the field's.
const (
	ConservativeUpper = "upper"
	ConservativeLower = "lower"
)

// ConservativeEnds is the closed vocabulary, for O6's refusal message and for
// the authoring documentation's table.
var ConservativeEnds = []string{ConservativeUpper, ConservativeLower}

// Index is the §4.3 derived ordered index: each ordered field's declared value
// order, ascending, taken from the `enum` array's DOCUMENT ORDER (O1).
//
// The values are held unexported and no method hands them out. That is the
// point: Position is the only way to learn anything about rank (I3), so there
// is no path by which a caller could obtain two indices and compare them
// itself — which would be the same guess Position exists to refuse.
type Index struct {
	order map[string][]string
}

// Position reports a value's index in the field's declared order.
//
// The second result is false when the field declares no order AND when the
// value is not in the order it declares (I4). Those are one answer on purpose:
// both mean "core cannot rank this", and a caller that could tell them apart
// would be tempted to handle the second by sorting the value to an end.
func (ix Index) Position(field, value string) (int, bool) {
	values, ok := ix.order[field]
	if !ok {
		return 0, false
	}
	for i, v := range values {
		if v == value {
			return i, true
		}
	}
	return 0, false
}

// Ordered reports whether the field declares an order at all.
//
// It exists for T3's reason string, which must say WHICH of the three
// unorderable cases it hit (§5 T3), and for `schema list`'s `ordered_fields`.
// It reports the existence of an order, never a position.
func (ix Index) Ordered(field string) bool {
	_, ok := ix.order[field]
	return ok
}

// Fields lists the ordered fields by NAME, sorted. Names only: an index's
// values never leave this package.
func (ix Index) Fields() []string {
	out := make([]string, 0, len(ix.order))
	for name := range ix.order {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Len is how many fields declare an order.
func (ix Index) Len() int { return len(ix.order) }

// MarshalJSON renders the index as the object stored in `schemas.ordered`.
// encoding/json sorts map keys, so the stored text is canonical and I2's
// round-trip comparison can be a byte comparison.
func (ix Index) MarshalJSON() ([]byte, error) {
	if ix.order == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(ix.order)
}

// UnmarshalJSON reads a stored index back.
func (ix *Index) UnmarshalJSON(data []byte) error {
	var order map[string][]string
	if err := json.Unmarshal(data, &order); err != nil {
		return err
	}
	ix.order = order
	return nil
}

// DeriveIndex is §4.3's derivation as a standalone function: schema bytes in,
// ordered index out, nothing else consulted.
//
// It is exported because I2 makes the stored copy a CACHE — a QA check
// re-derives from `schemas.body` and compares against `schemas.ordered` for
// every registered row, and a derivation that could only be reached through a
// full Compile would make that check depend on the validator it is meant to be
// independent of.
func DeriveIndex(body []byte) (Index, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return Index{}, &Error{Message: fmt.Sprintf("not valid JSON: %v", err)}
	}
	if err := checkAnnotations(doc); err != nil {
		return Index{}, err
	}
	fields, names := deriveFields(doc)
	return indexFromFields(fields, names), nil
}

// indexFromFields keeps the index and the field table derived from ONE walk, so
// a field can never be ordered in one and not the other.
func indexFromFields(fields map[string]Field, names []string) Index {
	order := make(map[string][]string)
	for _, name := range names {
		f := fields[name]
		if f.Ordered {
			order[name] = f.Enum
		}
	}
	return Index{order: order}
}

// deriveFields extracts the top-level properties of the ARRAY'S ITEM SCHEMA.
//
// O5: only those are indexed. §11.2's grammar is `agg(field op literal)` with a
// bare field token, so a nested ordered enum is unreachable by any predicate,
// and indexing it would build a map nothing can query.
//
// A document that is not an array of objects yields no fields. That is not an
// error: such a schema is registrable and validates payloads perfectly well; it
// simply declares nothing a threshold predicate can name.
func deriveFields(doc any) (map[string]Field, []string) {
	fields := make(map[string]Field)
	var names []string

	root, ok := doc.(map[string]any)
	if !ok {
		return fields, names
	}
	items, ok := root["items"].(map[string]any)
	if !ok {
		return fields, names
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		return fields, names
	}

	// Map iteration order is random, and `names` is emitted in `schema show`
	// and compared by tests, so the field list is sorted rather than
	// accidental. The ENUM's order is the author's document order and is
	// untouched by this — that is the order that carries meaning (O1).
	names = make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		sub, ok := props[name].(map[string]any)
		if !ok {
			fields[name] = Field{Name: name}
			continue
		}
		f := Field{Name: name}
		if t, ok := sub["type"].(string); ok {
			f.Type = t
		}
		f.Enum = stringEnum(sub["enum"])
		if ordered, ok := sub[annotation].(bool); ok && ordered && len(f.Enum) > 0 {
			f.Ordered = true
		}
		// The direction rides on the ORDER, so it is only derived for a field
		// that has one. checkAnnotations has already refused a direction
		// declared without one (O6), so this is not where that mistake is
		// caught — it is where a well-formed declaration is read.
		if end, ok := sub[conservativeAnnotation].(string); ok && f.Ordered {
			f.Conservative = end
		}
		fields[name] = f
	}
	return fields, names
}

// stringEnum returns a declared `enum` as strings, or nil when it is absent or
// not entirely strings. An enum of numbers is a perfectly good enum; it just
// cannot carry an ORDER, since the annotation's contract is a declared sequence
// of the values a threshold literal is written as.
func stringEnum(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// checkAnnotations is O2, applied to the WHOLE document rather than only to the
// indexed properties.
//
// A nested `ordered_enum` is not indexed (O5), but a malformed one is still an
// authoring mistake, and reporting it is free. The alternative — silently
// ignoring an annotation that will never do anything — is how an author comes
// to believe an order exists that does not.
func checkAnnotations(doc any) error {
	var walk func(node any, path []string) error
	walk = func(node any, path []string) error {
		switch n := node.(type) {
		case map[string]any:
			if raw, present := n[annotation]; present {
				if err := checkOneAnnotation(raw, n, path); err != nil {
					return err
				}
			}
			if raw, present := n[conservativeAnnotation]; present {
				if err := checkConservativeAnnotation(raw, n, path); err != nil {
					return err
				}
			}
			// Sorted, so an author fixing several errors sees them in the same
			// sequence on every run.
			keys := make([]string, 0, len(n))
			for k := range n {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if err := walk(n[k], extend(path, k)); err != nil {
					return err
				}
			}
		case []any:
			for i, item := range n {
				if err := walk(item, extend(path, strconv.Itoa(i))); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(doc, nil)
}

// extend appends a token to a path over a FRESH backing array. Reusing the
// caller's would let one branch of the walk overwrite the path a sibling branch
// is about to report, which is a bug that only shows up in the error message —
// the worst place to find one.
func extend(path []string, tok string) []string {
	out := make([]string, len(path), len(path)+1)
	copy(out, path)
	return append(out, tok)
}

// checkOneAnnotation is O1 and O2 for a single subschema.
func checkOneAnnotation(raw any, sub map[string]any, path []string) error {
	where := renderPointer("", path)

	flag, ok := raw.(bool)
	if !ok {
		return &Error{Rule: "O1", Path: where, Message: fmt.Sprintf(
			"`%s` must be a boolean; it declares position, and the order itself "+
				"is the sibling `enum` array's document order", annotation)}
	}
	if !flag {
		// `"ordered_enum": false` is the default said out loud. Nothing to check
		// and nothing to index.
		return nil
	}

	enum, present := sub["enum"]
	if !present {
		return &Error{Rule: "O2", Path: where, Message: fmt.Sprintf(
			"`%s` is true but the subschema declares no `enum`; the order IS the "+
				"`enum` array, ascending, and there is no second list", annotation)}
	}
	values := stringEnum(enum)
	if values == nil {
		return &Error{Rule: "O2", Path: where, Message: fmt.Sprintf(
			"`%s` requires `enum` to be an array of strings", annotation)}
	}
	if len(values) < 2 {
		return &Error{Rule: "O2", Path: where, Message: fmt.Sprintf(
			"`%s` requires at least 2 `enum` values, got %d; a one-value order "+
				"orders nothing", annotation, len(values))}
	}
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		if _, dup := seen[v]; dup {
			return &Error{Rule: "O2", Path: where, Message: fmt.Sprintf(
				"`%s` requires unique `enum` values; %q appears twice, so its "+
					"position would be ambiguous", annotation, v)}
		}
		seen[v] = struct{}{}
	}
	return nil
}

// renderPointer renders a location's path tokens in this repo's notation:
// numeric tokens as `[3]`, names as `.name`, under an optional root label.
//
// One renderer for both the schema document (`items.properties.severity`) and a
// payload instance (`payload[3].severity`), so the two notations cannot drift.
func renderPointer(root string, tokens []string) string {
	var b strings.Builder
	b.WriteString(root)
	for _, tok := range tokens {
		if isIndex(tok) {
			fmt.Fprintf(&b, "[%s]", tok)
			continue
		}
		b.WriteByte('.')
		b.WriteString(tok)
	}
	out := b.String()
	if root == "" {
		return strings.TrimPrefix(out, ".")
	}
	return out
}

func isIndex(tok string) bool {
	if tok == "" {
		return false
	}
	for _, r := range tok {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// displayValue renders a JSON scalar the way an author wrote it, for a refusal
// that quotes both what was given and what is accepted.
func displayValue(v any) string {
	switch t := v.(type) {
	case string:
		return strconv.Quote(t)
	case nil:
		return "null"
	default:
		encoded, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(encoded)
	}
}

// checkConservativeAnnotation is O6: a `conservative_end` must be one of the two
// declared ends, and must sit on a subschema that actually declares an order.
//
// The second half is the one that matters. A direction on an unordered field is
// not harmless-but-ignored: it is an author who believes they have said which
// end is worse, on a field where no end exists, and every tie will resolve the
// way they did not ask for with nothing anywhere to say why. That is the same
// failure mode O2 exists to prevent for the order itself.
func checkConservativeAnnotation(raw any, sub map[string]any, path []string) error {
	where := renderPointer("", path)

	end, ok := raw.(string)
	if !ok {
		return &Error{Rule: "O6", Path: where, Message: fmt.Sprintf(
			"`%s` must be a string naming one of %s; it names an END of the "+
				"sibling `enum`'s order, got %s", conservativeAnnotation,
			strings.Join(ConservativeEnds, " or "), displayValue(raw))}
	}
	if end != ConservativeUpper && end != ConservativeLower {
		return &Error{Rule: "O6", Path: where, Message: fmt.Sprintf(
			"`%s` must be %s, got %q; the ends are named by POSITION in the "+
				"ascending `enum`, not by the values the enum holds",
			conservativeAnnotation, strings.Join(ConservativeEnds, " or "), end)}
	}

	ordered, _ := sub[annotation].(bool)
	if !ordered {
		return &Error{Rule: "O6", Path: where, Message: fmt.Sprintf(
			"`%s` is declared but `%s` is not true; a direction names an end of "+
				"an order, and this subschema declares no order for it to name",
			conservativeAnnotation, annotation)}
	}
	return nil
}
