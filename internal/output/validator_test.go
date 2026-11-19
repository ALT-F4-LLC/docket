package output

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
)

// debugCapture returns a Validator whose debug log lines land in the returned
// buffer, one text-handler line per record.
func debugCapture() (*Validator, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewValidator(logger), &buf
}

// withUnknownField decodes msg's wire form with one extra field appended
// under a number the v1 schema does not declare, the way a newer daemon's
// message arrives at this client.
func withUnknownField[M proto.Message](t *testing.T, msg M) M {
	t.Helper()
	wire, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire = protowire.AppendTag(wire, 999, protowire.BytesType)
	wire = protowire.AppendString(wire, "from a newer daemon")
	decoded := msg.ProtoReflect().New().Interface().(M)
	if err := proto.Unmarshal(wire, decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.ProtoReflect().GetUnknown()) == 0 {
		t.Fatal("fixture carries no unknown fields")
	}
	return decoded
}

func requireIncompatible(t *testing.T, err error) {
	t.Helper()
	var incompatible *IncompatibleError
	if !errors.As(err, &incompatible) {
		t.Fatalf("err = %v, want an *IncompatibleError", err)
	}
	if incompatible.Code() != ErrIncompatible {
		t.Fatalf("code = %q, want %q", incompatible.Code(), ErrIncompatible)
	}
}

func TestValidatorAcceptsDeclaredStateMachineValues(t *testing.T) {
	runStatuses := []string{"planning", "active", "waiting-human", "done", "abandoned"}
	stepStatuses := []string{
		"pending", "claimed", "running", "gated", "done", "waiting-human",
		"skipped", "superseded", "failed-routed", "ready", "staged",
	}
	verdicts := []string{"approve", "approve-with-concerns", "reject"}
	v, _ := debugCapture()

	for _, status := range runStatuses {
		if err := v.Validate(&docketv1.RunStatusResponse{Items: []*docketv1.Run{{Status: status}}}); err != nil {
			t.Errorf("run status %q: %v", status, err)
		}
	}
	for _, status := range stepStatuses {
		if err := v.Validate(&docketv1.StepListResponse{Items: []*docketv1.Step{{Status: status}}}); err != nil {
			t.Errorf("step status %q: %v", status, err)
		}
	}
	for _, verdict := range verdicts {
		resp := &docketv1.VoteShowResponse{Proposal: &docketv1.Proposal{Votes: []*docketv1.Vote{{Verdict: verdict}}}}
		if err := v.Validate(resp); err != nil {
			t.Errorf("verdict %q: %v", verdict, err)
		}
	}
}

func TestValidatorRejectsUndeclaredStateMachineValues(t *testing.T) {
	cases := map[string]proto.Message{
		"run status":   &docketv1.RunStatusResponse{Items: []*docketv1.Run{{Status: "archived"}}},
		"step status":  &docketv1.StepShowResponse{Steps: []*docketv1.Step{{Status: "quarantined"}}},
		"vote verdict": &docketv1.VoteCastResponse{Vote: &docketv1.Vote{Verdict: "abstain"}},
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			v, _ := debugCapture()
			requireIncompatible(t, v.Validate(msg))
		})
	}
}

func TestValidatorRejectsUnknownFieldsOnStrictMessages(t *testing.T) {
	cases := map[string]proto.Message{
		"run":  &docketv1.RunStartResponse{Run: withUnknownField(t, &docketv1.Run{Status: "active"})},
		"step": &docketv1.StepShowResponse{Steps: []*docketv1.Step{withUnknownField(t, &docketv1.Step{Status: "done"})}},
		"vote": &docketv1.VoteCastResponse{Vote: withUnknownField(t, &docketv1.Vote{Verdict: "approve"})},
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			v, _ := debugCapture()
			requireIncompatible(t, v.Validate(msg))
		})
	}
}

func TestValidatorToleratesUnknownEventKind(t *testing.T) {
	event := &docketv1.Event{Kind: "run-teleported"}
	v, _ := debugCapture()

	if err := v.Validate(&docketv1.EventsListResponse{Items: []*docketv1.Event{event}}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := DisplayEnum(event, "kind"); got != "unknown(run-teleported)" {
		t.Errorf("DisplayEnum = %q, want unknown(run-teleported)", got)
	}
	if got := DisplayEnum(&docketv1.Event{Kind: "step-claimed"}, "kind"); got != "step-claimed" {
		t.Errorf("DisplayEnum of a known kind = %q, want step-claimed", got)
	}
}

func TestValidatorToleratesUnknownDisplayFields(t *testing.T) {
	issue := &docketv1.Issue{Status: "parked", Priority: "urgent-ish", Kind: "spike"}
	v, _ := debugCapture()

	if err := v.Validate(&docketv1.IssueShowResponse{Issues: []*docketv1.IssueShowDetail{{Issue: issue}}}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for field, want := range map[string]string{
		"status":   "unknown(parked)",
		"priority": "unknown(urgent-ish)",
		"kind":     "unknown(spike)",
	} {
		if got := DisplayEnum(issue, protoreflect.Name(field)); got != want {
			t.Errorf("DisplayEnum(%s) = %q, want %q", field, got, want)
		}
	}
	known := &docketv1.Issue{Status: "backlog", Priority: "high", Kind: "bug"}
	for field, want := range map[string]string{"status": "backlog", "priority": "high", "kind": "bug"} {
		if got := DisplayEnum(known, protoreflect.Name(field)); got != want {
			t.Errorf("DisplayEnum(%s) of a known value = %q, want %q", field, got, want)
		}
	}
}

func TestValidatorIgnoresUnknownFieldsOnTolerantMessagesWithOneDebugLine(t *testing.T) {
	resp := withUnknownField(t, &docketv1.EventsListResponse{
		Items: []*docketv1.Event{
			withUnknownField(t, &docketv1.Event{Kind: "step-claimed"}),
			{Kind: "step-recorded"},
		},
	})
	v, logs := debugCapture()

	if err := v.Validate(resp); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("debug lines = %d, want one per message carrying unknown fields (2):\n%s", len(lines), logs.String())
	}
	for i, message := range []string{"docket.v1.EventsListResponse", "docket.v1.Event"} {
		if !strings.Contains(lines[i], "level=DEBUG") || !strings.Contains(lines[i], "message="+message) {
			t.Errorf("line %d = %q, want a DEBUG line naming %s", i, lines[i], message)
		}
	}
}
