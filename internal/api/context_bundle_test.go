package api

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

func TestContextBundleRoundTrip(t *testing.T) {
	exit := 0
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
			ID: "DKT-3", Title: "Carry the bundle", BodySnapshot: "frozen body\n",
			Kind: "task", Labels: []string{"api"}, Scope: []string{"internal/api/**"},
		},
		Inputs: []engine.ContextInput{{
			Artifact: "ARTIFACT-9", Kind: "change-summary", ProducerStep: "implement@0",
			Body: "summary", Payload: `{"sha":"abc"}`,
		}},
		Pins:      []engine.ContextPin{{Path: "contracts/implement.md", SHA256: "deadbeef"}},
		LoopEntry: &engine.LoopEntry{Ordinal: 2, Routing: "fix-loop"},
		Metadata:  map[string]any{"seat": "implement"},
		PreGates: []engine.PreGateResult{{
			Gate: "build", Argv: []string{"bash", "scripts/qa/build.sh"}, Exit: &exit,
			DurationMS: 1500, Output: "ok", Verdict: "pass",
		}},
		TargetSHA: "1111", TargetWorktree: "/wt/a", DiffBase: "0000",
		Resolution: &engine.ContextResolution{Routing: "retry", Note: "use the new base"},
		Notes: []engine.RunNote{
			{ID: 181, Text: "first note", Authority: "operator", RecordedAtMS: 10},
			{ID: 182, Text: "second note", Authority: "conductor", RecordedAtMS: 20},
		},
	}

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

	if n := got.GetNotes(); len(n) != 2 || n[0].GetText() != "first note" || n[1].GetOrdinal() != 182 {
		t.Errorf("notes did not survive the round trip: %v", n)
	}
	if p := got.GetInputs(); len(p) != 1 || p[0].GetProducerStep() != "implement@0" {
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
			Id: "DKT-3", Title: "Carry the bundle", BodySnapshot: "frozen body\n",
			Kind: "task", Labels: []string{"api"},
			Scope: &docketv1.ScopeGlobs{Globs: []string{"internal/api/**"}},
		},
		Inputs: []*docketv1.ContextInput{{
			Artifact: "ARTIFACT-9", Kind: "change-summary", ProducerStep: "implement@0",
			Body: "summary", PayloadJson: `{"sha":"abc"}`,
		}},
		Pins:         []*docketv1.ContextPin{{Path: "contracts/implement.md", Sha256: "deadbeef"}},
		LoopEntry:    &docketv1.LoopEntry{K: 2, Routing: "fix-loop"},
		MetadataJson: `{"seat":"implement"}`,
		PreGates: []*docketv1.GateResult{{
			Gate: "build", Command: "bash scripts/qa/build.sh", Status: "pass",
			ExitCode: 0, DurationMs: 1500, Output: "ok", Pre: true,
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
