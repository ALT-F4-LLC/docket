package workflow

import (
	"strings"
	"testing"
)

// onFailVoteSrc is one workflow whose `implement` step routes its failure to
// the named step. `triage` is the vote panel, `review` an ordinary executor, so
// the three cases below differ only in the value of `on_fail`.
const onFailVoteSrc = `
[pipeline]
name = "w"
version = 1
[[step]]
name = "implement"
executor = "x"
emits = "k"
after = []
on_fail = "TARGET"
[[step]]
name = "triage"
type = "vote"
voters = ["a"]
on_fail = "abandon-issue"
after = ["implement"]
vote_rule = "standard"
[step.on_fail_routes]
approved = "retry"
rejected = "abandon-issue"
[[step]]
name = "review"
executor = "y"
emits = "r"
after = ["implement"]
`

// TestOnFailMayNameAVoteStep is DKT-1901's criterion 1: `on_fail` may name a
// `type = "vote"` step in the same workflow, while a name that is not a vote
// step is still refused with a V-rule id.
func TestOnFailMayNameAVoteStep(t *testing.T) {
	t.Run("a vote step name is accepted", func(t *testing.T) {
		if err := Validate(parseOnFail(t, "triage")); err != nil {
			t.Fatalf("on_fail naming the vote step must validate, got %v", err)
		}
	})

	t.Run("an executor step name is rejected", func(t *testing.T) {
		assertOnFailRefused(t, Validate(parseOnFail(t, "review")), "review")
	})

	t.Run("an unknown name is rejected", func(t *testing.T) {
		assertOnFailRefused(t, Validate(parseOnFail(t, "nobody")), "nobody")
	})
}

func parseOnFail(t *testing.T, target string) *Definition {
	t.Helper()
	def, err := Parse([]byte(strings.Replace(onFailVoteSrc, "TARGET", target, 1)))
	if err != nil {
		t.Fatalf("parsing the definition with on_fail = %q: %v", target, err)
	}
	return def
}

// assertOnFailRefused pins the rejection's SHAPE rather than one rule's prose:
// the error names the offending value and carries a V-rule id, which is what
// makes the refusal actionable in an operator's terminal.
func assertOnFailRefused(t *testing.T, err error, value string) {
	t.Helper()
	if err == nil {
		t.Fatalf("on_fail = %q must be refused, got no error", value)
	}
	if !strings.Contains(err.Error(), value) {
		t.Errorf("the refusal must name %q, got %q", value, err)
	}
	var werr *Error
	if !asWorkflowError(err, &werr) || werr.Rule == "" {
		t.Errorf("the refusal must carry a V-rule id, got %q", err)
	}
}
