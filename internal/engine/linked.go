package engine

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The `issue.linked.<relation>.<kind>` input form (DKT-547): a step consuming
// an artifact RECORDED UNDER ANOTHER ISSUE, reached through the consuming
// issue's declared relations rather than through an unenforced issue-body
// citation.
//
// The incident that forced it: ui-change@12 claimed its changes were "bound to
// an accepted ux-spec", but the spec was a doc produced by a spec-doc run on a
// DIFFERENT issue, and no input form reached it — every legal form is same-run
// and same-issue (`<step>.<kind>`, `issue.latest.<kind>`, the issue forms). So
// the spec reached executors only when the issue body happened to cite it: 33
// design-qa instances across 3 runs all relying on prose.
//
// RESOLUTION IS ACTIVATION'S, NOT ASSEMBLY'S. Context assembly is pure and
// snapshot-pinned (§6.6): it may not read live state, and "the linked issue's
// latest artifact" is live state — a later run on that issue would change the
// answer mid-run. So activation resolves the relation and the artifact ONCE,
// while it snapshots everything else about the issue, and records the resolved
// ARTIFACT IDS in the issue's `issue_snapshot` under a `linked` key. Artifact
// rows are never mutated (reliability-delta §2.1), so an id is a content pin
// exactly as a `pins` row's hash is — assembly reads the pinned rows by id and
// never asks the live question. And because re-activation never re-snapshots
// (RA2's reasoning), the binding cannot drift under a run already under way.
//
// FAILURE IS LOUD, AT ACTIVATION, per the acceptance criteria: a declared
// relation with no link, or a link whose issues hold no artifact of the kind,
// refuses the whole activation inside the fat transaction — the binding is
// enforced rather than conventional, and nothing is written.

// linkedSuffix is the snapshot key for one declared form: the declaration
// verbatim, minus the `issue.linked.` prefix — "<relation>.<kind>" exactly as
// the author spelled it, so assembly's lookup is a verbatim match and no
// normalization rule has to agree across two packages.
func linkedSuffix(declared string) string {
	return strings.TrimPrefix(declared, workflow.InputIssueLinkedPrefix)
}

// resolveLinkedInputs resolves every distinct `issue.linked.<relation>.<kind>`
// declaration in a bound definition for ONE issue, at activation.
//
// Per declaration: the linked issue(s) by relation (canonical token = this
// issue is the relation's source; inverse token = its target; the symmetric
// `relates_to` admits both directions), then each linked issue's latest
// recorded artifact of the kind — highest artifact id whose producing step
// recorded its work (done or superseded, recordedProducer's rule), across
// every run in the store, which is the same "highest id is newest" reading
// latestPerProducer applies within a run.
//
// A linked issue with NO artifact of the kind contributes nothing rather than
// failing: relations are overloaded — `depends_on` orders scheduling as well
// as binding specs — so a ui-change issue depending on three implementation
// issues and one spec issue must resolve the spec, not refuse over the
// implementations. The failure modes are the empty ones: no relation at all,
// or a relation none of whose issues holds the kind.
//
// The result maps each declaration's suffix ("<relation>.<kind>", verbatim) to
// the pinned artifact ids, ordered by linked issue id then artifact id — a
// pure function of the store, so two activations of the same state pin the
// same rows.
func resolveLinkedInputs(
	tx *sql.Tx, issue *model.Issue, def *workflow.Definition,
) (map[string][]int, error) {
	var out map[string][]int
	for _, step := range def.Steps {
		for _, input := range step.Inputs {
			relation, kind, ok := workflow.LinkedInput(input)
			if !ok {
				continue
			}
			suffix := linkedSuffix(input)
			if _, done := out[suffix]; done {
				continue
			}

			ids, err := resolveLinkedDeclaration(tx, issue, input, relation, kind)
			if err != nil {
				return nil, err
			}
			if out == nil {
				out = make(map[string][]int)
			}
			out[suffix] = ids
		}
	}
	return out, nil
}

// resolveLinkedDeclaration is one declaration's resolution: linked issues,
// then the latest recorded artifact of the kind per linked issue, with the
// loud refusals.
func resolveLinkedDeclaration(
	tx *sql.Tx, issue *model.Issue, declared, relation, kind string,
) ([]int, error) {
	linked, err := linkedIssueIDs(tx, issue.ID, relation)
	if err != nil {
		return nil, err
	}
	if len(linked) == 0 {
		return nil, validationErr(
			"issue %s declares input %q, but has no %s relation to any issue; "+
				"link one with `docket issue link add` before activating",
			model.FormatID(issue.ID), declared, relation)
	}

	var ids []int
	for _, linkedID := range linked {
		id, err := latestArtifactOfKind(tx, linkedID, kind)
		if err != nil {
			return nil, err
		}
		if id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, validationErr(
			"issue %s declares input %q, but none of its %s-linked issue(s) "+
				"(%s) has a recorded artifact of kind %q; produce and record "+
				"one there before activating",
			model.FormatID(issue.ID), declared, relation,
			formatIDList(linked), kind)
	}
	return ids, nil
}

