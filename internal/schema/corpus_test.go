package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// corpusCase is one testdata/corpus file (§4.1 mitigation 1).
type corpusCase struct {
	Why      string          `json:"why"`
	Schema   json.RawMessage `json:"schema"`
	Document json.RawMessage `json:"document"`
	Valid    bool            `json:"valid"`
	// Failures, when set, is how many leaf failures the document produces
	// BEFORE C3's five-line cap. It exists so the cap has something to cap.
	Failures        int    `json:"failures"`
	MessageContains string `json:"message_contains"`
}

func loadCorpus(t *testing.T) map[string]corpusCase {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join("testdata", "corpus", "*.json"))
	testsupport.Must(t, err, "globbing the corpus: %v", err)
	// A corpus that failed to load passes every assertion vacuously, which is
	// the one property a renovate gate may not have.
	if len(paths) == 0 {
		t.Fatal("the corpus is empty; TestCorpus would pass vacuously")
	}

	out := make(map[string]corpusCase, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		testsupport.Must(t, err, "reading %s: %v", path, err)
		var c corpusCase
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		if c.Why == "" {
			t.Fatalf("%s: every corpus case states why it exists", path)
		}
		out[strings.TrimSuffix(filepath.Base(path), ".json")] = c
	}
	return out
}

// TestCorpus is THE RENOVATE GATE (§4.1). A validator bump that changes a
// verdict — or the wording of a refusal a worker reads — turns red here rather
// than in production, on a `complete` that used to be accepted.
func TestCorpus(t *testing.T) {
	for name, c := range loadCorpus(t) {
		t.Run(name, func(t *testing.T) {
			reg, err := Compile("corpus", 1, c.Schema)
			testsupport.Must(t, err, "compiling the case schema: %v", err)

			err = reg.ValidatePayload(c.Document)
			if c.Valid {
				testsupport.Must(t, err, "expected the document to validate, got: %v", err)
				return
			}
			if err == nil {
				t.Fatal("expected the document to be refused, it validated")
			}

			perr, ok := err.(*PayloadError)
			if !ok {
				t.Fatalf("expected a *PayloadError, got %T", err)
			}
			if perr.Ref != "corpus@1" {
				t.Errorf("refusal names %q, want the schema it validated against", perr.Ref)
			}
			if c.MessageContains != "" && !strings.Contains(err.Error(), c.MessageContains) {
				t.Errorf("refusal does not carry the pinned substring\n  want: %s\n  got: %s",
					c.MessageContains, err.Error())
			}
			if c.Failures > 0 {
				// C3: at most five lines, then the count of what was dropped.
				if len(perr.Lines) != maxPayloadErrors {
					t.Errorf("rendered %d lines, want the %d-line cap",
						len(perr.Lines), maxPayloadErrors)
				}
				if want := c.Failures - maxPayloadErrors; perr.More != want {
					t.Errorf("reported (+%d more), want (+%d more)", perr.More, want)
				}
				if !strings.Contains(err.Error(), "(+2 more)") {
					t.Errorf("the capped refusal does not say how many were dropped: %s", err.Error())
				}
			}
		})
	}
}

// TestOrderedIndexRoundTrips is I2: the stored index is a CACHE of a pure
// function of the registered bytes, never a second source of truth.
//
// derive -> store -> read back -> re-derive must agree, for every corpus
// schema. If it ever does not, `schemas.ordered` and `schemas.body` disagree
// about what a field's order is, and a threshold routes on the stale one.
func TestOrderedIndexRoundTrips(t *testing.T) {
	for name, c := range loadCorpus(t) {
		t.Run(name, func(t *testing.T) {
			derived, err := DeriveIndex(c.Schema)
			testsupport.Must(t, err, "deriving: %v", err)

			stored, err := json.Marshal(derived)
			testsupport.Must(t, err, "storing: %v", err)

			var readBack Index
			if err := json.Unmarshal(stored, &readBack); err != nil {
				t.Fatalf("reading back: %v", err)
			}
			restored, err := json.Marshal(readBack)
			testsupport.Must(t, err, "re-storing: %v", err)
			if string(stored) != string(restored) {
				t.Errorf("the stored index does not survive a round trip\n  %s\n  %s",
					stored, restored)
			}

			// And the same index falls out of Compile, so the two derivations
			// (the standalone one a QA check uses and the one registration
			// takes) cannot drift.
			reg, err := Compile("corpus", 1, c.Schema)
			testsupport.Must(t, err, "compiling: %v", err)
			viaCompile, err := json.Marshal(reg.Ordered)
			testsupport.Must(t, err, "storing the compiled index: %v", err)
			if string(stored) != string(viaCompile) {
				t.Errorf("DeriveIndex and Compile disagree\n  %s\n  %s", stored, viaCompile)
			}

			for _, field := range readBack.Fields() {
				if !derived.Ordered(field) {
					t.Errorf("field %q survived the round trip but is no longer ordered", field)
				}
			}
		})
	}
}
