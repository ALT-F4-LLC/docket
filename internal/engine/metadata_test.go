package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// COMPLETION METADATA (docs/tdd/completion-metadata.md).
//
// `step complete --metadata` was accepted, reported success, and dropped: the
// flag parsed, the option field existed and documented a merge, the column
// existed, and the R7 rollup read it correctly — but nothing in the engine ever
// read `opts.Metadata`. Zero steps in any database carried metadata.
//
// The tests below are written against the merge (§1.2), the cap (§1.1.1), and
// the §6.9 refusal-writes-nothing property that places validation before the
// transaction opens.
//
// GENERICITY: every bag in this file uses NEUTRAL keys — `desk`, `tier`,
// `rework`. The reference instance's routing keys are instance data and must
// never appear in core, and scripts/qa/genericity.sh scans tests too.

// TestCompleteMetadataIsPersisted is the feature reduced to one assertion: the bag a
// worker reports at completion reaches the step's row.
//
// This is the regression test. Before the fix it fails with empty metadata,
// which is exactly what the tracker reproduced through the CLI.
func TestCompleteMetadataIsPersisted(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
		Metadata: `{"desk":"front","rework":"true"}`,
	})
	testsupport.Must(t, err, "complete: %v", err)

	bag := stepMetadata(t, conn, stepID)
	if bag["desk"] != "front" {
		t.Errorf("desk = %v, want front — the completing worker's bag was dropped (DKT-68)",
			bag["desk"])
	}
	if bag["rework"] != "true" {
		t.Errorf("rework = %v, want true", bag["rework"])
	}
}

// TestCompleteMetadataMergesOverDefinition is §1.2: the definition's bag
// survives, the worker's keys are added, and a key in both takes the WORKER's
// value. `saga.go` has promised this merge in a comment since S3.
func TestCompleteMetadataMergesOverDefinition(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")

	// Seed a definition-side bag, which is what activation writes.
	seedStepMetadata(t, conn, stepID, `{"desk":"back","tier":"a"}`)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
		Metadata: `{"desk":"front","rework":"true"}`,
	})
	testsupport.Must(t, err, "complete: %v", err)

	bag := stepMetadata(t, conn, stepID)
	if bag["tier"] != "a" {
		t.Errorf("tier = %v, want a — a definition-only key must survive", bag["tier"])
	}
	if bag["desk"] != "front" {
		t.Errorf("desk = %v, want front — the worker's value must win", bag["desk"])
	}
	if bag["rework"] != "true" {
		t.Errorf("rework = %v, want true — a worker-only key must be added", bag["rework"])
	}
}

// TestCompleteWithoutMetadataLeavesDefinitionBag is the no-regression
// assertion: every existing caller passes no `--metadata`, and none of them may
// see their step's bag change.
func TestCompleteWithoutMetadataLeavesDefinitionBag(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	const seeded = `{"desk":"back","tier":"a"}`
	seedStepMetadata(t, conn, stepID, seeded)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	if got := rawStepMetadata(t, conn, stepID); got != seeded {
		t.Errorf("metadata = %q, want %q byte-identical — an absent flag writes nothing",
			got, seeded)
	}
}

// TestCompleteMetadataNestedValueIsReplacedWholesale is §1.2's shallow-merge
// clause. Values are OPAQUE: a nested object is a value, not a subtree to
// descend into. Deep-merging would be core having an opinion about the
// structure of an instance's bag.
func TestCompleteMetadataNestedValueIsReplacedWholesale(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	seedStepMetadata(t, conn, stepID, `{"desk":{"floor":"2","wing":"east"}}`)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
		Metadata: `{"desk":{"floor":"3"}}`,
	})
	testsupport.Must(t, err, "complete: %v", err)

	bag := stepMetadata(t, conn, stepID)
	nested, ok := bag["desk"].(map[string]any)
	if !ok {
		t.Fatalf("desk = %#v, want an object", bag["desk"])
	}
	if nested["floor"] != "3" {
		t.Errorf("desk.floor = %v, want 3", nested["floor"])
	}
	if _, present := nested["wing"]; present {
		t.Error("desk.wing survived — the merge must be SHALLOW, replacing the value wholesale")
	}
}

