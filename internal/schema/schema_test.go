package schema

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestDefaultDraftIsPinned is §4.1 mitigation 2.
//
// A document with NO `$schema` key is read under the draft this package pins,
// not under whatever the library's default happens to be this release. The
// keyword chosen is one whose meaning DIFFERS across drafts:
// `dependentRequired` asserts from 2019-09 onward and is an unknown keyword —
// therefore an ignored annotation — under draft-07 and earlier.
//
// So the assertion is about INTERPRETATION rather than about a setting: if the
// library's default ever slid back to draft-07, the document below would
// validate and this test would fail, which is exactly when a maintainer needs
// to hear about it.
func TestDefaultDraftIsPinned(t *testing.T) {
	const noDraftDeclared = `{
  "type": "object",
  "properties": {"risk": {"type": "string"}, "tier": {"type": "string"}},
  "dependentRequired": {"risk": ["tier"]}
}`

	reg, err := Compile("draft", 1, []byte(noDraftDeclared))
	testsupport.Must(t, err, "compiling a document with no $schema: %v", err)

	if err := reg.ValidatePayload([]byte(`{"risk": "low"}`)); err == nil {
		t.Fatal("`dependentRequired` did not assert: the document was read under a " +
			"draft older than 2020-12, so an unchanged schema now means something else")
	}

	// The satisfied case still passes, so the test fails for the right reason
	// rather than because everything is refused.
	if err := reg.ValidatePayload([]byte(`{"risk": "low", "tier": "one"}`)); err != nil {
		t.Fatalf("the satisfied document was refused: %v", err)
	}
}

// TestEmbeddedAggregateSchemaMatchesItsGolden is §7.6 E4.
//
// `aggregate@1` is seeded into every database at v9 and pinned by every run
// that binds an `aggregate` step. Editing the embedded bytes therefore does not
// change the schema those databases hold — it creates a divergence the seed's
// INSERT OR IGNORE will not correct (U7), and a database must never become
// unopenable because a binary changed.
//
// So the divergence is caught HERE, at build time: an edit is a deliberate act
// with a failing test attached, and the remedy is `aggregate@2` plus the code
// that consumes it.
func TestEmbeddedAggregateSchemaMatchesItsGolden(t *testing.T) {
	const golden = "1e0a0be3939417d203bef7e26a18f36f9dbaad6ed8b9e7b583a0d86fac8358b1"

	if got := AggregateSHA256(); got != golden {
		t.Fatalf("the embedded %s document changed.\n"+
			"  golden: %s\n"+
			"  got:    %s\n"+
			"A registered name@version is immutable (§4.4): every database seeded "+
			"by an earlier build still holds the old bytes, and the seed will not "+
			"overwrite them. Ship aggregate@2 instead, or restore the document.",
			AggregateRef(), golden, got)
	}
}

// TestTheShippedAggregateSchemaIsUsable proves E1 and E5's half that group 1
// can prove: the document compiles, requires what §7.6 says it requires, and
// admits the carried-through keys G3 depends on.
func TestTheShippedAggregateSchemaIsUsable(t *testing.T) {
	reg, err := Aggregate()
	testsupport.Must(t, err, "the shipped %s document does not compile: %v", AggregateRef(), err)
	if reg.Ref() != "aggregate@1" {
		t.Errorf("Ref() = %q, want aggregate@1", reg.Ref())
	}

	// The author's own field key rides through, alongside G3's other keys.
	valid := `[{"risk": "medium", "members": ["low","medium","high"], "held": true,
	            "demoted_from": "high", "operator_resolved": false, "ticket": "AB-12"}]`
	if err := reg.ValidatePayload([]byte(valid)); err != nil {
		t.Errorf("an aggregate-shaped element was refused: %v", err)
	}

	// `demoted_from` and `operator_resolved` are optional.
	if err := reg.ValidatePayload([]byte(`[{"risk": "low", "members": ["low"], "held": false}]`)); err != nil {
		t.Errorf("an element with no demotion was refused: %v", err)
	}

	for _, bad := range []struct{ why, doc string }{
		{"members is required", `[{"held": false}]`},
		{"held is required", `[{"members": ["low"]}]`},
		{"members may not be empty", `[{"members": [], "held": false}]`},
		{"held is a boolean", `[{"members": ["low"], "held": "no"}]`},
	} {
		if err := reg.ValidatePayload([]byte(bad.doc)); err == nil {
			t.Errorf("%s: the document was accepted", bad.why)
		}
	}
}

