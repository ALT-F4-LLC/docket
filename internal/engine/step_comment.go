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
// carriage return rewrites what a terminal shows), any other character git and
// terminals treat as non-printing (format characters such as bidirectional
// overrides and zero-width characters, line and paragraph separators, private
// use and unassigned code points), and the characters that render as blank
// though they are printable by category (Hangul fillers, the braille blank,
// the combining grapheme joiner, Mongolian variation selectors). Ordinary
// spaces pass, and so does a carriage return that ends a CRLF line, which
// moves the cursor nowhere the newline after it does not.
//
// The joiners and selectors ordinary text is spelled with are the exception
// to the format-character rule, in one position only: inside a word, after a
// visible character. Persian and Urdu use the zero-width non-joiner between
// letters, Indic conjuncts and emoji sequences the zero-width joiner, the
// variation selectors pick a glyph ("⚠︎", "⚠️", an ideographic variant), and
// a soft hyphen or word joiner marks a break. At the start of the text, after
// whitespace or after another such character they draw as nothing, and a body
// of only such characters would pass the empty check, so there they are
// refused.
func unsafeCommentRune(s string) (rune, bool) {
	// prev is the rune before r; inWord reports whether it was accepted as part
	// of a word: a visible character, or a joiner or selector after one.
	prev, inWord := rune(-1), false
	for i, r := range s {
		switch {
		case r == '\n' || r == '\t' || (r == '\r' && strings.HasPrefix(s[i+1:], "\n")):
			inWord = false
		case joinerOrSelector(r):
			// After another joiner or selector only the zero-width joiner, and
			// only after a variation selector: "❤️‍🔥" is a heart, its
			// presentation selector, the joiner, then the fire.
			if !inWord || (joinerOrSelector(prev) && !(r == 0x200D && variationSelector(prev))) {
				return r, true
			}
		case !unicode.IsPrint(r) && !unicode.Is(unicode.Zs, r), blankRender(r):
			return r, true
		default:
			inWord = !unicode.IsSpace(r)
		}
		prev = r
	}
	return 0, false
}

// joinerOrSelector reports a format character or variation selector that
// ordinary text carries inside a word: the zero-width non-joiner and joiner,
// the soft hyphen, the word joiner, and the variation selectors.
func joinerOrSelector(r rune) bool {
	return r == 0x200C || r == 0x200D || r == 0x00AD || r == 0x2060 || variationSelector(r)
}

// variationSelector reports a standardized or ideographic variation selector.
func variationSelector(r rune) bool {
	return (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0xE0100 && r <= 0xE01EF)
}

// blankRender reports a printable-by-category character that draws as nothing.
func blankRender(r rune) bool {
	switch {
	case r == 0x034F, r == 0x115F, r == 0x1160, r == 0x17B4, r == 0x17B5,
		r == 0x2800, r == 0x3164, r == 0xFFA0:
		return true
	case r >= 0x180B && r <= 0x180D, r == 0x180F, r == 0x16FE4, r == 0x1D159:
		return true
	}
	return false
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