// TestCompleteMetadataRefusals is §1.1's validation ladder. Every refusal is
// pre-transaction, so each case also asserts §6.9's property: nothing written,
// `row_version` unmoved, no artifact recorded.
func TestCompleteMetadataRefusals(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
		wantIn   string
	}{
		{"invalid JSON", `{"desk":`, "metadata"},
		{"a JSON array", `[{"desk":"front"}]`, "object"},
		{"a JSON scalar", `"front"`, "object"},
		{"a JSON number", `7`, "object"},
		{"JSON null", `null`, "object"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			run, _ := activatedRun(t, conn)
			e := testEngine()

			stepID := stepIDByInstance(t, conn, "implement@0")
			claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
			testsupport.Must(t, err, "claim: %v", err)

			before := stepRowVersion(t, conn, stepID)

			err = e.CompleteStep(conn, stepID, CompleteOptions{
				Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
				Metadata: tc.metadata,
			})
			if err == nil {
				t.Fatal("the completion was accepted, want VALIDATION_ERROR")
			}
			if code, _ := CodeOf(err); code != CodeValidation {
				t.Errorf("error code = %q, want %q", code, CodeValidation)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %q, want it to mention %q", err.Error(), tc.wantIn)
			}

			// §6.9: a refusal before the transaction writes NOTHING.
			if after := stepRowVersion(t, conn, stepID); after != before {
				t.Errorf("row_version moved %d -> %d on a refusal", before, after)
			}
			artifacts, err := db.ListRunArtifacts(conn, run.ID)
			testsupport.Must(t, err, "ListRunArtifacts: %v", err)
			if len(artifacts) != 0 {
				t.Errorf("a refused completion recorded %d artifacts", len(artifacts))
			}

			// The step is still claimable-and-completable: the refusal cost the
			// worker nothing but the round trip.
			if err := e.CompleteStep(conn, stepID, CompleteOptions{
				Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
				Metadata: `{"desk":"front"}`,
			}); err != nil {
				t.Errorf("the step was not completable after a refusal: %v", err)
			}
		})
	}
}

// TestCompleteMetadataCap is §1.1.1, the cap's own test.
//
// The boundary is INCLUSIVE and asserted in both directions so an off-by-one
// cannot pass silently, and the message must name both numbers plus the remedy
// — R12's phrasing style, because an operator needs to know what to do next.
func TestCompleteMetadataCap(t *testing.T) {
	// A bag of exactly MetadataMaxBytes bytes: {"desk":"aaa…"}.
	const envelope = `{"desk":""}`
	atCap := `{"desk":"` + strings.Repeat("a", MetadataMaxBytes-len(envelope)) + `"}`
	if len(atCap) != MetadataMaxBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(atCap), MetadataMaxBytes)
	}
	overCap := `{"desk":"` + strings.Repeat("a", MetadataMaxBytes-len(envelope)+1) + `"}`

	t.Run("exactly at the cap is accepted", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)
		e := testEngine()

		stepID := stepIDByInstance(t, conn, "implement@0")
		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
		testsupport.Must(t, err, "claim: %v", err)
		err = e.CompleteStep(conn, stepID, CompleteOptions{
			Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
			Metadata: atCap,
		})
		testsupport.Must(t, err, "a bag of exactly %d bytes was refused: %v", MetadataMaxBytes, err)
	})

	t.Run("one byte over is refused", func(t *testing.T) {
		conn := mustDB(t)
		run, _ := activatedRun(t, conn)
		e := testEngine()

		stepID := stepIDByInstance(t, conn, "implement@0")
		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
		testsupport.Must(t, err, "claim: %v", err)
		before := stepRowVersion(t, conn, stepID)

		err = e.CompleteStep(conn, stepID, CompleteOptions{
			Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
			Metadata: overCap,
		})
		if code, _ := CodeOf(err); code != CodeValidation {
			t.Fatalf("error code = %q (err %v), want %q", code, err, CodeValidation)
		}
		// BOTH numbers, and the remedy — the error has to be actionable.
		for _, want := range []string{"16385", "16384", "artifact", "payload"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to mention %q", err.Error(), want)
			}
		}

		if after := stepRowVersion(t, conn, stepID); after != before {
			t.Errorf("row_version moved %d -> %d on a refusal", before, after)
		}
		artifacts, err := db.ListRunArtifacts(conn, run.ID)
		testsupport.Must(t, err, "ListRunArtifacts: %v", err)
		if len(artifacts) != 0 {
			t.Errorf("a refused completion recorded %d artifacts", len(artifacts))
		}
	})

	// The cap measures the RAW INPUT, not the merged result. Measuring the
	// merge would make a refusal depend on what the definition bag already
	// held, so an unchanged command could start failing because a workflow
	// author edited an unrelated file.
	t.Run("the cap measures raw input, not the merged result", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)
		e := testEngine()

		stepID := stepIDByInstance(t, conn, "implement@0")
		// A large definition bag: the merged text will exceed the cap even
		// though the incoming bag does not.
		seedStepMetadata(t, conn, stepID,
			`{"tier":"`+strings.Repeat("b", MetadataMaxBytes-len(`{"tier":""}`))+`"}`)

		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
		testsupport.Must(t, err, "claim: %v", err)
		err = e.CompleteStep(conn, stepID, CompleteOptions{
			Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
			Metadata: `{"desk":"front"}`,
		})
		testsupport.Must(t, err, "a small bag was refused because the MERGED text was large: %v", err)
	})
}

// TestCompleteMetadataDoesNotDoubleApply is R9 at the metadata level: a second
// `complete` is refused, so the merge runs at most once per attempt.
func TestCompleteMetadataDoesNotDoubleApply(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	opts := CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
		Metadata: `{"desk":"front"}`,
	}
	err = e.CompleteStep(conn, stepID, opts)
	testsupport.Must(t, err, "first complete: %v", err)
	first := rawStepMetadata(t, conn, stepID)

	if err := e.CompleteStep(conn, stepID, opts); !errors.Is(err, db.ErrNotHolder) {
		t.Errorf("second complete = %v, want ErrNotHolder (R9)", err)
	}
	if got := rawStepMetadata(t, conn, stepID); got != first {
		t.Errorf("metadata changed on the refused second complete: %q -> %q", first, got)
	}
}

