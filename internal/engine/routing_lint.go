package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// THE SECURITY-ROUTING LINT (DKT-4275).
//
// Which issues reach the security-labelled review track is decided by a label,
// and a label is only as good as the planner's memory of it. In one run, eight
// issues about bearer tokens, DSNs, sockets and a trust boundary were bound to
// the ordinary pipeline; two activation panels rejected them before an operator
// relabelled, at the cost of an operator question and several extra seats.
//
// `run activate --dry-run` therefore lists every bound issue that lacks the
// security label yet looks security-shaped, before any seat is spent. It is a
// WARNING and nothing more. It changes no routing and writes nothing: whether a
// flagged issue is security-sensitive is the operator's answer, asked in the
// planning batch, and an engine that decided it would be deciding a security
// boundary.
//
// An issue is flagged on either of two grounds:
//
//   - its scope overlaps the scope of a security-labelled issue in the same run,
//     since two issues editing the same paths were planned as one concern;
//   - its title, scope or body contains a term of `routing.security_lexicon`.
//
// The lexicon is CONFIGURATION and ships empty. The engine attaches no domain
// meaning to any word, so the vocabulary belongs to the instance that knows its
// own security surface. The label name is `routing.security_label`; setting it
// empty turns the lint off.
//
// WHAT THE LINT DOES NOT SEE. An empty list is not a clearance. The overlap
// check compares the issues' DECLARED scope strings after taking each glob's
// literal prefix: no path normalization (`./x`, `a/../b`, case), no writer diff,
// no symlink targets, and only security-labelled issues bound in the same run.
// The lexicon is a case-folded substring match on the title, scope and body. A
// scope with no literal prefix (`**`, `{a,b}/**`) is skipped. The report below
// says how many issues each of those left unchecked, so a reader can tell a
// clean run from a lint that had nothing to compare against.

// RoutingLintReport states what a dry run's lint was able to check. It is
// present on every dry run, so "no warnings" is distinguishable from "nothing
// was checkable".
type RoutingLintReport struct {
	// Label is the configured security label; empty means the lint is off.
	Label string `json:"label"`
	// LexiconTerms is the number of configured lexicon terms.
	LexiconTerms int `json:"lexicon_terms"`
	// LabelledIssues is the number of bound issues carrying the label whose
	// scope the overlap check can compare (a glob with a literal prefix): the
	// only issues an overlap can be found against.
	LabelledIssues int `json:"labelled_issues"`
	// Linted is the number of bound issues without the label that were checked.
	Linted int `json:"linted"`
	// Unscoped is the number of linted issues whose scope held no glob with a
	// literal prefix, so the overlap check could not apply to them.
	Unscoped int `json:"unscoped"`
}

// CheckedNothing reports a lint that was off, or had no lexicon and nothing to
// compare scopes against (no comparable labelled issue, or no linted issue with a
// comparable scope): its empty result carries no information.
func (r RoutingLintReport) CheckedNothing() bool {
	return r.Label == "" ||
		(r.LexiconTerms == 0 && (r.LabelledIssues == 0 || r.Unscoped == r.Linted))
}

// RoutingWarning is one bound issue the lint flagged.
type RoutingWarning struct {
	Issue string `json:"issue"`
	// Label is the security label the issue lacks.
	Label string `json:"label"`
	// Overlaps lists the security-labelled issues whose scope this issue's scope
	// overlaps.
	Overlaps []string `json:"overlaps,omitempty"`
	// Terms lists the lexicon terms found in the issue's title, scope or body.
	Terms []string `json:"terms,omitempty"`
	// Reason is the one-line statement of the above.
	Reason string `json:"reason"`
}

