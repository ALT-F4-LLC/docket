package planner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// CycleError is returned when the DAG contains a cycle and topological
// sorting is not possible.
type CycleError struct {
	IDs []int
}

func (e *CycleError) Error() string {
	parts := make([]string, len(e.IDs))
	for i, id := range e.IDs {
		parts[i] = model.FormatID(id)
	}
	return fmt.Sprintf("cycle detected among issues: %s", strings.Join(parts, ", "))
}

// TopoSort performs a topological sort on the DAG using Kahn's algorithm.
// It returns issue IDs grouped by topological level: level 0 contains issues
// with no dependencies, level 1 contains issues whose dependencies are all
// in level 0, and so on.
//
// Returns a CycleError if the graph contains a cycle, listing the IDs of
// the issues involved in the cycle.
func TopoSort(dag *DAG) ([][]int, error) {
	// Build a mutable in-degree map.
	inDegree := make(map[int]int, len(dag.Nodes))
	for id, node := range dag.Nodes {
		inDegree[id] = len(node.Reverse)
	}

	// Seed the queue with nodes that have in-degree 0.
	var queue []int
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	sort.Ints(queue)

	var levels [][]int
	processed := 0

	for len(queue) > 0 {
		level := make([]int, len(queue))
		copy(level, queue)
		sort.Ints(level)
		levels = append(levels, level)
		processed += len(level)

		var nextQueue []int
		for _, id := range queue {
			node := dag.Nodes[id]
			for neighbor := range node.Forward {
				inDegree[neighbor]--
				if inDegree[neighbor] == 0 {
					nextQueue = append(nextQueue, neighbor)
				}
			}
		}
		sort.Ints(nextQueue)
		queue = nextQueue
	}

	if processed != len(dag.Nodes) {
		// Collect IDs of nodes still in the graph (part of cycles).
		var cycleIDs []int
		for id, deg := range inDegree {
			if deg > 0 {
				cycleIDs = append(cycleIDs, id)
			}
		}
		sort.Ints(cycleIDs)
		return nil, &CycleError{IDs: cycleIDs}
	}

	return levels, nil
}