// TestMergeMetadata is the pure function `fail --metadata` reuses (§1.6). It knows
// nothing about sagas, transactions, or which verb called it — which is what
// makes `fail --metadata` parity a matter of adding a flag rather than a
// mechanism.
func TestMergeMetadata(t *testing.T) {
	cases := []struct {
		name       string
		definition string
		completion string
		want       string
		wantErr    bool
	}{
		{"both empty", "", "", "", false},
		{"definition only", `{"tier":"a"}`, "", `{"tier":"a"}`, false},
		{"completion only", "", `{"desk":"front"}`, `{"desk":"front"}`, false},
		{"disjoint keys", `{"tier":"a"}`, `{"desk":"front"}`, `{"desk":"front","tier":"a"}`, false},
		{"completion wins", `{"desk":"back"}`, `{"desk":"front"}`, `{"desk":"front"}`, false},
		{"nested replaced wholesale", `{"d":{"a":"1","b":"2"}}`, `{"d":{"a":"9"}}`, `{"d":{"a":"9"}}`, false},
		{"non-string values survive", `{"n":1}`, `{"m":true}`, `{"m":true,"n":1}`, false},
		{"invalid completion", `{"tier":"a"}`, `{`, "", true},
		{"completion is an array", "", `[1]`, "", true},
		{"completion is a scalar", "", `"x"`, "", true},
		{"invalid definition", `{`, `{"desk":"front"}`, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeMetadata(tc.definition, tc.completion)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mergeMetadata(%q, %q) = %q, want an error",
						tc.definition, tc.completion, got)
				}
				return
			}
			testsupport.Must(t, err, "mergeMetadata(%q, %q): %v", tc.definition, tc.completion, err)
			if got != tc.want {
				t.Errorf("mergeMetadata(%q, %q) = %q, want %q",
					tc.definition, tc.completion, got, tc.want)
			}
		})
	}
}

// TestMergeMetadataIsDeterministic is R9 through the merge: the stored text is
// stable regardless of the order keys arrived in, so goldens do not flap on Go
// map iteration order.
func TestMergeMetadataIsDeterministic(t *testing.T) {
	const definition = `{"tier":"a","desk":"back","zone":"1"}`
	const completion = `{"rework":"true","desk":"front","batch":"7"}`

	first, err := mergeMetadata(definition, completion)
	testsupport.Must(t, err, "mergeMetadata: %v", err)
	for i := 0; i < 32; i++ {
		got, err := mergeMetadata(definition, completion)
		testsupport.Must(t, err, "mergeMetadata: %v", err)
		if got != first {
			t.Fatalf("merge is not deterministic: %q then %q", first, got)
		}
	}
}

// TestMergeMetadataReadsNoKey is the mechanical form of the genericity promise,
// mirroring db.TestMetadataRollupReadsNoKey: the merge groups and overlays keys
// without naming a single one. A merge that special-cased a key would be core
// having an opinion about what a workflow author's bag of strings means.
//
// The banned terms are assembled from fragments rather than written out,
// because scripts/qa/genericity.sh scans THIS FILE's string literals like any
// other core surface — spelling them here would fail the very gate the test
// exists to reinforce. The reference instance's routing-key vocabulary is
// checked the same way, and for the same reason.
func TestMergeMetadataReadsNoKey(t *testing.T) {
	source, err := os.ReadFile("metadata.go")
	testsupport.Must(t, err, "reading metadata.go: %v", err)
	lowered := strings.ToLower(string(source))

	// docs/design/genericity.md's banned set, plus the recorded routing keys,
	// each split so no banned term appears as a literal.
	banned := []string{
		"mod" + "el", "pro" + "mpt", "ll" + "m", "age" + "nt", "bri" + "ef",
		"eff" + "ort", "resol" + "ved", "req" + "uested",
	}
	for _, term := range banned {
		if strings.Contains(lowered, term) {
			t.Errorf("metadata.go mentions %q — core must read no key and name no instance concept",
				term)
		}
	}
}

// FAIL METADATA — parity with `step complete --metadata`.
//
// `step fail` accepts the same opaque `--metadata` bag, merged with the same
// mergeMetadata and written with the same db.SetStepMetadataTx `stageZero`
// uses. Every test below uses the committed `implement@0` fixture, which
// declares `max_attempts = 2` — so ONE failure leaves `attempt` at 1 (E-8:
// attempt counts claims, never failures) and the step still has a retry left.

// TestFailMetadataIsPersisted is the feature reduced to one assertion: the bag a
// worker reports at `step fail` reaches the step's row, the same property
// pinned for `complete`.
func TestFailMetadataIsPersisted(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	err = e.FailStep(conn, stepID, claim.Token, "gave up",
		`{"desk":"front","rework":"true"}`, nowMS)
	testsupport.Must(t, err, "fail: %v", err)

	bag := stepMetadata(t, conn, stepID)
	if bag["desk"] != "front" {
		t.Errorf("desk = %v, want front — a failing worker's bag was dropped", bag["desk"])
	}
	if bag["rework"] != "true" {
		t.Errorf("rework = %v, want true", bag["rework"])
	}
}

