package api

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Every fixture value differs from its siblings and from its proto3 default,
// so a conversion that drops a field or reads a neighbouring one fails the
// final proto.Equal.
func TestContextBundleRoundTrip(t *testing.T) {
	exit := 2
	claim := &engine.Context{
		Step: model.StepRow{
			Step: "STEP-7", Instance: "implement@0", Issue: "DKT-3", Run: "RUN-2",
			Kind: "executor", Executor: "implement", Class: "write",
			Labels: []string{"api"}, Size: "s",
			Model: "m-1", Effort: "high", Variant: "standard",
			Voters:           []string{"judge-a"},
			VoterAssignments: []model.VoterAssignment{{Voter: "judge-a", Model: "m-2", Effort: "low", Variant: "cheap"}},
			Proposal:         "PROP-4",
		},
		Issue: engine.ContextIssue{
			ID: "DKT-4", Title: "Carry the bundle", BodySnapshot: "frozen body\n",
			Kind: "task", Labels: []string{"wire"}, Scope: []string{"internal/api/**"},
		},
		Inputs: []engine.ContextInput{{
			Artifact: "ARTIFACT-9", Kind: "change-summary", ProducerStep: "plan@0",
			Body: "summary", Payload: `{"sha":"abc"}`,
		}},
		Pins:      []engine.ContextPin{{Path: "contracts/implement.md", SHA256: "deadbeef"}},
		LoopEntry: &engine.LoopEntry{Ordinal: 2, Routing: "fix-loop"},
		Metadata:  map[string]any{"seat": "implement"},
		PreGates: []engine.PreGateResult{{
			Gate: "build", Argv: []string{"bash", "scripts/qa/build.sh"}, Exit: &exit,
			DurationMS: 1500, Output: "compile error", Verdict: "fail",
		}},
		TargetSHA: "1111", TargetWorktree: "/wt/a", DiffBase: "0000",
		Resolution: &engine.ContextResolution{Routing: "retry", Note: "use the new base"},
		Notes: []engine.RunNote{
			{ID: 181, Text: "first note", Authority: "operator", RecordedAtMS: 10},
			{ID: 182, Text: "second note", Authority: "conductor", RecordedAtMS: 20},
		},
	}

	got := roundTripContextBundle(t, claim)

	if n := got.GetNotes(); len(n) != 2 || n[0].GetText() != "first note" || n[1].GetOrdinal() != 182 {
		t.Errorf("notes did not survive the round trip: %v", n)
	}
	if p := got.GetInputs(); len(p) != 1 || p[0].GetProducerStep() != "plan@0" {
		t.Errorf("producer step did not survive the round trip: %v", p)
	}
	if r := got.GetResolutionRecord(); r.GetRouting() != "retry" || r.GetNote() != "use the new base" {
		t.Errorf("resolution did not survive the round trip: %v", r)
	}
	if b := got.GetIssue().GetBodySnapshot(); b != "frozen body\n" {
		t.Errorf("body snapshot did not survive the round trip: %q", b)
	}

	want := &docketv1.StepContextBundle{
		Step: &docketv1.StepRow{
			Step: "STEP-7", Instance: "implement@0", Issue: "DKT-3", Run: "RUN-2",
			Kind: "executor", Executor: "implement", Class: "write",
			Labels: []string{"api"}, Size: "s",
			Model: "m-1", Effort: "high", Variant: "standard",
			Voters: []string{"judge-a"},
			VoterAssignments: []*docketv1.VoterAssignment{
				{Voter: "judge-a", Model: "m-2", Effort: "low", Variant: "cheap"},
			},
			Proposal: "PROP-4",
		},
		Issue: &docketv1.Issue{
			Id: "DKT-4", Title: "Carry the bundle", BodySnapshot: "frozen body\n",
			Kind: "task", Labels: []string{"wire"},
			Scope: &docketv1.ScopeGlobs{Globs: []string{"internal/api/**"}},
		},
		Inputs: []*docketv1.ContextInput{{
			Artifact: "ARTIFACT-9", Kind: "change-summary", ProducerStep: "plan@0",
			Body: "summary", PayloadJson: `{"sha":"abc"}`,
		}},
		Pins:         []*docketv1.ContextPin{{Path: "contracts/implement.md", Sha256: "deadbeef"}},
		LoopEntry:    &docketv1.LoopEntry{K: 2, Routing: "fix-loop"},
		MetadataJson: `{"seat":"implement"}`,
		PreGates: []*docketv1.GateResult{{
			Gate: "build", Command: "bash scripts/qa/build.sh", Status: "fail",
			ExitCode: 2, DurationMs: 1500, Output: "compile error", Pre: true,
		}},
		TargetSha: "1111", TargetWorktree: "/wt/a", DiffBase: "0000",
		ResolutionRecord: &docketv1.ContextResolution{Routing: "retry", Note: "use the new base"},
		Notes: []*docketv1.RunNote{
			{Ordinal: 181, Authority: "operator", Text: "first note", CreatedAtMs: 10},
			{Ordinal: 182, Authority: "conductor", Text: "second note", CreatedAtMs: 20},
		},
	}
	if !proto.Equal(got, want) {
		t.Errorf("round-tripped bundle differs from the engine context\n got: %v\nwant: %v", got, want)
	}
}

