package engine

import (
	"database/sql"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The seam between a run's PINNED payload schema and the threshold evaluator.
//
// Everything here is a read of what the run pinned. Reading the live registry
// would make a routing decision depend on when `complete` happened to run, and
// determinism requires the opposite: the same run at the same pins routes the same
// way over the same payload.

// schemaOrder adapts a compiled schema to the evaluator's OrderResolver.
//
// It is an adapter rather than a method set on schema.Registered because the
// ordering API a threshold needs is deliberately narrower than what a registered
// document knows: Position, whether a field is ordered, and the declared type.
// Nothing here can hand out an order's values, which is what keeps `Position` the
// only way to learn anything about rank.
type schemaOrder struct{ reg *schema.Registered }

func (s schemaOrder) Position(field, value string) (int, bool) {
	return s.reg.Position(field, value)
}

func (s schemaOrder) Ordered(field string) bool {
	return s.reg.Ordered.Ordered(field)
}

func (s schemaOrder) FieldType(field string) string {
	f, ok := s.reg.Field(field)
	if !ok {
		return ""
	}
	return f.Type
}

func (s schemaOrder) Conservative(field string) string {
	f, ok := s.reg.Field(field)
	if !ok {
		return ""
	}
	return f.Conservative
}

var _ OrderResolver = schemaOrder{}

// stepOrder returns the OrderResolver a step's threshold evaluates under, or
// NIL when the step declares no `payload`.
//
// Nil is the schema-free case — the original behavior, unchanged — and returning
// it rather than an empty resolver is what makes "this step declares no schema" a
// distinct answer from "this step's schema declares no order on that field". The
// two need different sentences in a park's reason, and an empty resolver would
// collapse them.
//
// A step whose `payload` is declared but whose pinned schema cannot be read is a
// REFUSAL, not a silent fall-back to nil. Falling back would evaluate an ordered
// comparison as a park with the wrong reason, or worse, evaluate an equality
// under schema-free rules while the operator believes a schema is in force.
func stepOrder(conn *sql.DB, step *db.Step, spec *workflow.Step) (OrderResolver, error) {
	if spec == nil || spec.Payload == "" {
		return nil, nil
	}
	registered, err := pinnedSchema(conn, step.RunID, spec.Payload)
	if err != nil {
		return nil, err
	}
	return schemaOrder{reg: registered}, nil
}
