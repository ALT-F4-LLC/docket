package workflow

import "strings"

// Error is a register-time failure: a grammar error, a validation-table row
// (V1-V25 incl. V13a), or a lint (L1-L4). Every one maps to VALIDATION_ERROR
// (exit 3) at the CLI boundary (TDD §4.5).
//
// Rule carries the documented rule ID. It is what
// TestValidationTableIsComplete asserts set equality over, so every rule the
// table documents has a test and every test names a documented rule — a
// validation table that drifts from its tests documents behavior the code does
// not have.
type Error struct {
	Rule     string // "V13a", "L2", "PARSE"
	Workflow string
	Step     string
	Field    string
	Message  string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Workflow != "" {
		b.WriteString("workflow ")
		b.WriteString(e.Workflow)
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	return b.String()
}

// withWorkflow stamps the workflow name onto an error raised before the name
// was known to the caller. Every register-time error names the workflow, the
// step, and the offending field (TDD §4.3).
func withWorkflow(err error, name string) error {
	if e, ok := err.(*Error); ok && e.Workflow == "" {
		e.Workflow = name
	}
	return err
}