// TestTheShippedAggregateSchemaNamesNoInstanceToken is E2, as an assertion
// rather than as a claim. The genericity gate scans the file (§1.1.1); this
// makes the same point where a reader of the package will see it.
func TestTheShippedAggregateSchemaNamesNoInstanceToken(t *testing.T) {
	body := strings.ToLower(string(AggregateBody()))
	// The keys the document may name are exactly §2's, plus JSON Schema's own
	// vocabulary. An instance's field key cannot appear, because it is the
	// author's.
	for _, token := range []string{"severity", "risk", "priority", "tier", "findings"} {
		if strings.Contains(body, token) {
			t.Errorf("the shipped document names the instance token %q", token)
		}
	}
}

// TestPayloadRefusalsArePathPrecise is C3's requirement stated against the
// package that renders it: a refusal names the element and the property, in the
// notation a worker's payload file is written in.
func TestPayloadRefusalsArePathPrecise(t *testing.T) {
	reg, err := Compile("findings", 1, arraySchema(
		`"risk": {"type": "string", "enum": ["info","low","medium","high","blocker"], "ordered_enum": true}`))
	testsupport.Must(t, err, "compiling: %v", err)

	err = reg.ValidatePayload([]byte(`[{"risk":"low"},{"risk":"low"},{"risk":"low"},{"risk":"urgent"}]`))
	if err == nil {
		t.Fatal("expected a refusal")
	}

	const want = `payload[3].risk: value "urgent" is not one of ["info","low","medium","high","blocker"]`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal is not path-precise\n  want: %s\n  got:  %s", want, err.Error())
	}
	if !strings.Contains(err.Error(), "findings@1") {
		t.Errorf("refusal does not name the schema it consulted: %s", err.Error())
	}
}

// TestAnUnparseablePayloadIsRefusedAsOne keeps the JSON failure inside the same
// refusal type, so a caller has one error shape to map to VALIDATION_ERROR
// rather than two.
func TestAnUnparseablePayloadIsRefusedAsOne(t *testing.T) {
	reg, err := Compile("findings", 1, arraySchema(`"risk": {"type": "string"}`))
	testsupport.Must(t, err, "compiling: %v", err)
	err = reg.ValidatePayload([]byte(`[{"risk": `))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if _, ok := err.(*PayloadError); !ok {
		t.Fatalf("expected a *PayloadError, got %T", err)
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("refusal does not say what was wrong: %s", err.Error())
	}
}

// TestAMalformedSchemaDocumentIsRefusedAtCompile covers §4.5's refusal row: a
// schema that does not compile is refused at REGISTER, not at first use. A
// registry that accepted a broken document would surface it hours into a run,
// on a step whose work is already done.
func TestAMalformedSchemaDocumentIsRefusedAtCompile(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"not JSON at all", `{"type":`, "not valid JSON"},
		{"a keyword with the wrong type", `{"type": "array", "items": {"required": "risk"}}`, "does not compile"},
		{"a broken regexp", `{"type": "string", "pattern": "("}`, "does not compile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile("bad", 1, []byte(tc.body))
			if err == nil {
				t.Fatal("expected a refusal, the document compiled")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestTheAnnotationConstrainsNothing is O3: `ordered_enum` is an ANNOTATION,
// not an assertion. A document valid under the schema-minus-annotation is valid
// under it — which is what makes O4 safe, since the validator never sees the
// key and therefore cannot be asked to enforce it.
func TestTheAnnotationConstrainsNothing(t *testing.T) {
	withAnnotation := arraySchema(
		`"risk": {"type": "string", "enum": ["low","medium","high"], "ordered_enum": true}`)
	without := arraySchema(
		`"risk": {"type": "string", "enum": ["low","medium","high"]}`)

	annotated, err := Compile("a", 1, withAnnotation)
	testsupport.Must(t, err, "compiling the annotated document: %v", err)
	plain, err := Compile("b", 1, without)
	testsupport.Must(t, err, "compiling the plain document: %v", err)

	for _, doc := range []string{
		`[{"risk":"low"}]`,
		`[{"risk":"nope"}]`,
		`[{"risk":"high","extra":1}]`,
		`[]`,
	} {
		annotatedErr := annotated.ValidatePayload([]byte(doc))
		plainErr := plain.ValidatePayload([]byte(doc))
		if (annotatedErr == nil) != (plainErr == nil) {
			t.Errorf("the annotation changed a verdict for %s: annotated=%v plain=%v",
				doc, annotatedErr, plainErr)
		}
	}
}