// TestFailMetadataMergesOverDefinition is complete's §1.2 merge, exercised
// through fail: the definition's bag survives, the worker's keys are added,
// and a key in both takes the worker's value.
func TestFailMetadataMergesOverDefinition(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	seedStepMetadata(t, conn, stepID, `{"desk":"back","tier":"a"}`)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, stepID, claim.Token, "gave up",
		`{"desk":"front","rework":"true"}`, nowMS)
	testsupport.Must(t, err, "fail: %v", err)

	bag := stepMetadata(t, conn, stepID)
	if bag["tier"] != "a" {
		t.Errorf("tier = %v, want a — a definition-only key must survive", bag["tier"])
	}
	if bag["desk"] != "front" {
		t.Errorf("desk = %v, want front — the worker's value must win", bag["desk"])
	}
	if bag["rework"] != "true" {
		t.Errorf("rework = %v, want true — a worker-only key must be added", bag["rework"])
	}
}

// TestFailWithoutMetadataLeavesDefinitionBag is the no-regression assertion:
// every caller that predates the flag passes no `--metadata`, and none of them may see
// their step's bag change.
func TestFailWithoutMetadataLeavesDefinitionBag(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	const seeded = `{"desk":"back","tier":"a"}`
	seedStepMetadata(t, conn, stepID, seeded)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, stepID, claim.Token, "note only", "", nowMS)
	testsupport.Must(t, err, "fail: %v", err)

	if got := rawStepMetadata(t, conn, stepID); got != seeded {
		t.Errorf("metadata = %q, want %q byte-identical — an absent flag writes nothing",
			got, seeded)
	}
}

// TestFailMetadataRefusals mirrors TestCompleteMetadataRefusals: invalid
// metadata is refused pre-transaction, and — because the attempt counter is
// bumped only by CLAIMS (E-8) — a refused `fail` spends no claim either, so
// the step is failable again on the SAME token with a corrected bag.
func TestFailMetadataRefusals(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
		wantIn   string
	}{
		{"invalid JSON", `{"desk":`, "metadata"},
		{"a JSON array", `[{"desk":"front"}]`, "object"},
		{"a JSON scalar", `"front"`, "object"},
		{"a JSON number", `7`, "object"},
		{"JSON null", `null`, "object"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)
			e := testEngine()

			stepID := stepIDByInstance(t, conn, "implement@0")
			claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
			testsupport.Must(t, err, "claim: %v", err)

			before := stepRowVersion(t, conn, stepID)

			err = e.FailStep(conn, stepID, claim.Token, "note", tc.metadata, nowMS)
			if err == nil {
				t.Fatal("the failure was accepted, want VALIDATION_ERROR")
			}
			if code, _ := CodeOf(err); code != CodeValidation {
				t.Errorf("error code = %q, want %q", code, CodeValidation)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %q, want it to mention %q", err.Error(), tc.wantIn)
			}

			// §6.9: a refusal before the transaction writes NOTHING.
			if after := stepRowVersion(t, conn, stepID); after != before {
				t.Errorf("row_version moved %d -> %d on a refusal", before, after)
			}

			// The step is still failable on the SAME token: the refusal cost the
			// caller nothing but the round trip, and spent no claim.
			if err := e.FailStep(conn, stepID, claim.Token, "note",
				`{"desk":"front"}`, nowMS); err != nil {
				t.Errorf("the step was not failable after a refusal: %v", err)
			}
		})
	}
}

// TestFailMetadataCap mirrors TestCompleteMetadataCap: the same 16KiB cap
// established for `complete` applies to `fail`, unchanged — but the remedy names
// `fail`'s own channel (C14): `step fail` has no `--artifact-file` or
// `--payload-file`, so pointing at either would send an operator looking for
// a flag this verb does not offer.
func TestFailMetadataCap(t *testing.T) {
	const envelope = `{"desk":""}`
	overCap := `{"desk":"` + strings.Repeat("a", MetadataMaxBytes-len(envelope)+1) + `"}`

	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	before := stepRowVersion(t, conn, stepID)

	err = e.FailStep(conn, stepID, claim.Token, "note", overCap, nowMS)
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Fatalf("error code = %q (err %v), want %q", code, err, CodeValidation)
	}
	for _, want := range []string{fmt.Sprintf("%d", MetadataMaxBytes), "note"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err.Error(), want)
		}
	}
	// `fail` offers no artifact or payload channel, so the remedy must not
	// point at either (C14).
	for _, mustNotContain := range []string{"artifact-file", "payload-file"} {
		if strings.Contains(err.Error(), mustNotContain) {
			t.Errorf("err = %q names a flag `step fail` does not offer (%q)",
				err.Error(), mustNotContain)
		}
	}

	if after := stepRowVersion(t, conn, stepID); after != before {
		t.Errorf("row_version moved %d -> %d on a refusal", before, after)
	}
}

