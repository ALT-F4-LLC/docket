package schema

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// arraySchema wraps item properties in the §4.2 shape, so a table row states
// only the property under test.
func arraySchema(properties string) []byte {
	return []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "array",
  "items": {"type": "object", "properties": {` + properties + `}}
}`)
}

// TestOrderedEnumAnnotation is O1-O5: what the annotation accepts, what it
// refuses, and where the refusal points.
func TestOrderedEnumAnnotation(t *testing.T) {
	cases := []struct {
		name string
		// clause is the §4.2 row the case exercises.
		clause     string
		properties string
		// wantErr is a substring of the refusal, or "" when the document is
		// legal.
		wantErr string
		// wantPath is the property path the refusal must name.
		wantPath string
		// wantOrdered lists the fields the derived index holds.
		wantOrdered []string
	}{
		{
			name: "five values in document order", clause: "O1",
			properties:  `"risk": {"type": "string", "enum": ["info","low","medium","high","blocker"], "ordered_enum": true}`,
			wantOrdered: []string{"risk"},
		},
		{
			name: "two values is the minimum order", clause: "O1",
			properties:  `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": true}`,
			wantOrdered: []string{"risk"},
		},
		{
			name: "annotation with no sibling enum", clause: "O2",
			properties: `"risk": {"type": "string", "ordered_enum": true}`,
			wantErr:    "declares no `enum`",
			wantPath:   "items.properties.risk",
		},
		{
			name: "enum is not an array", clause: "O2",
			properties: `"risk": {"type": "string", "enum": "low", "ordered_enum": true}`,
			wantErr:    "requires `enum` to be an array of strings",
			wantPath:   "items.properties.risk",
		},
		{
			name: "enum holds a non-string", clause: "O2",
			properties: `"risk": {"enum": ["low", 2], "ordered_enum": true}`,
			wantErr:    "requires `enum` to be an array of strings",
			wantPath:   "items.properties.risk",
		},
		{
			name: "enum has one value", clause: "O2",
			properties: `"risk": {"type": "string", "enum": ["low"], "ordered_enum": true}`,
			wantErr:    "at least 2 `enum` values",
			wantPath:   "items.properties.risk",
		},
		{
			name: "enum repeats a value", clause: "O2",
			properties: `"risk": {"type": "string", "enum": ["low","high","low"], "ordered_enum": true}`,
			wantErr:    "unique `enum` values",
			wantPath:   "items.properties.risk",
		},
		{
			name: "annotation is not a boolean", clause: "O1",
			properties: `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": "yes"}`,
			wantErr:    "must be a boolean",
			wantPath:   "items.properties.risk",
		},
		{
			name: "an explicit false declares nothing", clause: "O1",
			properties:  `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": false}`,
			wantOrdered: nil,
		},
		{
			name: "an enum with no annotation is not ordered", clause: "O3",
			properties:  `"risk": {"type": "string", "enum": ["low","high"]}`,
			wantOrdered: nil,
		},
		{
			name: "a nested ordered enum is legal but not indexed", clause: "O5",
			properties: `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": true},
			             "detail": {"type": "object", "properties": {
			                "tier": {"type": "string", "enum": ["one","two"], "ordered_enum": true}}}`,
			wantOrdered: []string{"risk"},
		},
		{
			name: "a malformed nested annotation is still refused", clause: "O2",
			properties: `"detail": {"type": "object", "properties": {
			                "tier": {"type": "string", "ordered_enum": true}}}`,
			wantErr:  "declares no `enum`",
			wantPath: "items.properties.detail.properties.tier",
		},
	}

	for _, tc := range cases {
		t.Run(tc.clause+" "+tc.name, func(t *testing.T) {
			reg, err := Compile("case", 1, arraySchema(tc.properties))

			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("expected a refusal, the document compiled")
				}
				serr, ok := err.(*Error)
				if !ok {
					t.Fatalf("expected a *schema.Error, got %T: %v", err, err)
				}
				if !strings.Contains(serr.Message, tc.wantErr) {
					t.Errorf("refusal message = %q, want it to contain %q", serr.Message, tc.wantErr)
				}
				if serr.Path != tc.wantPath {
					t.Errorf("refusal path = %q, want %q", serr.Path, tc.wantPath)
				}
				return
			}

			testsupport.Must(t, err, "expected the document to compile, got: %v", err)
			got := reg.Ordered.Fields()
			if len(got) != len(tc.wantOrdered) {
				t.Fatalf("ordered fields = %v, want %v", got, tc.wantOrdered)
			}
			for i, want := range tc.wantOrdered {
				if got[i] != want {
					t.Fatalf("ordered fields = %v, want %v", got, tc.wantOrdered)
				}
			}
		})
	}
}