// buildRoutingWarningsTx lints the run's bound issues. It reads only the
// activation snapshots, so it sees the run as the dry run computed it.
func buildRoutingWarningsTx(tx *sql.Tx, runID, projectID int) ([]RoutingWarning, RoutingLintReport, error) {
	label, err := db.GetConfigTx(tx, projectID, db.KeyRoutingSecurityLabel)
	if err != nil {
		return nil, RoutingLintReport{}, err
	}
	if label.Value == "" {
		return nil, RoutingLintReport{}, nil
	}
	lexicon, err := db.GetConfigTx(tx, projectID, db.KeyRoutingSecurityLexicon)
	if err != nil {
		return nil, RoutingLintReport{}, err
	}
	terms := db.SplitNameList(lexicon.Value)

	runIssues, err := db.ListRunIssuesTx(tx, runID)
	if err != nil {
		return nil, RoutingLintReport{}, err
	}
	type bound struct {
		id       int
		title    string
		scope    []string
		body     string
		security bool
	}
	issues := make([]bound, 0, len(runIssues))
	for _, ri := range runIssues {
		var snap issueSnapshotFields
		if ri.IssueSnapshot != "" {
			if err := json.Unmarshal([]byte(ri.IssueSnapshot), &snap); err != nil {
				return nil, RoutingLintReport{}, fmt.Errorf("reading the snapshot of %s: %w", model.FormatID(ri.IssueID), err)
			}
		}
		issues = append(issues, bound{
			id: ri.IssueID, title: snap.Title, scope: snap.Scope, body: ri.BodySnapshot,
			security: slices.Contains(snap.Labels, label.Value),
		})
	}

	report := RoutingLintReport{Label: label.Value, LexiconTerms: len(terms)}
	for _, issue := range issues {
		if issue.security && hasLiteralPrefixGlob(issue.scope) {
			report.LabelledIssues++
		}
	}

	var out []RoutingWarning
	for _, issue := range issues {
		if issue.security {
			continue
		}
		report.Linted++
		if !hasLiteralPrefixGlob(issue.scope) {
			report.Unscoped++
		}
		var overlaps, found []string
		for _, other := range issues {
			if other.security && scopesOverlap(issue.scope, other.scope) {
				overlaps = append(overlaps, model.FormatID(other.id))
			}
		}
		haystack := issue.title + "\n" + strings.Join(issue.scope, "\n") + "\n" + issue.body
		for _, term := range terms {
			if lexiconMatch(haystack, term) {
				found = append(found, term)
			}
		}
		if len(overlaps) == 0 && len(found) == 0 {
			continue
		}
		var reasons []string
		if len(overlaps) > 0 {
			reasons = append(reasons, "scope overlaps "+strings.Join(overlaps, ", ")+" ("+label.Value+")")
		}
		if len(found) > 0 {
			reasons = append(reasons, "matches the "+label.Value+" lexicon: "+strings.Join(found, ", "))
		}
		out = append(out, RoutingWarning{
			Issue: model.FormatID(issue.id), Label: label.Value, Overlaps: overlaps, Terms: found,
			Reason: "no " + label.Value + " label, but " + strings.Join(reasons, "; "),
		})
	}
	return out, report, nil
}

// lexiconMatch reports whether text holds term. The space-free lexicon term
// `trust-boundary` (a config name cannot hold whitespace) finds "trust
// boundary" in prose, including prose a line wrap or a double space splits. A
// format character such as a zero-width space is read two ways, as nothing
// and as a space, so it neither hides a split word nor a word break.
func lexiconMatch(text, term string) bool {
	for _, formatAsSpace := range []bool{false, true} {
		if strings.Contains(normalizeLexicon(text, formatAsSpace), normalizeLexicon(term, formatAsSpace)) {
			return true
		}
	}
	return false
}

// normalizeLexicon lowercases s, reads `-`, `_` and any whitespace as one
// space, and reads each format character as a space or drops it.
func normalizeLexicon(s string, formatAsSpace bool) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == '_':
			return ' '
		case unicode.Is(unicode.Cf, r) && formatAsSpace:
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, strings.ToLower(s))), " ")
}

// hasLiteralPrefixGlob reports whether any glob of scope has a literal prefix,
// the only kind the overlap check can compare.
func hasLiteralPrefixGlob(scope []string) bool {
	for _, g := range scope {
		if literalPrefix(g) != "" {
			return true
		}
	}
	return false
}

// scopesOverlap reports whether any glob of a shares paths with any glob of b,
// by the engine's own conservative test (globsIntersect). A glob with no
// literal prefix (`**`, `**/x.go`) intersects everything, which makes it no
// signal here, so it is skipped.
func scopesOverlap(a, b []string) bool {
	for _, ga := range a {
		if literalPrefix(ga) == "" {
			continue
		}
		for _, gb := range b {
			if literalPrefix(gb) != "" && globsIntersect(ga, gb) {
				return true
			}
		}
	}
	return false
}