// TestFailMetadataSurvivesIntoRetry is docs/tdd/completion-metadata.md §1.6's
// open question, answered: a failed attempt's metadata survives into the
// retry, last-write-wins, same as completion's overlay semantics. A worker
// that reports why it failed has produced the most valuable metadata in the
// run, and discarding it at retry would lose exactly the diagnostic an
// operator wants.
//
// THE REAL RETRY, under E-8's claims-only attempt counting: `implement@0`
// declares `max_attempts = 2`, and a claim alone brings `attempt` to 1 — so
// ONE failure leaves it at 1 (still below the limit) and the step genuinely
// returns to `pending` for a second claim, rather than routing on the first
// failure the way a claims-plus-failures counter (E-8's defect) would have.
func TestFailMetadataSurvivesIntoRetry(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, stepID, claim.Token, "attempt one", `{"a":"1"}`, nowMS)
	testsupport.Must(t, err, "fail: %v", err)

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Attempt != 1 || step.Status != db.StepPending {
		t.Fatalf("attempt = %d status = %q after one claim + one failure, want "+
			"1 and %q — implement@0 declares two attempts and this is a REAL "+
			"retry, not exhaustion", step.Attempt, step.Status, db.StepPending)
	}

	bag := stepMetadata(t, conn, stepID)
	if bag["a"] != "1" {
		t.Fatalf("bag after the failed attempt = %v, want a=1 — the metadata "+
			"must survive the return to pending", bag)
	}

	// The retry: re-claim and complete, overlaying on top of the failure's bag.
	claim2, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "re-claim: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim2.Token, Artifact: []byte("body"), NowMS: nowMS,
		Metadata: `{"a":"2","b":"3"}`,
	})
	testsupport.Must(t, err, "complete: %v", err)

	bag = stepMetadata(t, conn, stepID)
	if bag["a"] != "2" {
		t.Errorf("a = %v, want 2 — the retry's completion overlays the failed "+
			"attempt's bag, last-write-wins", bag["a"])
	}
	if bag["b"] != "3" {
		t.Errorf("b = %v, want 3 — a key only the retry reported must be added", bag["b"])
	}
}

// TestFailMetadataReachesTheReportRollup is the AC that a failed step's
// metadata is visible in `run report --json` through the existing R7 rollup,
// alongside completions. No new rollup, no new report section:
// db.MetadataRollup already reads `steps.metadata` regardless of the step's
// terminal status, so this proves the WRITE path needs no read-side change.
func TestFailMetadataReachesTheReportRollup(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, stepID, claim.Token, "gave up", `{"desk":"front"}`, nowMS)
	testsupport.Must(t, err, "fail: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	found := false
	for _, rollup := range report.Metadata {
		if rollup.Key != "desk" {
			continue
		}
		for _, vc := range rollup.Values {
			if vc.Value == "front" && vc.Count == 1 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("report.Metadata = %s, want a failed step's desk=front rollup",
			mustJSON(t, report.Metadata))
	}
}

// TestFailValidatesMetadataBeforeTheTransactionOpens is C6: a runtime
// assertion over the ROW cannot pin this property, because `human.go`'s
// `FailStep` runs entirely under `defer tx.Rollback()` — any write a
// refused call made INSIDE the transaction is undone identically whether
// the validation ran before `conn.Begin()` or immediately after it, so
// `row_version` alone cannot tell the two placements apart (this is why
// TestFailMetadataRefusals's row_version check is necessary but not
// SUFFICIENT for this property).
//
// So this is a SOURCE-POSITION check, the same shape as
// TestNoPoolReadsInsideTransactions: it parses human.go's AST and asserts
// that FailStep's calls to validateFailMetadataSize and DecodeMetadataBag
// appear before its call to conn.Begin(). Moving either call after
// conn.Begin() — even though authorize itself has no side effect today —
// makes this fail, which is the placement guarantee stageZero's C5 makes
// and `fail` must mirror.
func TestFailValidatesMetadataBeforeTheTransactionOpens(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "human.go", nil, 0)
	testsupport.Must(t, err, "parsing human.go: %v", err)

	var beginPos, sizePos, decodePos token.Pos

	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "FailStep" || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				switch fn.Name {
				case "validateFailMetadataSize":
					sizePos = call.Pos()
				case "DecodeMetadataBag":
					decodePos = call.Pos()
				}
			case *ast.SelectorExpr:
				if ident, ok := fn.X.(*ast.Ident); ok && ident.Name == "conn" && fn.Sel.Name == "Begin" {
					beginPos = call.Pos()
				}
			}
			return true
		})
		return false
	})

	if beginPos == token.NoPos {
		t.Fatal("FailStep no longer calls conn.Begin() — this test needs updating, not deleting")
	}
	if sizePos == token.NoPos {
		t.Fatal("FailStep no longer calls validateFailMetadataSize")
	}
	if decodePos == token.NoPos {
		t.Fatal("FailStep no longer calls DecodeMetadataBag")
	}
	if sizePos > beginPos {
		t.Error("validateFailMetadataSize runs AFTER conn.Begin() — a refusal " +
			"would happen inside the transaction, invisible to a row_version check")
	}
	if decodePos > beginPos {
		t.Error("DecodeMetadataBag runs AFTER conn.Begin() — a refusal would " +
			"happen inside the transaction, invisible to a row_version check")
	}
}

