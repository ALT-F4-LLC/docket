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

	// The remaining V41 refusals, each reached by one edit to the parsed
	// definition and identified by its own message.
	for _, tc := range []struct {
		name, target string
		edit         func(def *Definition)
		want         string
	}{
		{
			name: "rejects a step that cannot route fix-loop", target: OnFailWaitingHuman,
			edit: func(def *Definition) { StepByName(def, "drain").OnExhausted = OnFailWaitingHuman },
			want: "is only valid on a step that routes",
		},
		{
			name: "rejects a missing max_fix_loops", target: OnFailWaitingHuman,
			edit: func(def *Definition) { StepByName(def, "check").MaxFixLoops = nil },
			want: "requires a positive `max_fix_loops`",
		},
		{
			name: "rejects a step that is neither vote nor executor", target: "drain",
			edit: func(def *Definition) {
				drain := StepByName(def, "drain")
				drain.Executor, drain.Type, drain.OnFail = "", TypeHuman, OnFailAbandonIssue
			},
			want: "neither a `type=\"vote\"` step nor an executor",
		},
		{
			name: "rejects a step not ordered after the router", target: "drain",
			edit: func(def *Definition) { StepByName(def, "drain").After = nil },
			want: "whose `after` does not include",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := parseOnExhausted(t, tc.target)
			tc.edit(def)
			err := Validate(def)
			var werr *Error
			if !asWorkflowError(err, &werr) {
				t.Fatalf("want a *workflow.Error, got %v", err)
			}
			if werr.Rule != "V41" || werr.Field != "on_exhausted" {
				t.Errorf("Rule, Field = %q, %q, want V41, on_exhausted (%v)", werr.Rule, werr.Field, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal must say %q, got %q", tc.want, err)
			}
		})
	}
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
