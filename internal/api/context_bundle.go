package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// ContextBundle converts the context bundle the engine assembles at claim into
// its v1 wire form, so a daemon claim carries it without dropping data.
//
// The v1 schema has no slot for a step row's attempt counters, a pre-gate's
// truncation and stub flags or reason, or a resolution's gate outcomes; those
// do not cross the wire. A pre-gate's argv travels space-joined as its command.
// A pre-gate that produced no exit status carries exit_code 0, the same as a
// clean exit; its status still names the verdict. A nil issue scope leaves the
// scope message unset, while an empty non-nil scope sets it with no globs.
func ContextBundle(c *engine.Context) (*docketv1.StepContextBundle, error) {
	bundle := &docketv1.StepContextBundle{
		Step: stepRowMessage(c.Step),
		Issue: &docketv1.Issue{
			Id:           c.Issue.ID,
			Title:        c.Issue.Title,
			BodySnapshot: c.Issue.BodySnapshot,
			Kind:         c.Issue.Kind,
			Labels:       c.Issue.Labels,
		},
		TargetSha:      c.TargetSHA,
		TargetWorktree: c.TargetWorktree,
		DiffBase:       c.DiffBase,
	}
	if c.Issue.Scope != nil {
		bundle.Issue.Scope = &docketv1.ScopeGlobs{Globs: c.Issue.Scope}
	}
	for _, in := range c.Inputs {
		bundle.Inputs = append(bundle.Inputs, &docketv1.ContextInput{
			Artifact:     in.Artifact,
			Kind:         in.Kind,
			ProducerStep: in.ProducerStep,
			Body:         in.Body,
			PayloadJson:  in.Payload,
		})
	}
	for _, pin := range c.Pins {
		bundle.Pins = append(bundle.Pins, &docketv1.ContextPin{Path: pin.Path, Sha256: pin.SHA256})
	}
	if c.LoopEntry != nil {
		bundle.LoopEntry = &docketv1.LoopEntry{K: int32(c.LoopEntry.Ordinal), Routing: c.LoopEntry.Routing}
	}
	if len(c.Metadata) > 0 {
		metadata, err := json.Marshal(c.Metadata)
		if err != nil {
			return nil, fmt.Errorf("encode context metadata: %w", err)
		}
		bundle.MetadataJson = string(metadata)
	}
	for _, g := range c.PreGates {
		gate := &docketv1.GateResult{
			Gate:       g.Gate,
			Command:    strings.Join(g.Argv, " "),
			Status:     g.Verdict,
			DurationMs: g.DurationMS,
			Output:     g.Output,
			Pre:        true,
		}
		if g.Exit != nil {
			gate.ExitCode = int32(*g.Exit)
		}
		bundle.PreGates = append(bundle.PreGates, gate)
	}
	if c.Resolution != nil {
		bundle.ResolutionRecord = &docketv1.ContextResolution{Routing: c.Resolution.Routing, Note: c.Resolution.Note}
	}
	for _, note := range c.Notes {
		bundle.Notes = append(bundle.Notes, &docketv1.RunNote{
			Ordinal:     int32(note.ID),
			Authority:   note.Authority,
			Text:        note.Text,
			CreatedAtMs: note.RecordedAtMS,
		})
	}
	return bundle, nil
}

func stepRowMessage(row model.StepRow) *docketv1.StepRow {
	msg := &docketv1.StepRow{
		Step:     row.Step,
		Instance: row.Instance,
		Issue:    row.Issue,
		Run:      row.Run,
		Kind:     row.Kind,
		Executor: row.Executor,
		Class:    row.Class,
		Labels:   row.Labels,
		Size:     row.Size,
		Voters:   row.Voters,
		Model:    row.Model,
		Effort:   row.Effort,
		Variant:  row.Variant,
		Proposal: row.Proposal,
	}
	for _, v := range row.VoterAssignments {
		msg.VoterAssignments = append(msg.VoterAssignments, &docketv1.VoterAssignment{
			Voter: v.Voter, Model: v.Model, Effort: v.Effort, Variant: v.Variant,
		})
	}
	return msg
}