// CLAIM METADATA (docs/tdd/completion-metadata.md §1.7, DKT-592).
//
// The bag a dispatcher knows at claim time was reaching the row only through
// `complete`, so the steps that failed or crashed — the ones an operator most
// wants to characterize — were the only steps carrying nothing. Two runs
// measured it: 76 of 119 steps carried the keys, then 55 of 83. A rollup over
// those keys went blind at precisely the rows that motivated it.
//
// GENERICITY, as above: the bags here use NEUTRAL keys. `tier_requested` and
// `desk_resolved` mirror the SHAPE of a routing pair — one fact known when the
// work is handed out, one known only when it comes back — without naming any
// instance's vocabulary.

// TestClaimMetadataIsPersisted is the feature reduced to one assertion: the bag
// a dispatcher supplies at claim reaches the step's row IN THE CLAIM, with no
// completion anywhere in the test.
func TestClaimMetadataIsPersisted(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	stepID := stepIDByInstance(t, conn, "implement@0")
	_, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "worker", NowMS: nowMS,
		Metadata: `{"tier_requested":"a","desk_requested":"front"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	bag := stepMetadata(t, conn, stepID)
	if bag["tier_requested"] != "a" {
		t.Errorf("tier_requested = %v, want a — the claim's bag was not recorded (DKT-592)",
			bag["tier_requested"])
	}
	if bag["desk_requested"] != "front" {
		t.Errorf("desk_requested = %v, want front", bag["desk_requested"])
	}
}

// TestClaimMetadataSurvivesFailure is DKT-592's whole point: a step that FAILS
// after being claimed still carries what the dispatcher knew when it handed
// the step out. Before the claim-time write this step recorded nothing, which
// is why the drift row was blind exactly where it mattered.
func TestClaimMetadataSurvivesFailure(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "worker", NowMS: nowMS,
		Metadata: `{"tier_requested":"a","desk_requested":"front"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	// The failing worker reports nothing of its own — the case a crashed
	// executor's supervisor produces.
	err = e.FailStep(conn, stepID, claim.Token, "gave up", "", nowMS)
	testsupport.Must(t, err, "fail: %v", err)

	bag := stepMetadata(t, conn, stepID)
	if bag["tier_requested"] != "a" || bag["desk_requested"] != "front" {
		t.Errorf("bag after a failure = %v, want both claim-time keys — a step "+
			"that never completes is the one whose dispatch facts matter most", bag)
	}
}

// TestClaimMetadataSurvivesTheReap is the CRASH case, which is not the failure
// case: nobody calls `fail` at all. The worker dies, the lease lapses, and the
// next claim reaps it. The reap writes status, lease and counters and must
// leave `metadata` alone.
func TestClaimMetadataSurvivesTheReap(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	stepID := stepIDByInstance(t, conn, "implement@0")
	_, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "worker", NowMS: nowMS, TTLOverride: 1_000,
		Metadata: `{"tier_requested":"a"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	// The holder is gone. The lazy reap fires on the next claim (§6.3).
	const after = nowMS + 60_000
	_, err = ClaimStep(conn, stepID, ClaimOptions{
		Owner: "worker-2", NowMS: after,
		Metadata: `{"tier_requested":"b"}`,
	})
	testsupport.Must(t, err, "re-claim: %v", err)

	bag := stepMetadata(t, conn, stepID)
	if bag["tier_requested"] != "b" {
		t.Errorf("tier_requested = %v, want b — the re-claim's bag overlays the "+
			"dead attempt's, last-write-wins like every other writer of this column",
			bag["tier_requested"])
	}
}

// TestClaimThenCompleteCarriesEveryKey is the AC that the two writes compose:
// a step that completes normally ends up with BOTH pairs — the facts known at
// dispatch and the facts known only at completion. The claim-time keys must not
// be clobbered when the completion bag merges in.
func TestClaimThenCompleteCarriesEveryKey(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "worker", NowMS: nowMS,
		Metadata: `{"tier_requested":"a","desk_requested":"front"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
		Metadata: `{"tier_resolved":"b","desk_resolved":"back"}`,
	})
	testsupport.Must(t, err, "complete: %v", err)

	bag := stepMetadata(t, conn, stepID)
	for key, want := range map[string]string{
		"tier_requested": "a", "desk_requested": "front",
		"tier_resolved": "b", "desk_resolved": "back",
	} {
		if bag[key] != want {
			t.Errorf("%s = %v, want %s — a completed step carries the whole pair "+
				"of pairs; the completion merge must not clobber the claim's keys",
				key, bag[key], want)
		}
	}
}

