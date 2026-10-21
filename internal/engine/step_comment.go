package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// IssueCommentArtifactKind is the auxiliary kind a step may record beside its
// declared emit when its acceptance criterion requires an issue comment
// (DKT-4272). Like GapArtifactKind it is packaging vocabulary, always
// emittable, and needs no workflow declaration.
//
// The engine posts the body as a comment on the step's OWN issue, in the same
// transaction as the recorded artifact. An executor in an isolated worktree
// then never runs `docket issue comment add` itself, which the harness refuses
// there.
//
// THE TARGET IS THE STEP'S ISSUE AND NOTHING ELSE. A recorded artifact is
// executor-authored text, and letting it name another issue would let any
// step write into any issue's thread. The body is capped and the count per
// step is capped, so a step cannot flood a thread either.
const IssueCommentArtifactKind = "issue-comment"

// Caps on the comments one step may record. The body cap is the artifact cap's
// order of magnitude on purpose: a comment is prose for a person, and a record
// larger than this belongs in an artifact the comment cites.
const (
	StepCommentMaxBytes = 16 * 1024
	StepCommentMaxCount = 4
)

// validateStepComments refuses an unusable comment set before the transaction
// opens, so a refusal spends nothing.
func validateStepComments(step *db.Step, comments [][]byte) error {
	if len(comments) == 0 {
		return nil
	}
	if step.IssueID == 0 {
		return validationErr(
			"step %s has no issue, so there is nothing for a comment to attach to",
			step.Instance)
	}
	if len(comments) > StepCommentMaxCount {
		return validationErr(
			"%d comments recorded; a step may record at most %d",
			len(comments), StepCommentMaxCount)
	}
	for i, body := range comments {
		if len(body) > StepCommentMaxBytes {
			return validationErr(
				"comment %d is %d bytes, over the %d-byte cap; cite a recorded "+
					"artifact instead", i+1, len(body), StepCommentMaxBytes)
		}
		if !utf8.Valid(body) {
			return validationErr("comment %d is not valid UTF-8", i+1)
		}
		if r, bad := unsafeCommentRune(string(body)); bad {
			return validationErr(
				"comment %d holds the control or formatting character U+%04X, which "+
					"could rewrite a reader's terminal or pass for a different author; "+
					"only printable text, newlines and tabs are posted", i+1, r)
		}
		if strings.TrimSpace(string(body)) == "" {
			return validationErr("comment %d is empty; it would post nothing", i+1)
		}
	}
	return nil
}

// unsafeCommentRune finds the first character a comment may not carry: a
// control character other than newline and tab (an escape sequence or a bare
// carriage return rewrites what a terminal shows), and any Unicode format
// character (bidirectional overrides and zero-width characters reorder or hide
// text, and a body of only those would pass the empty check).
func unsafeCommentRune(s string) (rune, bool) {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return r, true
		}
	}
	return 0, false
}

// stepCommentKey is the idempotency key for the comment one recorded artifact
// posts. It names the artifact, so the same artifact can never post twice and
// two artifacts with identical bodies each post once.
func stepCommentKey(artifactID int) string {
	return fmt.Sprintf("step-comment:%d", artifactID)
}

// postStepCommentTx posts a recorded issue-comment artifact as a comment on the
// step's issue, attributed to the step. The key names an artifact minted in
// this same transaction, in a scope no verb accepts a caller's key for, so a
// key already recorded is never legitimate: it refuses the recording rather
// than letting the artifact record with its comment skipped.
func postStepCommentTx(tx *sql.Tx, step *db.Step, artifactID int, body []byte) error {
	_, inserted, err := db.CreateCommentIdempotentTx(tx, &model.Comment{
		IssueID: step.IssueID,
		Body:    string(body),
		Author:  model.FormatStepID(step.ID),
	}, db.ScopeStepComment, stepCommentKey(artifactID))
	if err != nil {
		return fmt.Errorf("posting the comment %s recorded: %w", step.Instance, err)
	}
	if !inserted {
		return fmt.Errorf("posting the comment %s recorded: idempotency key %q is already "+
			"recorded, so the comment would not post; refusing the recording",
			step.Instance, stepCommentKey(artifactID))
	}
	return nil
}