// TestOrderIsTheEnumArraysDocumentOrder is O1's single-list rule: there is no
// second list to disagree with, and the order is the array as written —
// NOT sorted, which is the mistake that would silently reverse an author's
// intent for a descending enum.
func TestOrderIsTheEnumArraysDocumentOrder(t *testing.T) {
	reg, err := Compile("case", 1, arraySchema(
		`"risk": {"type": "string", "enum": ["blocker","high","medium","low","info"], "ordered_enum": true}`))
	testsupport.Must(t, err, "compiling: %v", err)

	for want, value := range []string{"blocker", "high", "medium", "low", "info"} {
		got, ok := reg.Position("risk", value)
		if !ok {
			t.Fatalf("Position(risk, %q) reports the field unordered", value)
		}
		if got != want {
			t.Errorf("Position(risk, %q) = %d, want %d — the order is the array as written", value, got, want)
		}
	}
}

// TestPosition is I1-I4: the only ordering API, and every way it can decline.
func TestPosition(t *testing.T) {
	reg, err := Compile("case", 1, arraySchema(
		`"risk": {"type": "string", "enum": ["low","medium","high"], "ordered_enum": true},
		 "tier": {"type": "string", "enum": ["one","two"]},
		 "count": {"type": "integer"}`))
	testsupport.Must(t, err, "compiling: %v", err)

	cases := []struct {
		name, clause, field, value string
		wantPos                    int
		wantOK                     bool
	}{
		{name: "an ordered field", clause: "I1", field: "risk", value: "medium", wantPos: 1, wantOK: true},
		{name: "the first value", clause: "I1", field: "risk", value: "low", wantPos: 0, wantOK: true},
		{name: "a field with an enum but no order", clause: "I3", field: "tier", value: "one"},
		{name: "a field with no enum at all", clause: "I3", field: "count", value: "3"},
		{name: "a field the schema does not declare", clause: "I3", field: "absent", value: "low"},
		{name: "a value outside the declared order", clause: "I4", field: "risk", value: "urgent"},
	}

	for _, tc := range cases {
		t.Run(tc.clause+" "+tc.name, func(t *testing.T) {
			pos, ok := reg.Position(tc.field, tc.value)
			if ok != tc.wantOK {
				t.Fatalf("Position(%q, %q) ok = %v, want %v", tc.field, tc.value, ok, tc.wantOK)
			}
			if !tc.wantOK {
				// I4, stated as an assertion: an unorderable value is not
				// sorted to an end and not treated as smallest. The int result
				// is meaningless when ok is false, and a caller that read it
				// anyway would be inventing exactly the fallback this clause
				// exists to refuse — so it is zero, and only the flag decides.
				if pos != 0 {
					t.Errorf("Position returned %d alongside ok=false; an unorderable value has no position", pos)
				}
				return
			}
			if pos != tc.wantPos {
				t.Errorf("Position(%q, %q) = %d, want %d", tc.field, tc.value, pos, tc.wantPos)
			}
		})
	}
}

// TestDeclaredFieldsAreTheItemSchemasTopLevelProperties is O5 from the other
// side: the field table a threshold predicate is validated against holds
// exactly what §11.2's bare field token can name.
func TestDeclaredFieldsAreTheItemSchemasTopLevelProperties(t *testing.T) {
	reg, err := Compile("case", 1, arraySchema(
		`"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": true},
		 "count": {"type": "integer"},
		 "detail": {"type": "object", "properties": {"tier": {"type": "string"}}}`))
	testsupport.Must(t, err, "compiling: %v", err)

	want := []string{"count", "detail", "risk"}
	got := reg.FieldNames()
	if len(got) != len(want) {
		t.Fatalf("FieldNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FieldNames() = %v, want %v", got, want)
		}
	}

	risk, ok := reg.Field("risk")
	if !ok {
		t.Fatal("`risk` is not a declared field")
	}
	if risk.Type != "string" || !risk.Ordered || len(risk.Enum) != 2 {
		t.Errorf("risk = %+v, want a string enum declaring an order", risk)
	}

	count, ok := reg.Field("count")
	if !ok {
		t.Fatal("`count` is not a declared field")
	}
	if count.Type != "integer" || count.Ordered || count.Enum != nil {
		t.Errorf("count = %+v, want an unordered integer with no enum", count)
	}

	// The nested property is NOT a field: no predicate can name it, so
	// indexing it would build a map nothing can query.
	if _, ok := reg.Field("tier"); ok {
		t.Error("`tier` is nested and must not appear as a declared field (O5)")
	}
}

// TestASchemaWithNoItemsDeclaresNoFields records the case that is legal and
// empty rather than an error: a document that is not an array of objects
// registers fine, validates payloads fine, and simply declares nothing a
// threshold can name.
func TestASchemaWithNoItemsDeclaresNoFields(t *testing.T) {
	reg, err := Compile("case", 1, []byte(`{"type": "object"}`))
	testsupport.Must(t, err, "compiling: %v", err)
	if len(reg.FieldNames()) != 0 {
		t.Errorf("FieldNames() = %v, want none", reg.FieldNames())
	}
	if reg.Ordered.Len() != 0 {
		t.Errorf("ordered index holds %d fields, want none", reg.Ordered.Len())
	}
}

