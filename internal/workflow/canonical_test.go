package workflow

import (
	"os"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestCanonicalRoundTrips: parse -> serialize -> parse reproduces the same
// canonical bytes. `parsed` is the PINNED interpretation of a definition, so a
// round trip that loses a field would mean activation reads something the
// author did not write.
func TestCanonicalRoundTrips(t *testing.T) {
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading the fixture: %v", err)
	def, err := Load(src)
	testsupport.Must(t, err, "Load: %v", err)

	first, err := Canonical(def)
	testsupport.Must(t, err, "Canonical: %v", err)

	restored, err := FromCanonical(first)
	testsupport.Must(t, err, "FromCanonical: %v", err)
	second, err := Canonical(restored)
	testsupport.Must(t, err, "Canonical (round 2): %v", err)

	if string(first) != string(second) {
		t.Errorf("canonical JSON does not round-trip:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestCanonicalIsByteStable: the serialization must not vary between runs.
//
// This is not a nicety. Map iteration order leaking into `parsed` would make
// the same file serialize differently on re-register, so an IDENTICAL
// re-register would compare unequal — and the idempotent-success path (§4.1)
// would report CONFLICT instead. `params`, `metadata`, `threshold`, and
// `limits` are all maps, so the risk is real on the fixture specifically.
func TestCanonicalIsByteStable(t *testing.T) {
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading the fixture: %v", err)

	var want []byte
	// Re-parse from source each time: a single *Definition serialized twice
	// would not exercise the map ordering that a fresh decode produces.
	for i := 0; i < 50; i++ {
		def, err := Load(src)
		testsupport.Must(t, err, "Load (iteration %d): %v", i, err)
		got, err := Canonical(def)
		testsupport.Must(t, err, "Canonical (iteration %d): %v", i, err)
		if want == nil {
			want = got
			continue
		}
		if string(got) != string(want) {
			t.Fatalf("canonical JSON is not byte-stable at iteration %d:\nwant %s\ngot  %s",
				i, want, got)
		}
	}
}

// TestCanonicalHashIsStable is the consequence that matters at the CLI: the
// same bytes hash the same, every time, so an identical re-register is an
// idempotent success rather than a CONFLICT.
func TestCanonicalHashIsStable(t *testing.T) {
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading the fixture: %v", err)

	first := SHA256(src)
	for i := 0; i < 10; i++ {
		if got := SHA256(src); got != first {
			t.Fatalf("SHA256 varies across calls: %s then %s", first, got)
		}
	}
	if len(first) != 64 {
		t.Errorf("SHA256 returned %d hex chars, want 64", len(first))
	}

	// Different bytes hash differently — the property CONFLICT detection rests
	// on. A trailing comment changes the bytes without changing the meaning,
	// and it must still be a different hash: `name@version` is frozen, and
	// version pinning is worth nothing if the pinned bytes can be swapped.
	if SHA256(append(append([]byte(nil), src...), []byte("\n# edited\n")...)) == first {
		t.Error("edited source hashes identically to the original")
	}
}

// TestCanonicalPreservesDefaults: the defaults §11.1 declares are materialized
// into `parsed`, so every reader sees one interpretation rather than
// re-deriving it identically and independently.
func TestCanonicalPreservesDefaults(t *testing.T) {
	def, err := Load([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "hint"
emits = "k"
[[step]]
name = "f"
after = ["a"]
fanout = ["p", "q"]
emits = "k"
`))
	testsupport.Must(t, err, "Load: %v", err)
	data, err := Canonical(def)
	testsupport.Must(t, err, "Canonical: %v", err)
	restored, err := FromCanonical(data)
	testsupport.Must(t, err, "FromCanonical: %v", err)

	if restored.Steps[0].Class != "hint" {
		t.Errorf("class default lost in the round trip: %q", restored.Steps[0].Class)
	}
	if restored.Steps[0].OnFail != OnFailWaitingHuman {
		t.Errorf("on_fail default lost in the round trip: %q", restored.Steps[0].OnFail)
	}
	if restored.Steps[1].MinSiblings == nil || *restored.Steps[1].MinSiblings != 2 {
		t.Errorf("min_siblings default lost in the round trip: %v", restored.Steps[1].MinSiblings)
	}
	if restored.Steps[0].ExpectedCost == nil || *restored.Steps[0].ExpectedCost != 0 {
		t.Errorf("expected_cost default lost in the round trip: %v", restored.Steps[0].ExpectedCost)
	}
}

// TestCanonicalPreservesRootDeclaration: `after` distinguishes a declared root
// (`[]`) from a step whose ordering comes from elsewhere (absent), and the
// stored form must keep them apart — V8 and V10 turn on exactly this.
func TestCanonicalPreservesRootDeclaration(t *testing.T) {
	def, err := Load([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "root"
after = []
executor = "x"
emits = "k"
[[step]]
name = "fixup"
loop = true
executor = "x"
emits = "k"
after_loop = "root"
`))
	testsupport.Must(t, err, "Load: %v", err)
	data, err := Canonical(def)
	testsupport.Must(t, err, "Canonical: %v", err)
	restored, err := FromCanonical(data)
	testsupport.Must(t, err, "FromCanonical: %v", err)

	if !restored.Steps[0].HasAfter() {
		t.Error("an explicit `after = []` did not survive as a declared root")
	}
	if restored.Steps[1].HasAfter() {
		t.Error("an absent `after` came back as declared")
	}
}

// TestEmbeddedTemplatesCanonicalize: every shipped template survives the same
// round trip, since `workflow register` stores their parsed form like any other.
func TestEmbeddedTemplatesCanonicalize(t *testing.T) {
	for _, name := range TemplateNames() {
		t.Run(name, func(t *testing.T) {
			src, err := Template(name)
			testsupport.Must(t, err, "Template: %v", err)
			def, err := Load(src)
			testsupport.Must(t, err, "Load: %v", err)
			first, err := Canonical(def)
			testsupport.Must(t, err, "Canonical: %v", err)
			restored, err := FromCanonical(first)
			testsupport.Must(t, err, "FromCanonical: %v", err)
			second, err := Canonical(restored)
			testsupport.Must(t, err, "Canonical (round 2): %v", err)
			if string(first) != string(second) {
				t.Errorf("template %q does not round-trip", name)
			}
		})
	}
}
