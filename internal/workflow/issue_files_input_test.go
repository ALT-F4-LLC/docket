package workflow

import (
	"strings"
	"testing"
)

func issueFilesInputSrc(input string) string {
	return `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "doc"
[[step]]
name = "b"
after = ["a"]
executor = "y"
emits = "findings"
inputs = ["issue.body", "` + input + `"]
`
}

// TestInputsAcceptIssueFiles pins `issue.files` as a member of the engine-produced
// input vocabulary (DKT-44), and pins that a near-miss spelling is refused with a
// message naming the real form rather than reported as an unknown producer step.
func TestInputsAcceptIssueFiles(t *testing.T) {
	t.Run("issue.files is accepted", func(t *testing.T) {
		def, err := Parse([]byte(issueFilesInputSrc("issue.files")))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if err := Validate(def); err != nil {
			t.Fatalf("Validate rejected `issue.files`: %v", err)
		}
		if err := Lint(def); err != nil {
			t.Fatalf("Lint rejected `issue.files`: %v", err)
		}
	})

	t.Run("issue.attachments is refused naming issue.files", func(t *testing.T) {
		def, err := Parse([]byte(issueFilesInputSrc("issue.attachments")))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		err = Validate(def)
		if err == nil {
			t.Fatal("Validate accepted `issue.attachments`; want a V11 refusal")
		}
		if !strings.Contains(err.Error(), "issue.files") {
			t.Errorf("refusal does not name `issue.files`:\n  %v", err)
		}
		if !strings.Contains(err.Error(), "issue.attachments") {
			t.Errorf("refusal does not name the offending entry:\n  %v", err)
		}
	})
}
