package output

import (
	"fmt"
	"log/slog"
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// IncompatibleError reports a daemon message this client cannot interpret
// safely: a state-machine value outside the set it knows, or fields it does
// not know on a message that carries one.
type IncompatibleError struct {
	Message protoreflect.FullName
	Reason  string
}

func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("incompatible daemon response: %s %s", e.Message, e.Reason)
}

// Code is the docket error code the client reports for the failure.
func (e *IncompatibleError) Code() ErrorCode { return ErrIncompatible }

// stringEnum is a proto string field whose values form a closed set this
// client knows. A strict field drives a state machine, so an unknown value
// fails the message; a tolerant one is only displayed.
type stringEnum struct {
	label  string
	strict bool
	known  func(string) bool
}

var stepStatuses = []string{
	db.StepPending, db.StepClaimed, db.StepRunning, db.StepGated, db.StepDone,
	db.StepWaitingHuman, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
	db.StepReady, db.StepStaged,
}

var eventKinds = engine.EventKinds()

var stringEnums = map[protoreflect.FullName]stringEnum{
	fieldName(&docketv1.Run{}, "status"): {"run status", true, func(s string) bool {
		return model.ValidateRunStatus(model.RunStatus(s)) == nil
	}},
	fieldName(&docketv1.Step{}, "status"): {"step status", true, func(s string) bool {
		return slices.Contains(stepStatuses, s)
	}},
	fieldName(&docketv1.Vote{}, "verdict"): {"vote verdict", true, func(s string) bool {
		return model.ValidateVerdict(model.Verdict(s)) == nil
	}},
	fieldName(&docketv1.Event{}, "kind"): {"event kind", false, func(s string) bool {
		return slices.Contains(eventKinds, s)
	}},
	fieldName(&docketv1.Issue{}, "status"): {"issue status", false, func(s string) bool {
		return model.ValidateStatus(model.Status(s)) == nil
	}},
	fieldName(&docketv1.Issue{}, "priority"): {"issue priority", false, func(s string) bool {
		return model.ValidatePriority(model.Priority(s)) == nil
	}},
	fieldName(&docketv1.Issue{}, "kind"): {"issue kind", false, func(s string) bool {
		return model.ValidateIssueKind(model.IssueKind(s)) == nil
	}},
}

// strictMessages are the messages declaring a strict field. Fields this
// client does not know on one of them may change what its state means.
var strictMessages = func() map[protoreflect.FullName]bool {
	out := map[protoreflect.FullName]bool{}
	for name, enum := range stringEnums {
		if enum.strict {
			out[name.Parent()] = true
		}
	}
	return out
}()

// fieldName resolves a field through the generated descriptor, so a renamed
// or removed field fails at package init instead of silently going unchecked.
func fieldName(msg proto.Message, name protoreflect.Name) protoreflect.FullName {
	fd := msg.ProtoReflect().Descriptor().Fields().ByName(name)
	if fd == nil {
		panic(fmt.Sprintf("output: %s has no field %q", msg.ProtoReflect().Descriptor().FullName(), name))
	}
	return fd.FullName()
}

// Validator checks a decoded daemon message against the values and fields
// this client knows: strict on state-machine values (run status, step
// status, vote verdict) and on unknown fields of the messages carrying them,
// tolerant everywhere else.
type Validator struct {
	logger *slog.Logger
}

// NewValidator returns a Validator that logs ignored unknown fields to logger
// at debug level.
func NewValidator(logger *slog.Logger) *Validator {
	return &Validator{logger: logger}
}

// Validate walks msg and every message nested in it. It returns an
// *IncompatibleError for a strict field holding an unknown value or a strict
// message carrying unknown fields, and logs one debug line for each other
// message carrying unknown fields.
func (v *Validator) Validate(msg proto.Message) error {
	return v.validateMessage(msg.ProtoReflect())
}

func (v *Validator) validateMessage(m protoreflect.Message) error {
	name := m.Descriptor().FullName()
	if unknown := m.GetUnknown(); len(unknown) > 0 {
		if strictMessages[name] {
			return &IncompatibleError{
				Message: name,
				Reason:  fmt.Sprintf("carries %d bytes of fields this client does not know", len(unknown)),
			}
		}
		v.logger.Debug("ignoring unknown fields", "message", string(name), "bytes", len(unknown))
	}
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		err = v.validateField(fd, value)
		return err == nil
	})
	return err
}

func (v *Validator) validateField(fd protoreflect.FieldDescriptor, value protoreflect.Value) error {
	switch {
	case fd.IsMap():
		if fd.MapValue().Message() == nil {
			return nil
		}
		var err error
		value.Map().Range(func(_ protoreflect.MapKey, entry protoreflect.Value) bool {
			err = v.validateMessage(entry.Message())
			return err == nil
		})
		return err
	case fd.IsList():
		if fd.Message() == nil {
			return nil
		}
		list := value.List()
		for i := range list.Len() {
			if err := v.validateMessage(list.Get(i).Message()); err != nil {
				return err
			}
		}
		return nil
	case fd.Message() != nil:
		return v.validateMessage(value.Message())
	}
	enum, ok := stringEnums[fd.FullName()]
	if ok && enum.strict && !enum.known(value.String()) {
		return &IncompatibleError{
			Message: fd.ContainingMessage().FullName(),
			Reason:  fmt.Sprintf("has %s %q, which this client does not know", enum.label, value.String()),
		}
	}
	return nil
}

// DisplayEnum renders the named string field of msg for display: the value as
// sent when this client knows it, unknown(<value>) when the field has a
// closed value set the value falls outside.
func DisplayEnum(msg proto.Message, field protoreflect.Name) string {
	m := msg.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(field)
	value := m.Get(fd).String()
	enum, ok := stringEnums[fd.FullName()]
	if ok && value != "" && !enum.known(value) {
		return "unknown(" + value + ")"
	}
	return value
}
