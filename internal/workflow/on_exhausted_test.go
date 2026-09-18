package workflow

import (
	"strings"
	"testing"
)

// onExhaustedSrc is one workflow whose `check` step declares `max_fix_loops`
// and routes its exhaustion to TARGET. `panel` is a vote step, `drain` an
// ordinary executor, so the cases below differ only in `on_exhausted`.
const onExhaustedSrc = `
[pipeline]
name = "w"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
executor = "check"
emits = "findings"
threshold = { "fix-loop" = "any(status == unmet)" }
max_fix_loops = 2
on_exhausted = "TARGET"

[[step]]
name = "fix"
executor = "fix"
emits = "findings"
loop = true
after_loop = "check"

[[step]]
name = "panel"
type = "vote"
voters = ["a"]
vote_rule = "standard"
on_fail = "abandon-issue"
after = ["check"]

[[step]]
name = "drain"
executor = "drain"
emits = "notes"
after = ["check"]
`

// TestOnExhaustedVocabulary is criterion 1: a step declaring `max_fix_loops`
// may declare `on_exhausted` naming `waiting-human`, `abandon-issue`, a
// `type = "vote"` step, or an executor step of the same workflow; anything
// else is refused with a V-rule id.
func TestOnExhaustedVocabulary(t *testing.T) {
	for _, accepted := range []string{
		OnFailWaitingHuman, OnFailAbandonIssue, "panel", "drain",
	} {
		t.Run("accepts "+accepted, func(t *testing.T) {
			def := parseOnExhausted(t, accepted)
			if err := Validate(def); err != nil {
				t.Fatalf("on_exhausted = %q must validate, got %v", accepted, err)
			}
			want := interposedOnExhausted(accepted)
			if got := StepByName(def, "check").OnExhaustedTarget(); got != want {
				t.Errorf("OnExhaustedTarget() = %q, want %q", got, want)
			}
		})
	}

	t.Run("rejects an unknown step name", func(t *testing.T) {
		err := Validate(parseOnExhausted(t, "nobody"))
		if err == nil {
			t.Fatal(`on_exhausted = "nobody" must be refused, got no error`)
		}
		if !strings.Contains(err.Error(), "nobody") {
			t.Errorf("the refusal must name the offending value, got %q", err)
		}
		var werr *Error
		if !asWorkflowError(err, &werr) || werr.Rule == "" {
			t.Errorf("the refusal must carry a V-rule id, got %q", err)
		}
	})
}

// interposedOnExhausted is the interposed target an accepted value implies: a
// closed-verb value names no step, a step name names itself.
func interposedOnExhausted(value string) string {
	if value == OnFailWaitingHuman || value == OnFailAbandonIssue {
		return ""
	}
	return value
}

func parseOnExhausted(t *testing.T, target string) *Definition {
	t.Helper()
	def, err := Parse([]byte(strings.Replace(onExhaustedSrc, "TARGET", target, 1)))
	if err != nil {
		t.Fatalf("parsing the definition with on_exhausted = %q: %v", target, err)
	}
	return def
}
