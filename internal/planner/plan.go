package planner

import (
	"sort"

	"github.com/ALT-F4-LLC/docket/internal/filter"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Phase is a group of issues that can be worked in parallel.
type Phase struct {
	Number int
	Issues []*model.Issue
}

// Plan is the full execution plan: a sequence of phases with summary stats.
type Plan struct {
	Phases         []Phase
	TotalIssues    int
	TotalPhases    int
	MaxParallelism int
}

// PlanFilters controls which issues are included in the generated plan.
type PlanFilters struct {
	Statuses []string
	Labels   []string
	RootID   *int
}

// GeneratePlan builds an execution plan from the DAG. It uses topological
// level grouping to create phases: phase 1 contains issues with no blockers,
// phase N contains issues whose blockers are all in earlier phases. Issues
// already done are skipped, and optional status/label/root filters are applied.
func GeneratePlan(dag *DAG, filters PlanFilters) (*Plan, error) {
	// When RootID is set, scope the DAG to the root and its descendants.
	if filters.RootID != nil {
		dag = scopeToDescendants(dag, *filters.RootID)
	}

	// Build filter sets for O(1) lookup.
	statusSet := filter.ToStringSet(filters.Statuses)
	labelSet := filter.ToStringSet(filters.Labels)

	levels, err := TopoSort(dag)
	if err != nil {
		return nil, err
	}

	plan := &Plan{}

	for _, level := range levels {
		var phaseIssues []*model.Issue
		for _, id := range level {
			node, ok := dag.Nodes[id]
			if !ok {
				continue
			}
			issue := node.Issue

			// Skip done issues.
			if issue.Status == model.StatusDone {
				continue
			}

			// Apply status filter.
			if len(statusSet) > 0 {
				if _, ok := statusSet[string(issue.Status)]; !ok {
					continue
				}
			}

			// Apply label filter (AND logic: issue must have all labels).
			if len(labelSet) > 0 && !filter.HasAllLabels(issue, labelSet) {
				continue
			}

			phaseIssues = append(phaseIssues, issue)
		}

		if len(phaseIssues) == 0 {
			continue
		}

		sortIssues(phaseIssues)

		plan.Phases = append(plan.Phases, Phase{
			Number: len(plan.Phases) + 1,
			Issues: phaseIssues,
		})
	}

	// Compute summary stats.
	for _, phase := range plan.Phases {
		plan.TotalIssues += len(phase.Issues)
		if len(phase.Issues) > plan.MaxParallelism {
			plan.MaxParallelism = len(phase.Issues)
		}
	}
	plan.TotalPhases = len(plan.Phases)

	return plan, nil
}

// FindReady returns issues that are work-ready: their status is in the
// provided list (default: backlog, todo), all blockers are done, and they
// have no children (leaf tasks only). Results are sorted by priority
// (highest first), then by ID (oldest first).
func FindReady(dag *DAG, statuses []string) []*model.Issue {
	if len(statuses) == 0 {
		statuses = []string{string(model.StatusBacklog), string(model.StatusTodo)}
	}
	statusSet := filter.ToStringSet(statuses)

	// Build set of issues that have children (non-leaf).
	parents := make(map[int]struct{})
	for _, node := range dag.Nodes {
		if node.Issue.ParentID != nil {
			parents[*node.Issue.ParentID] = struct{}{}
		}
	}

	var ready []*model.Issue
	for _, node := range dag.Nodes {
		issue := node.Issue

		// Must be in an allowed status.
		if _, ok := statusSet[string(issue.Status)]; !ok {
			continue
		}

		// Exclude non-leaf issues (those that have children).
		if _, ok := parents[issue.ID]; ok {
			continue
		}

		// All blockers (reverse edges) must be done.
		allBlockersDone := true
		for blockerID := range node.Reverse {
			blockerNode, ok := dag.Nodes[blockerID]
			if !ok {
				continue
			}
			if blockerNode.Issue.Status != model.StatusDone {
				allBlockersDone = false
				break
			}
		}
		if !allBlockersDone {
			continue
		}

		ready = append(ready, issue)
	}

	sortIssues(ready)
	return ready
}

// --- helpers ---

// priorityRank returns a numeric rank for sorting: lower rank = higher priority.
func priorityRank(p model.Priority) int {
	switch p {
	case model.PriorityCritical:
		return 0
	case model.PriorityHigh:
		return 1
	case model.PriorityMedium:
		return 2
	case model.PriorityLow:
		return 3
	case model.PriorityNone:
		return 4
	default:
		return 5
	}
}

// sortIssues sorts by priority (highest first), then by ID (oldest/lowest first).
func sortIssues(issues []*model.Issue) {
	sort.Slice(issues, func(i, j int) bool {
		ri, rj := priorityRank(issues[i].Priority), priorityRank(issues[j].Priority)
		if ri != rj {
			return ri < rj
		}
		return issues[i].ID < issues[j].ID
	})
}

// scopeToDescendants returns a new DAG containing only the root node and its
// descendants (by parent-child hierarchy in the DAG nodes).
func scopeToDescendants(dag *DAG, rootID int) *DAG {
	// BFS over the parent-child tree to find all descendants.
	keep := make(map[int]struct{})

	// Include the root itself if present.
	if _, ok := dag.Nodes[rootID]; ok {
		keep[rootID] = struct{}{}
	}

	// Find all nodes whose ParentID chains back to rootID.
	// Build a children map first for efficient traversal.
	children := make(map[int][]int)
	for id, node := range dag.Nodes {
		if node.Issue.ParentID != nil {
			children[*node.Issue.ParentID] = append(children[*node.Issue.ParentID], id)
		}
	}

	queue := []int{rootID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, childID := range children[current] {
			if _, ok := dag.Nodes[childID]; ok {
				keep[childID] = struct{}{}
				queue = append(queue, childID)
			}
		}
	}

	// Build the scoped DAG.
	scoped := &DAG{
		Nodes: make(map[int]*Node, len(keep)),
	}
	for id := range keep {
		orig := dag.Nodes[id]
		node := &Node{
			Issue:   orig.Issue,
			Forward: make(map[int]struct{}),
			Reverse: make(map[int]struct{}),
		}
		// Only include edges to other nodes in the scoped set.
		for fwd := range orig.Forward {
			if _, ok := keep[fwd]; ok {
				node.Forward[fwd] = struct{}{}
			}
		}
		for rev := range orig.Reverse {
			if _, ok := keep[rev]; ok {
				node.Reverse[rev] = struct{}{}
			}
		}
		scoped.Nodes[id] = node
	}

	return scoped
}
