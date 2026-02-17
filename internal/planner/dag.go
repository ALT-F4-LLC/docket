package planner

import "github.com/ALT-F4-LLC/docket/internal/model"

// Node wraps a model.Issue with forward and reverse dependency edges.
// Forward edges point from a blocker to the issues it blocks (blocker -> blocked).
// Reverse edges point from a blocked issue back to its blockers (blocked -> blockers).
type Node struct {
	Issue   *model.Issue
	Forward map[int]struct{} // issue IDs this node blocks
	Reverse map[int]struct{} // issue IDs that block this node
}

// DAG holds the directed acyclic graph of issue dependencies.
type DAG struct {
	Nodes map[int]*Node
}

// BuildDAG constructs a DAG from issues and their directional relations.
//
// Relations are normalized into a single edge direction:
//   - "blocks":     source blocks target  -> edge from source to target (target depends on source)
//   - "depends_on": source depends on target -> edge from target to source (source depends on target)
//
// Forward edges go from blocker to blocked; reverse edges go from blocked to blockers.
// Only issues present in the input slice are included as nodes. Relations referencing
// issues not in the input are silently ignored.
func BuildDAG(issues []*model.Issue, relations []model.Relation) *DAG {
	dag := &DAG{
		Nodes: make(map[int]*Node, len(issues)),
	}

	for _, issue := range issues {
		dag.Nodes[issue.ID] = &Node{
			Issue:   issue,
			Forward: make(map[int]struct{}),
			Reverse: make(map[int]struct{}),
		}
	}

	for _, rel := range relations {
		var fromID, toID int

		switch rel.RelationType {
		case model.RelationBlocks:
			// source blocks target: edge from source (blocker) to target (blocked)
			fromID = rel.SourceIssueID
			toID = rel.TargetIssueID
		case model.RelationDependsOn:
			// source depends_on target: edge from target (blocker) to source (blocked)
			fromID = rel.TargetIssueID
			toID = rel.SourceIssueID
		default:
			continue
		}

		fromNode, fromOK := dag.Nodes[fromID]
		toNode, toOK := dag.Nodes[toID]
		if !fromOK || !toOK {
			continue
		}

		fromNode.Forward[toID] = struct{}{}
		toNode.Reverse[fromID] = struct{}{}
	}

	return dag
}

// BuildAdjacency constructs forward and backward adjacency lists from relations.
// forward[A] contains IDs that A blocks (downstream). backward[A] contains IDs
// that block A (upstream). Relations are normalized so both "blocks" and
// "depends_on" produce the same canonical edge direction.
func BuildAdjacency(relations []model.Relation) (forward, backward map[int][]int) {
	forward = make(map[int][]int)
	backward = make(map[int][]int)

	for _, rel := range relations {
		switch rel.RelationType {
		case model.RelationBlocks:
			forward[rel.SourceIssueID] = append(forward[rel.SourceIssueID], rel.TargetIssueID)
			backward[rel.TargetIssueID] = append(backward[rel.TargetIssueID], rel.SourceIssueID)
		case model.RelationDependsOn:
			// "A depends_on B" means "B blocks A"
			forward[rel.TargetIssueID] = append(forward[rel.TargetIssueID], rel.SourceIssueID)
			backward[rel.SourceIssueID] = append(backward[rel.SourceIssueID], rel.TargetIssueID)
		}
	}

	return forward, backward
}