// linkedIssueIDs resolves the issues one relation token reaches from a
// consuming issue, sorted ascending so the pin order is deterministic.
func linkedIssueIDs(tx *sql.Tx, issueID int, relation string) ([]int, error) {
	rt, inverse, err := model.ParseRelationDirection(relation)
	if err != nil {
		// V11 refuses unknown relation tokens at register, so reaching here
		// means a definition was written directly into the table.
		return nil, validationErr("input relation %q: %v", relation, err)
	}

	// The canonical token reads the relation from its SOURCE ("A depends_on
	// B": A's `depends_on` reaches B), the inverse token from its TARGET. The
	// symmetric `relates_to` is its own inverse, so it reads both directions.
	forward := !inverse || rt == model.RelationRelatesTo
	backward := inverse || rt == model.RelationRelatesTo

	seen := make(map[int]bool)
	var out []int
	collect := func(query string) error {
		rows, err := tx.Query(query, issueID, string(rt))
		if err != nil {
			return fmt.Errorf("reading %s relations: %w", rt, err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("reading %s relations: %w", rt, err)
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		return rows.Err()
	}

	if forward {
		if err := collect(
			`SELECT target_issue_id FROM issue_relations
			  WHERE source_issue_id = ? AND relation_type = ?`); err != nil {
			return nil, err
		}
	}
	if backward {
		if err := collect(
			`SELECT source_issue_id FROM issue_relations
			  WHERE target_issue_id = ? AND relation_type = ?`); err != nil {
			return nil, err
		}
	}
	sort.Ints(out)
	return out, nil
}

// latestArtifactOfKind is one linked issue's contribution: the highest-id
// artifact of the kind whose producing step recorded its work, across every
// run — or 0 when the issue holds none.
func latestArtifactOfKind(tx *sql.Tx, issueID int, kind string) (int, error) {
	var id int
	err := tx.QueryRow(
		`SELECT a.id FROM artifacts a
		   JOIN steps s ON s.id = a.step_id
		  WHERE s.issue_id = ? AND a.kind = ? AND s.status IN (?, ?)
		  ORDER BY a.id DESC LIMIT 1`,
		issueID, kind, db.StepDone, db.StepSuperseded).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf(
			"resolving the latest %q artifact of %s: %w",
			kind, model.FormatID(issueID), err)
	}
	return id, nil
}

// formatIDList renders issue ids as display ids for an error message.
func formatIDList(ids []int) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.FormatID(id))
	}
	return strings.Join(out, ", ")
}

// resolveLinkedPinned is assembly's half: the ContextInputs for one declared
// `issue.linked` entry, loaded by the artifact ids activation pinned into the
// issue snapshot.
//
// The producer instance is rendered as `<ISSUE>/<instance>` — the producing
// step belongs to another issue (usually another run), so its bare instance
// name would collide with this run's own namespace and say nothing about where
// the artifact came from.
//
// A missing snapshot entry is an error, not an empty resolution: activation
// pins every declaration the PINNED definition makes, in the same transaction
// that snapshots the issue, so absence means the ledger was edited by hand —
// resolving to nothing would silently reopen the unenforced-citation gap this
// form exists to close.
func resolveLinkedPinned(
	tx *sql.Tx, step *db.Step, linked map[string][]int, declared string,
) ([]ContextInput, error) {
	ids, ok := linked[linkedSuffix(declared)]
	if !ok {
		return nil, fmt.Errorf(
			"step %s: input %q was not pinned at activation — the issue "+
				"snapshot holds no resolution for it", step.Instance, declared)
	}

	out := make([]ContextInput, 0, len(ids))
	for _, id := range ids {
		var (
			kind, body string
			payload    sql.NullString
			instance   sql.NullString
			issueID    sql.NullInt64
		)
		err := tx.QueryRow(
			`SELECT a.kind, a.body, a.payload, s.instance, s.issue_id
			   FROM artifacts a
			   LEFT JOIN steps s ON s.id = a.step_id
			  WHERE a.id = ?`, id).Scan(&kind, &body, &payload, &instance, &issueID)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf(
				"step %s: input %q was pinned to artifact %d at activation, "+
					"but the artifact no longer exists", step.Instance, declared, id)
		}
		if err != nil {
			return nil, fmt.Errorf(
				"reading pinned artifact %d for %s: %w", id, step.Instance, err)
		}

		producer := ""
		if instance.Valid && issueID.Valid {
			producer = model.FormatID(int(issueID.Int64)) + "/" + instance.String
		}
		out = append(out, ContextInput{
			Artifact:     fmt.Sprintf("ARTIFACT-%d", id),
			Kind:         kind,
			ProducerStep: producer,
			Body:         body,
			Payload:      payload.String,
		})
	}
	return out, nil
}

// linkedArtifacts is resolveLinkedPinned for a consumer that needs the ROWS —
// ResolveInputArtifacts' reading, sharing the pinned-id lookup so the two
// resolvers cannot disagree about which artifacts a declaration binds.
func linkedArtifacts(
	tx *sql.Tx, step *db.Step, linked map[string][]int, declared string,
) ([]*db.Artifact, error) {
	ids, ok := linked[linkedSuffix(declared)]
	if !ok {
		return nil, fmt.Errorf(
			"step %s: input %q was not pinned at activation — the issue "+
				"snapshot holds no resolution for it", step.Instance, declared)
	}
	out := make([]*db.Artifact, 0, len(ids))
	for _, id := range ids {
		a, err := db.GetArtifactTx(tx, id)
		if err != nil {
			return nil, fmt.Errorf(
				"reading pinned artifact %d for %s: %w", id, step.Instance, err)
		}
		out = append(out, a)
	}
	return out, nil
}
