// Package schema is payload validation plus the `ordered_enum` annotation and
// the derived ordered-field index (docs/tdd/payloads-thresholds.md §4.2, §4.3).
//
// THIS IS THE PACKAGE WHERE CORE LEARNS ORDER WITHOUT MEANING. It knows that
// `enum[i]` precedes `enum[i+1]` because a user's registered document said so.
// It does not know which end is "worse", "higher", or "better", it holds no
// table of known fields, and it has no default order and no fallback ordering:
// a field with no declared order is not orderable, and Position says so rather
// than guessing (§4.3 I3, I4).
//
// It is PURE — no database, no engine, no filesystem beyond the one embedded
// document (§7.6). Every decision is a function of the registered bytes.
package schema

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// resourceURL is the compiler resource name a registered document is compiled
// under. It is a constant rather than the schema's `name@version` because a
// document may declare its own `$id`, and two registrations that differed only
// in the URL we invented would compile differently for no reason a user could
// see.
const resourceURL = "docket:///payload-schema"

// pinnedDraft is the default draft a document with no `$schema` key is read
// under (§4.1 mitigation 2).
//
// It is set EXPLICITLY rather than left to the library's own default, because
// a library default that changed in a patch release would re-interpret every
// already-registered document that omits `$schema` — a validation verdict
// changing under unchanged bytes, which is the one thing a pinned registry may
// not do. TestDefaultDraftIsPinned asserts the interpretation, not the setting.
var pinnedDraft = jsonschema.Draft2020

// Error is a refusal about a schema DOCUMENT — the O1-O5 annotation rules and
// the library's own compile failures. It carries the property path so an author
// is told where, not just that.
//
// It maps to VALIDATION_ERROR (exit 3) at `docket schema register`.
type Error struct {
	// Rule is the §4.2 clause, when one decided it ("O1".."O5"), or empty for a
	// compile failure the library reported.
	Rule string
	// Path is the property path within the schema document, rendered as
	// `items.properties.severity`.
	Path    string
	Message string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Message)
}

// Field is one top-level property of the item schema — what §11.2's grammar can
// name with its bare field token (§4.2 O5).
//
// The values are the AUTHOR'S. Nothing here interprets them: `Type` is the
// declared JSON type so a literal can be checked as parseable, `Enum` is the
// declared membership set so a literal can be checked as a member, and
// `Ordered` reports only that the author declared an order — never what the
// order means.
type Field struct {
	Name string
	// Type is the declared `type`, or "" when the property declares none.
	Type string
	// Enum is the declared `enum`, in document order, or nil when there is none.
	Enum []string
	// Ordered reports `"ordered_enum": true`.
	Ordered bool
	// Conservative is the declared `conservative_end` — ConservativeUpper,
	// ConservativeLower, or "" when the author declared no direction.
	//
	// It is the ONE place a declared order says something about MEANING rather
	// than about sequence, and it says the minimum: which end the author would
	// rather a tie fall toward. Core reads it in exactly one decision (an
	// even-count median tie) and nowhere else, so a direction can never turn
	// into a general opinion about what these values are.
	Conservative string
}

// Registered is a compiled schema document: the bytes as registered, the
// derived ordered index, the declared fields, and the compiled validator.
//
// The bytes are retained because they are what a run pins (§4.7 P4) and what a
// refusal quotes. Everything else is derived from them, once (§4.3 I1).
type Registered struct {
	Name    string
	Version int
	// Body is the registered document, VERBATIM. Idempotency and pinning are
	// decided on these bytes, so a re-serialization would be a different
	// registration (§4.4).
	Body []byte
	// Ordered is the §4.3 derived index. It is a cache of a pure function of
	// Body, never a second source of truth (I2).
	Ordered Index

	fields   map[string]Field
	names    []string
	compiled *jsonschema.Schema
}

// Compile parses a schema document, checks the §4.2 annotation rules, compiles
// it as ordinary JSON Schema, and derives the ordered index.
//
// The library validates the document as ORDINARY JSON SCHEMA: `ordered_enum` is
// not registered as a custom keyword, so the validator never sees it (O4).
// Unknown keywords are annotations by specification and are ignored, which is
// what makes the annotation additive — a document valid under the
// schema-minus-annotation is valid under it (O3) — and what makes the library
// replaceable: ordering is ours, validation is theirs.
func Compile(name string, version int, body []byte) (*Registered, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("not valid JSON: %v", err)}
	}

	// The annotation rules run BEFORE the compile, so an O2 violation is
	// reported as the annotation error it is rather than surviving as a
	// silently-ignored unknown keyword.
	if err := checkAnnotations(doc); err != nil {
		return nil, err
	}

	c := jsonschema.NewCompiler()
	c.DefaultDraft(pinnedDraft)
	if err := c.AddResource(resourceURL, doc); err != nil {
		return nil, &Error{Message: fmt.Sprintf("not a usable schema document: %v", err)}
	}
	compiled, err := c.Compile(resourceURL)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("schema does not compile: %v", err)}
	}

	fields, names := deriveFields(doc)
	return &Registered{
		Name:     name,
		Version:  version,
		Body:     body,
		Ordered:  indexFromFields(fields, names),
		fields:   fields,
		names:    names,
		compiled: compiled,
	}, nil
}

