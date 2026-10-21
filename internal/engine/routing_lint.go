package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

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

// RoutingWarning is one bound issue the lint flagged.
type RoutingWarning struct {
	Issue string `json:"issue"`
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
func buildRoutingWarningsTx(tx *sql.Tx, runID, projectID int) ([]RoutingWarning, error) {
	label, err := db.GetConfigTx(tx, projectID, db.KeyRoutingSecurityLabel)
	if err != nil {
		return nil, err
	}
	if label.Value == "" {
		return nil, nil
	}
	lexicon, err := db.GetConfigTx(tx, projectID, db.KeyRoutingSecurityLexicon)
	if err != nil {
		return nil, err
	}
	terms := db.SplitNameList(lexicon.Value)

	runIssues, err := db.ListRunIssuesTx(tx, runID)
	if err != nil {
		return nil, err
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
				return nil, fmt.Errorf("reading the snapshot of %s: %w", model.FormatID(ri.IssueID), err)
			}
		}
		issues = append(issues, bound{
			id: ri.IssueID, title: snap.Title, scope: snap.Scope, body: ri.BodySnapshot,
			security: slices.Contains(snap.Labels, label.Value),
		})
	}

	var out []RoutingWarning
	for _, issue := range issues {
		if issue.security {
			continue
		}
		var overlaps, found []string
		for _, other := range issues {
			if other.security && scopesOverlap(issue.scope, other.scope) {
				overlaps = append(overlaps, model.FormatID(other.id))
			}
		}
		haystack := normalizeLexicon(issue.title + "\n" + strings.Join(issue.scope, "\n") + "\n" + issue.body)
		for _, term := range terms {
			if strings.Contains(haystack, normalizeLexicon(term)) {
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
			Issue: model.FormatID(issue.id), Overlaps: overlaps, Terms: found,
			Reason: "no " + label.Value + " label, but " + strings.Join(reasons, "; "),
		})
	}
	return out, nil
}

// normalizeLexicon lowercases s and reads `-` and `_` as spaces, so the
// space-free lexicon term `trust-boundary` (a config name cannot hold
// whitespace) finds "trust boundary" in prose.
func normalizeLexicon(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' {
			return ' '
		}
		return r
	}, strings.ToLower(s))
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