// TestConservativeEndAnnotation is O6: what a declared direction accepts, what
// it refuses, and what it derives (DKT-267).
//
// The direction is the one thing a declared order says about MEANING rather
// than sequence, so its rules are the same shape as `ordered_enum`'s: a closed
// vocabulary, a refusal that names what was given, and a hard requirement that
// it sit on a subschema that actually declares an order.
func TestConservativeEndAnnotation(t *testing.T) {
	cases := []struct {
		name       string
		properties string
		// wantErr is a substring of the refusal, or "" when the document is
		// legal.
		wantErr  string
		wantPath string
		// wantEnd is the derived direction for `risk` on a legal document.
		wantEnd string
	}{
		{
			name:       "upper is derived",
			properties: `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": true, "conservative_end": "upper"}`,
			wantEnd:    ConservativeUpper,
		},
		{
			name:       "lower is derived",
			properties: `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": true, "conservative_end": "lower"}`,
			wantEnd:    ConservativeLower,
		},
		{
			name:       "declaring nothing derives nothing",
			properties: `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": true}`,
			wantEnd:    "",
		},
		{
			name:       "a value outside the vocabulary is refused",
			properties: `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": true, "conservative_end": "high"}`,
			wantErr:    "named by POSITION",
			wantPath:   "items.properties.risk",
		},
		{
			name:       "a non-string direction is refused",
			properties: `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": true, "conservative_end": true}`,
			wantErr:    "must be a string naming one of",
			wantPath:   "items.properties.risk",
		},
		{
			name:       "a direction over no order is refused",
			properties: `"risk": {"type": "string", "enum": ["low","high"], "conservative_end": "upper"}`,
			wantErr:    "is not true",
			wantPath:   "items.properties.risk",
		},
		{
			name:       "a direction over an explicit false is refused",
			properties: `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": false, "conservative_end": "upper"}`,
			wantErr:    "is not true",
			wantPath:   "items.properties.risk",
		},
		{
			name: "a malformed nested direction is refused too",
			properties: `"detail": {"type": "object", "properties": {
			                "tier": {"type": "string", "enum": ["one","two"], "ordered_enum": true, "conservative_end": "sideways"}}}`,
			wantErr:  "named by POSITION",
			wantPath: "items.properties.detail.properties.tier",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := Compile("case", 1, arraySchema(tc.properties))

			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("expected a refusal, the document compiled")
				}
				serr, ok := err.(*Error)
				if !ok {
					t.Fatalf("expected a *schema.Error, got %T: %v", err, err)
				}
				if !strings.Contains(serr.Message, tc.wantErr) {
					t.Errorf("refusal message = %q, want it to contain %q",
						serr.Message, tc.wantErr)
				}
				if serr.Path != tc.wantPath {
					t.Errorf("refusal path = %q, want %q", serr.Path, tc.wantPath)
				}
				return
			}

			testsupport.Must(t, err, "expected the document to compile, got: %v", err)
			risk, ok := reg.Field("risk")
			if !ok {
				t.Fatal("`risk` is not a declared field")
			}
			if risk.Conservative != tc.wantEnd {
				t.Errorf("derived conservative_end = %q, want %q",
					risk.Conservative, tc.wantEnd)
			}
		})
	}
}

// TestConservativeEndDoesNotChangeTheStoredIndex is the compatibility clause.
//
// `schemas.ordered` is a CACHE of the ordered index (I2) and its stored shape is
// compared byte-for-byte by the corpus round-trip. A direction is derived onto
// the FIELD TABLE and never onto the index, so adding one to a document leaves
// every stored row and every re-derivation identical — which is why DKT-267
// needed no schema version and no migration.
func TestConservativeEndDoesNotChangeTheStoredIndex(t *testing.T) {
	const props = `"risk": {"type": "string", "enum": ["low","high"], "ordered_enum": true%s}`

	plain, err := Compile("case", 1, arraySchema(fmt.Sprintf(props, "")))
	testsupport.Must(t, err, "compiling the undirected document: %v", err)
	directed, err := Compile("case", 1, arraySchema(
		fmt.Sprintf(props, `, "conservative_end": "upper"`)))
	testsupport.Must(t, err, "compiling the directed document: %v", err)

	plainIndex, err := json.Marshal(plain.Ordered)
	testsupport.Must(t, err, "marshaling: %v", err)
	directedIndex, err := json.Marshal(directed.Ordered)
	testsupport.Must(t, err, "marshaling: %v", err)

	if string(plainIndex) != string(directedIndex) {
		t.Errorf("the stored ordered index changed when a direction was declared:\n"+
			"  without: %s\n  with:    %s\n"+
			"I2 makes the stored index a cache of the ORDER; a direction is not "+
			"part of the order and must not enter it.", plainIndex, directedIndex)
	}
}