// Ref renders the `name@version` identity a run pins.
func (r *Registered) Ref() string {
	return fmt.Sprintf("%s@%d", r.Name, r.Version)
}

// Position is THE ONLY ordering API (§4.3 I3). It reports a value's index in
// the field's declared order, and whether the field is ordered at all.
//
// There is deliberately no Less(a, b): a comparison of two values of a field
// with no declared order is UNREPRESENTABLE rather than merely discouraged,
// which is what makes engine-spine §6.14's T3 a property of the type instead of
// a rule a caller has to remember.
func (r *Registered) Position(field, value string) (int, bool) {
	return r.Ordered.Position(field, value)
}

// Field returns one declared top-level property of the item schema.
func (r *Registered) Field(name string) (Field, bool) {
	f, ok := r.fields[name]
	return f, ok
}

// ValidateMember reports whether `value` is a declared member of `field`'s
// enum, as an error naming the declared set when it is not.
//
// It lives HERE, beside the values, so a caller never reads them: the engine's
// guard (TestNoComparisonAPIBypassesPosition) forbids reaching into
// `Field.Enum` from internal/engine, and membership is the one question a
// value-correcting verb needs answered. Membership is not ordering — Position
// stays the only ranking API — but the values themselves still never leave
// this package except inside a refusal a person reads.
func (r *Registered) ValidateMember(field, value string) error {
	f, ok := r.fields[field]
	if !ok || len(f.Enum) == 0 {
		return fmt.Errorf(
			"schema %s declares no enum for %q; there is no declared "+
				"membership set to choose a value from", r.Ref(), field)
	}
	if !slices.Contains(f.Enum, value) {
		return fmt.Errorf(
			"%q is not a value %s's %q accepts; declared values: %s",
			value, r.Ref(), field, strings.Join(f.Enum, ", "))
	}
	return nil
}

// FieldNames lists the declared top-level properties, in document order.
func (r *Registered) FieldNames() []string {
	out := make([]string, len(r.names))
	copy(out, r.names)
	return out
}

// PayloadError is the §4.8 C2/C3 refusal: path-precise lines, capped.
type PayloadError struct {
	// Ref is the `name@version` the payload was validated against.
	Ref string
	// Lines are the rendered failures, at most maxPayloadErrors of them.
	Lines []string
	// More is how many further failures were dropped by the cap.
	More int
}

// maxPayloadErrors is C3's cap. A worker's log is not improved by a hundred
// lines, and the count is reported so the operator knows the list is partial.
const maxPayloadErrors = 5

func (e *PayloadError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "payload does not satisfy %s:", e.Ref)
	for _, line := range e.Lines {
		b.WriteString("\n  ")
		b.WriteString(line)
	}
	if e.More > 0 {
		fmt.Fprintf(&b, "\n  (+%d more)", e.More)
	}
	return b.String()
}

// ValidatePayload validates payload bytes against the registered document.
//
// It returns a *PayloadError whose lines are rendered from the validator's
// INSTANCE LOCATION, so a refusal says `payload[3].severity: …` rather than
// "the payload is invalid" — the difference between something a worker can fix
// and something it can only re-submit blindly.
func (r *Registered) ValidatePayload(raw []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return &PayloadError{Ref: r.Ref(), Lines: []string{
			fmt.Sprintf("payload is not valid JSON: %v", err)}}
	}

	err = r.compiled.Validate(inst)
	if err == nil {
		return nil
	}

	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return &PayloadError{Ref: r.Ref(), Lines: []string{err.Error()}}
	}

	lines := renderFailures(verr)
	out := &PayloadError{Ref: r.Ref()}
	if len(lines) > maxPayloadErrors {
		out.Lines = lines[:maxPayloadErrors]
		out.More = len(lines) - maxPayloadErrors
	} else {
		out.Lines = lines
	}
	return out
}

// renderFailures flattens the validator's error tree to its LEAVES — the nodes
// that name an actual instance location — and renders one line each.
//
// Interior nodes restate their children ("'properties' failed"), which is noise
// in a worker's log; the leaves are the sentences an author can act on.
func renderFailures(verr *jsonschema.ValidationError) []string {
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, fmt.Sprintf("%s: %s",
				renderPointer("payload", e.InstanceLocation), describeFailure(e)))
			return
		}
		for _, cause := range e.Causes {
			walk(cause)
		}
	}
	walk(verr)
	return out
}

// describeFailure renders one leaf failure.
//
// The `enum` case is rendered here rather than deferred to the library so the
// message names the value that was REJECTED as well as the values that are
// accepted — an author reading "value must be one of …" still has to go find
// what they wrote.
func describeFailure(e *jsonschema.ValidationError) string {
	if enumErr, ok := e.ErrorKind.(*kind.Enum); ok {
		want := make([]string, 0, len(enumErr.Want))
		for _, v := range enumErr.Want {
			want = append(want, displayValue(v))
		}
		return fmt.Sprintf("value %s is not one of [%s]",
			displayValue(enumErr.Got), strings.Join(want, ","))
	}
	return trimInstancePrefix(e.Error())
}

// trimInstancePrefix drops the library's own `at '<pointer>': ` prefix, since
// the pointer is already rendered — in this repo's notation — by the caller.
func trimInstancePrefix(s string) string {
	if !strings.HasPrefix(s, "at ") {
		return s
	}
	if i := strings.Index(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}
