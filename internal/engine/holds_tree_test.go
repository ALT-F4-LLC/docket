package engine

import (
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"

	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `holds_tree` — the per-step declaration of whether a step occupies its
// issue's scope while it runs.
//
// Scope exclusion (R4) was class-BLIND: it excluded on any claimed or running
// step regardless of what that step does to the tree. Two different issues'
// judges — which only READ — excluded each other, so review fanouts serialized
// across issues, and an artifact-only `synthesize` step (which reads recorded
// payloads and never opens a file) lost claim races over files it would never
// touch.
//
// Core cannot infer this: classes are opaque strings and core attaches no
// meaning to `write` or to a judge's name (§6.5's genericity rule). So the
// instance DECLARES it, and core reads a boolean.
//
// The default is TRUE, which is both the conservative answer and every
// pre-existing workflow's behavior — asserted here, because a default that
// silently flipped to false would disable scope exclusion wholesale.

// TestHoldsTreeDefaultsToTrue is the safety property. An author who says
// nothing gets exclusion, because the two error directions are not symmetric.
func TestHoldsTreeDefaultsToTrue(t *testing.T) {
	def, err := workflow.Parse([]byte(`
[pipeline]
name    = "defaulting"
version = 1

[[step]]
name = "implement"
executor = "implement"
class = "write"
emits = "change-summary"
after = []
`))
	testsupport.Must(t, err, "parse: %v", err)
	err = workflow.Validate(def)
	testsupport.Must(t, err, "validate: %v", err)

	step := workflow.StepByName(def, "implement")
	if step.HoldsTree == nil {
		t.Fatal("holds_tree was left nil; the default must be MATERIALIZED so " +
			"no reader has to remember which way nil falls")
	}
	if !*step.HoldsTree {
		t.Error("holds_tree defaulted to false; an undeclared step must HOLD " +
			"the tree — guessing the other way lets two writers race one tree")
	}
}

// TestHoldsTreeFalseIsHonoured is the capability proper.
func TestHoldsTreeFalseIsHonoured(t *testing.T) {
	def, err := workflow.Parse([]byte(`
[pipeline]
name    = "reading"
version = 1

[[step]]
name = "review"
executor = "judge-correctness"
emits = "findings"
holds_tree = false
after = []
`))
	testsupport.Must(t, err, "parse: %v", err)
	err = workflow.Validate(def)
	testsupport.Must(t, err, "validate: %v", err)

	step := workflow.StepByName(def, "review")
	if step.HoldsTree == nil || *step.HoldsTree {
		t.Error("holds_tree = false was not carried through parse+validate")
	}
}

// TestHoldsTreeSurvivesSerialization pins that the flag reaches the PINNED
// form. The scheduler reads it from the pinned definition rather than a step
// column, so a value that did not survive the round trip would silently revert
// to the default at exactly the moment it matters.
func TestHoldsTreeSurvivesSerialization(t *testing.T) {
	def, err := workflow.Parse([]byte(`
[pipeline]
name    = "roundtrip"
version = 1

[[step]]
name = "review"
executor = "judge-correctness"
emits = "findings"
holds_tree = false
after = []
`))
	testsupport.Must(t, err, "parse: %v", err)
	err = workflow.Validate(def)
	testsupport.Must(t, err, "validate: %v", err)

	encoded, err := json.Marshal(def)
	testsupport.Must(t, err, "marshal: %v", err)
	var decoded workflow.Definition
	err = json.Unmarshal(encoded, &decoded)
	testsupport.Must(t, err, "unmarshal: %v", err)

	step := workflow.StepByName(&decoded, "review")
	if step.HoldsTree == nil || *step.HoldsTree {
		t.Errorf("holds_tree did not survive the pinned round trip: %v",
			step.HoldsTree)
	}
}
