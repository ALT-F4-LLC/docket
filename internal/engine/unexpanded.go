package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/planner"
)

// maxBlockerChains bounds how many blocker chains one unexpanded issue lists.
// A dense dependency graph has combinatorially many root-to-issue paths; the
// operator needs the roots that hold the issue, not every route to them.
const maxBlockerChains = 16

// BlockerHop is one issue on a blocker chain and its status.
type BlockerHop struct {
	Issue  string `json:"issue"`
	Status string `json:"status"`
}

// UnexpandedIssue is a bound roster issue that holds no steps yet, with the
// unmet depends_on chains keeping it from getting any.
//
// Each chain runs from the issue's direct blocker down to the blocker that is
// itself unblocked — the root an operator releases or finishes to move the
// issue. A conductor's close-out question reads it to state which held issues
// wait behind which, instead of diffing the roster against the step list by
// hand (DKT-4255).
type UnexpandedIssue struct {
	Issue     string         `json:"issue"`
	Status    string         `json:"status"`
	BlockedBy [][]BlockerHop `json:"blocked_by"`
	// BlockedByTruncated marks a list cut at maxBlockerChains.
	BlockedByTruncated bool `json:"blocked_by_truncated,omitempty"`
}

// UnexpandedIssues lists the run's bound issues that hold no step, in issue
// order. It is a pure read.
func UnexpandedIssues(conn *sql.DB, runID int) ([]UnexpandedIssue, error) {
	roster, err := db.ListRunIssues(conn, runID)
	if err != nil {
		return nil, fmt.Errorf("listing run issues: %w", err)
	}
	steps, err := db.ListRunSteps(conn, runID)
	if err != nil {
		return nil, fmt.Errorf("listing run steps: %w", err)
	}
	stepped := make(map[int]struct{}, len(roster))
	for _, s := range steps {
		stepped[s.IssueID] = struct{}{}
	}

	var unexpanded []int
	for _, ri := range roster {
		if _, ok := stepped[ri.IssueID]; !ok {
			unexpanded = append(unexpanded, ri.IssueID)
		}
	}
	if len(unexpanded) == 0 {
		return nil, nil
	}

	relations, err := db.GetAllDirectionalRelations(conn)
	if err != nil {
		return nil, fmt.Errorf("reading relations: %w", err)
	}
	_, blockers := planner.BuildAdjacency(relations)

	// Every issue reachable through the blocker edges needs a status.
	need := make(map[int]struct{})
	var walk func(id int)
	walk = func(id int) {
		if _, seen := need[id]; seen {
			return
		}
		need[id] = struct{}{}
		for _, b := range blockers[id] {
			walk(b)
		}
	}
	for _, id := range unexpanded {
		walk(id)
	}
	ids := make([]int, 0, len(need))
	for id := range need {
		ids = append(ids, id)
	}
	issues, err := db.GetIssuesByIDs(conn, ids)
	if err != nil {
		return nil, fmt.Errorf("reading blocker issues: %w", err)
	}

	unmet := func(id int) bool {
		i, ok := issues[id]
		return ok && i.Status != model.StatusDone
	}
	hop := func(id int) BlockerHop {
		return BlockerHop{Issue: model.FormatID(id), Status: string(issues[id].Status)}
	}

	out := make([]UnexpandedIssue, 0, len(unexpanded))
	for _, id := range unexpanded {
		item := UnexpandedIssue{
			Issue:     model.FormatID(id),
			BlockedBy: [][]BlockerHop{},
		}
		if i, ok := issues[id]; ok {
			item.Status = string(i.Status)
		}

		var chains [][]BlockerHop
		var extend func(id int, path []BlockerHop, onPath map[int]bool)
		extend = func(id int, path []BlockerHop, onPath map[int]bool) {
			var open []int
			for _, b := range blockers[id] {
				if unmet(b) && !onPath[b] {
					open = append(open, b)
				}
			}
			if len(open) == 0 {
				if len(path) > 0 {
					chains = append(chains, append([]BlockerHop(nil), path...))
				}
				return
			}
			for _, b := range open {
				if len(chains) >= maxBlockerChains {
					item.BlockedByTruncated = true
					return
				}
				onPath[b] = true
				extend(b, append(path, hop(b)), onPath)
				delete(onPath, b)
			}
		}
		extend(id, nil, map[int]bool{id: true})
		if chains != nil {
			item.BlockedBy = chains
		}
		out = append(out, item)
	}
	return out, nil
}