// A first-round claim has no resolution, loop entry, metadata, or notes; each
// stays unset on the wire rather than arriving as an empty message or "null".
func TestContextBundleLeavesAbsentSectionsUnset(t *testing.T) {
	claim := &engine.Context{
		Step:     model.StepRow{Step: "STEP-8"},
		Issue:    engine.ContextIssue{ID: "DKT-5"},
		PreGates: []engine.PreGateResult{{Gate: "vuln-scan", Verdict: "unmatched"}},
	}

	got := roundTripContextBundle(t, claim)

	if got.GetResolutionRecord() != nil {
		t.Errorf("resolution_record is set without a routing record: %v", got.GetResolutionRecord())
	}
	if got.GetLoopEntry() != nil {
		t.Errorf("loop_entry is set without a loop entry: %v", got.GetLoopEntry())
	}
	if m := got.GetMetadataJson(); m != "" {
		t.Errorf("metadata_json is %q without metadata", m)
	}
	if got.GetIssue().GetScope() != nil {
		t.Errorf("scope is set for an issue with no scope: %v", got.GetIssue().GetScope())
	}
	want := &docketv1.StepContextBundle{
		Step:     &docketv1.StepRow{Step: "STEP-8"},
		Issue:    &docketv1.Issue{Id: "DKT-5"},
		PreGates: []*docketv1.GateResult{{Gate: "vuln-scan", Status: "unmatched", Pre: true}},
	}
	if !proto.Equal(got, want) {
		t.Errorf("sparse bundle differs from the engine context\n got: %v\nwant: %v", got, want)
	}
}

func TestContextBundleKeepsDeclaredEmptyScope(t *testing.T) {
	claim := &engine.Context{Issue: engine.ContextIssue{ID: "DKT-6", Scope: []string{}}}

	got := roundTripContextBundle(t, claim)

	if s := got.GetIssue().GetScope(); s == nil || len(s.GetGlobs()) != 0 {
		t.Errorf("an empty declared scope must arrive present with no globs, got %v", s)
	}
}

func TestContextBundleRejectsUnencodableMetadata(t *testing.T) {
	claim := &engine.Context{Metadata: map[string]any{"seat": make(chan int)}}

	bundle, err := ContextBundle(claim)

	if err == nil {
		t.Fatalf("ContextBundle encoded unencodable metadata: %v", bundle)
	}
	if bundle != nil {
		t.Errorf("ContextBundle returned a bundle with its error: %v", bundle)
	}
}

func roundTripContextBundle(t *testing.T, claim *engine.Context) *docketv1.StepContextBundle {
	t.Helper()
	bundle, err := ContextBundle(claim)
	if err != nil {
		t.Fatalf("ContextBundle: %v", err)
	}
	wire, err := proto.Marshal(bundle)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	got := &docketv1.StepContextBundle{}
	if err := proto.Unmarshal(wire, got); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	return got
}