// TestClaimMetadataMergesOverDefinition is §1.2's merge, exercised through
// claim: a definition-only key survives, the dispatcher's keys are added, and a
// key in both takes the DISPATCHER's value.
func TestClaimMetadataMergesOverDefinition(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	stepID := stepIDByInstance(t, conn, "implement@0")
	seedStepMetadata(t, conn, stepID, `{"desk":"back","tier":"a"}`)

	_, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "worker", NowMS: nowMS,
		Metadata: `{"desk":"front","tier_requested":"b"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	bag := stepMetadata(t, conn, stepID)
	if bag["tier"] != "a" {
		t.Errorf("tier = %v, want a — a definition-only key must survive", bag["tier"])
	}
	if bag["desk"] != "front" {
		t.Errorf("desk = %v, want front — the dispatcher's value must win", bag["desk"])
	}
	if bag["tier_requested"] != "b" {
		t.Errorf("tier_requested = %v, want b — a dispatcher-only key must be added",
			bag["tier_requested"])
	}
}

// TestClaimWithoutMetadataLeavesDefinitionBag is the no-regression assertion:
// every existing claimant passes no `--metadata`, and none of them may see
// their step's bag change — nor pay a row_version bump for a write that has
// nothing to write.
func TestClaimWithoutMetadataLeavesDefinitionBag(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	stepID := stepIDByInstance(t, conn, "implement@0")
	const seeded = `{"desk":"back","tier":"a"}`
	seedStepMetadata(t, conn, stepID, seeded)

	_, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	if got := rawStepMetadata(t, conn, stepID); got != seeded {
		t.Errorf("metadata = %q, want %q byte-identical — an absent flag writes nothing",
			got, seeded)
	}
}

// TestClaimMetadataIsInTheContextBundle: the claim that recorded the bag also
// REPORTS it, so a worker reading `context.metadata` sees what it was
// dispatched with rather than the row's state before its own claim.
func TestClaimMetadataIsInTheContextBundle(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "worker", NowMS: nowMS, Metadata: `{"tier_requested":"a"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	if claim.Context == nil {
		t.Fatal("the claim returned no context bundle")
	}
	if claim.Context.Metadata["tier_requested"] != "a" {
		t.Errorf("context.metadata = %v, want tier_requested=a — the bundle must "+
			"report the bag this same claim recorded", claim.Context.Metadata)
	}
}

// TestClaimMetadataReachesTheReportRollup is the AC in the surface the defect
// was observed in: a step that was claimed and then FAILED shows its
// dispatch-time keys in `run report`'s R7 rollup. No read-side change — the
// rollup already reads the column regardless of the step's status.
func TestClaimMetadataReachesTheReportRollup(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "worker", NowMS: nowMS, Metadata: `{"tier_requested":"a"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, e.FailStep(conn, stepID, claim.Token, "gave up", "", nowMS),
		"fail: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	found := false
	for _, rollup := range report.Metadata {
		if rollup.Key != "tier_requested" {
			continue
		}
		for _, vc := range rollup.Values {
			if vc.Value == "a" && vc.Count == 1 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("report.Metadata = %s, want a FAILED step's tier_requested=a rollup — "+
			"the row the drift report was blind on", mustJSON(t, report.Metadata))
	}
}

// TestClaimMetadataRefusals is §1.1's validation ladder on the claim side.
// Every refusal is pre-transaction, and on THIS path that has an extra edge:
// the claim's transaction performs the lazy reap and the CAS, so a refusal
// that ran inside it would consume an attempt. Each case asserts the step is
// untouched AND still claimable.
func TestClaimMetadataRefusals(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
		wantIn   string
	}{
		{"invalid JSON", `{"desk":`, "metadata"},
		{"a JSON array", `[{"desk":"front"}]`, "object"},
		{"a JSON scalar", `"front"`, "object"},
		{"a JSON number", `7`, "object"},
		{"JSON null", `null`, "object"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)

			stepID := stepIDByInstance(t, conn, "implement@0")
			before := stepRowVersion(t, conn, stepID)

			_, err := ClaimStep(conn, stepID, ClaimOptions{
				Owner: "worker", NowMS: nowMS, Metadata: tc.metadata,
			})
			if err == nil {
				t.Fatal("the claim was accepted, want VALIDATION_ERROR")
			}
			if code, _ := CodeOf(err); code != CodeValidation {
				t.Errorf("error code = %q, want %q", code, CodeValidation)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %q, want it to mention %q", err.Error(), tc.wantIn)
			}

			// §6.9: a refusal before the transaction writes NOTHING — no claim,
			// no attempt, no status move.
			if after := stepRowVersion(t, conn, stepID); after != before {
				t.Errorf("row_version moved %d -> %d on a refusal", before, after)
			}
			step, err := db.GetStep(conn, stepID)
			testsupport.Must(t, err, "GetStep: %v", err)
			if step.Status != db.StepPending || step.Attempt != 0 {
				t.Errorf("step is %q at attempt %d after a refused claim, want %q at 0",
					step.Status, step.Attempt, db.StepPending)
			}

			// The step is still claimable: the refusal cost the dispatcher
			// nothing but the round trip.
			if _, err := ClaimStep(conn, stepID, ClaimOptions{
				Owner: "worker", NowMS: nowMS, Metadata: `{"desk":"front"}`,
			}); err != nil {
				t.Errorf("the step was not claimable after a refusal: %v", err)
			}
		})
	}
}

// TestClaimMetadataCap is §1.1.1 on the claim side. Same constant, same
// inclusive boundary asserted in both directions, and a message naming both
// numbers — with the remedy this verb can actually offer (a claim has no
// artifact or payload channel of its own; its completion does).
func TestClaimMetadataCap(t *testing.T) {
	const envelope = `{"desk":""}`
	atCap := `{"desk":"` + strings.Repeat("a", MetadataMaxBytes-len(envelope)) + `"}`
	if len(atCap) != MetadataMaxBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(atCap), MetadataMaxBytes)
	}
	overCap := `{"desk":"` + strings.Repeat("a", MetadataMaxBytes-len(envelope)+1) + `"}`

	t.Run("exactly at the cap is accepted", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)

		stepID := stepIDByInstance(t, conn, "implement@0")
		_, err := ClaimStep(conn, stepID, ClaimOptions{
			Owner: "worker", NowMS: nowMS, Metadata: atCap,
		})
		testsupport.Must(t, err, "a bag of exactly %d bytes was refused: %v",
			MetadataMaxBytes, err)
	})

	t.Run("one byte over is refused", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)

		stepID := stepIDByInstance(t, conn, "implement@0")
		before := stepRowVersion(t, conn, stepID)

		_, err := ClaimStep(conn, stepID, ClaimOptions{
			Owner: "worker", NowMS: nowMS, Metadata: overCap,
		})
		if code, _ := CodeOf(err); code != CodeValidation {
			t.Fatalf("error code = %q (err %v), want %q", code, err, CodeValidation)
		}
		for _, want := range []string{"16385", "16384", "completion"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to mention %q", err.Error(), want)
			}
		}
		if after := stepRowVersion(t, conn, stepID); after != before {
			t.Errorf("row_version moved %d -> %d on a refusal", before, after)
		}
	})

	// The cap measures the RAW INPUT, not the merged result — the same
	// property complete's cap has, for the same reason.
	t.Run("the cap measures raw input, not the merged result", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)

		stepID := stepIDByInstance(t, conn, "implement@0")
		seedStepMetadata(t, conn, stepID,
			`{"tier":"`+strings.Repeat("b", MetadataMaxBytes-len(`{"tier":""}`))+`"}`)

		_, err := ClaimStep(conn, stepID, ClaimOptions{
			Owner: "worker", NowMS: nowMS, Metadata: `{"desk":"front"}`,
		})
		testsupport.Must(t, err, "a small bag was refused because the MERGED text was large: %v", err)
	})
}

// TestClaimValidatesMetadataBeforeTheTransactionOpens is
// TestFailValidatesMetadataBeforeTheTransactionOpens's twin, and it is a
// SOURCE-POSITION check for the same reason: `claimStepWithGates` runs under
// `defer tx.Rollback()`, so a refusal made inside the transaction is undone
// identically to one made before it and `row_version` cannot tell the
// placements apart.
//
// The placement matters more here than on `fail`: that transaction performs
// the LAZY REAP and the CAS, so validation drifting after `Begin()` would put
// a malformed bag one code change away from consuming an attempt.
func TestClaimValidatesMetadataBeforeTheTransactionOpens(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "claim.go", nil, 0)
	testsupport.Must(t, err, "parsing claim.go: %v", err)

	var beginPos, sizePos, decodePos token.Pos

	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "claimStepWithGates" || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				switch fn.Name {
				case "validateClaimMetadataSize":
					sizePos = call.Pos()
				case "DecodeMetadataBag":
					decodePos = call.Pos()
				}
			case *ast.SelectorExpr:
				if ident, ok := fn.X.(*ast.Ident); ok && ident.Name == "conn" && fn.Sel.Name == "Begin" {
					if beginPos == token.NoPos {
						beginPos = call.Pos()
					}
				}
			}
			return true
		})
		return false
	})

	if beginPos == token.NoPos {
		t.Fatal("claimStepWithGates no longer calls conn.Begin() — this test needs updating, not deleting")
	}
	if sizePos == token.NoPos {
		t.Fatal("claimStepWithGates no longer calls validateClaimMetadataSize")
	}
	if decodePos == token.NoPos {
		t.Fatal("claimStepWithGates no longer calls DecodeMetadataBag")
	}
	if sizePos > beginPos {
		t.Error("validateClaimMetadataSize runs AFTER conn.Begin() — a refusal " +
			"would happen inside the transaction that reaps and claims")
	}
	if decodePos > beginPos {
		t.Error("DecodeMetadataBag runs AFTER conn.Begin() — a refusal would " +
			"happen inside the transaction that reaps and claims")
	}
}

// --- helpers ---------------------------------------------------------------

func stepMetadata(t *testing.T, conn *sql.DB, stepID int) map[string]any {
	t.Helper()
	raw := rawStepMetadata(t, conn, stepID)
	if raw == "" {
		return nil
	}
	var bag map[string]any
	err := json.Unmarshal([]byte(raw), &bag)
	testsupport.Must(t, err, "stored metadata %q is not an object: %v", raw, err)
	return bag
}

func rawStepMetadata(t *testing.T, conn *sql.DB, stepID int) string {
	t.Helper()
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	return step.Metadata
}

func seedStepMetadata(t *testing.T, conn *sql.DB, stepID int, bag string) {
	t.Helper()
	_, err := conn.Exec(`UPDATE steps SET metadata = ? WHERE id = ?`, bag, stepID)
	testsupport.Must(t, err, "seeding step metadata: %v", err)
}

func stepRowVersion(t *testing.T, conn *sql.DB, stepID int) int {
	t.Helper()
	var version int
	err := conn.QueryRow(
		`SELECT row_version FROM steps WHERE id = ?`, stepID,
	).Scan(&version)
	testsupport.Must(t, err, "reading row_version: %v", err)
	return version
}
