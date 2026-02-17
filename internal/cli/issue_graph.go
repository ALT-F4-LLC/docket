package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/tree"
	"github.com/spf13/cobra"
)

// graphNode represents an issue in the dependency graph.
type graphNode struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// graphEdge represents a dependency between two issues.
type graphEdge struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Type string `json:"type"`
}

// graphResult is the JSON output structure for the graph command.
type graphResult struct {
	IssueID int         `json:"issue_id"`
	Nodes   []graphNode `json:"nodes"`
	Edges   []graphEdge `json:"edges"`
}

var graphCmd = &cobra.Command{
	Use:   "graph [id]",
	Short: "Show dependency graph for an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		issue, err := db.GetIssue(conn, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		direction, _ := cmd.Flags().GetString("direction")
		if direction != "up" && direction != "down" && direction != "both" {
			return cmdErr(fmt.Errorf("invalid direction %q: must be one of [up, down, both]", direction), output.ErrValidation)
		}

		depth, _ := cmd.Flags().GetInt("depth")
		if depth < 0 {
			return cmdErr(fmt.Errorf("depth must be non-negative"), output.ErrValidation)
		}

		// Fetch all directional relations for graph traversal.
		allRelations, err := db.GetAllDirectionalRelations(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
		}

		// Build adjacency lists from relations.
		forward, backward := planner.BuildAdjacency(allRelations)

		// BFS to collect reachable nodes.
		visited := map[int]bool{id: true}
		var edges []graphEdge

		// maxDepth: 0 means unlimited.
		maxDepth := depth

		if direction == "down" || direction == "both" {
			bfsGraph(id, forward, visited, &edges, "blocks", maxDepth)
		}

		if direction == "up" || direction == "both" {
			bfsGraph(id, backward, visited, &edges, "blocked_by", maxDepth)
		}

		// Bulk-fetch issue details for all visited nodes.
		visitedIDs := make([]int, 0, len(visited))
		for nodeID := range visited {
			visitedIDs = append(visitedIDs, nodeID)
		}
		issueMap, err := db.GetIssuesByIDs(conn, visitedIDs)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching issues: %w", err), output.ErrGeneral)
		}
		// Ensure the focal issue is in the map (it was already fetched above).
		issueMap[id] = issue

		// Build nodes list.
		nodes := make([]graphNode, 0, len(issueMap))
		for _, iss := range issueMap {
			nodes = append(nodes, graphNode{
				ID:     model.FormatID(iss.ID),
				Title:  iss.Title,
				Status: string(iss.Status),
			})
		}

		result := graphResult{
			IssueID: id,
			Nodes:   nodes,
			Edges:   edges,
		}

		mermaidMode, _ := cmd.Flags().GetBool("mermaid")
		jsonMode, _ := cmd.Flags().GetBool("json")

		if jsonMode {
			w.Success(result, "")
			return nil
		}

		if mermaidMode {
			w.Success(result, renderMermaid(issueMap, edges))
			return nil
		}

		w.Success(result, renderGraphTree(id, issueMap, forward, backward, direction, maxDepth))
		return nil
	},
}

// bfsGraph performs BFS from the start node, following the given adjacency list,
// collecting edges and marking visited nodes. edgeType labels the edges.
func bfsGraph(start int, adj map[int][]int, visited map[int]bool, edges *[]graphEdge, edgeType string, maxDepth int) {
	type queueItem struct {
		id    int
		depth int
	}
	type edgeKey struct{ from, to int }
	seen := make(map[edgeKey]bool)

	queue := []queueItem{{id: start, depth: 0}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if maxDepth > 0 && current.depth >= maxDepth {
			continue
		}

		for _, neighbor := range adj[current.id] {
			var edge graphEdge
			if edgeType == "blocks" {
				edge = graphEdge{From: current.id, To: neighbor, Type: "blocks"}
			} else {
				edge = graphEdge{From: neighbor, To: current.id, Type: "blocks"}
			}

			k := edgeKey{edge.From, edge.To}
			if !seen[k] {
				seen[k] = true
				*edges = append(*edges, edge)
			}

			if !visited[neighbor] {
				visited[neighbor] = true
				queue = append(queue, queueItem{id: neighbor, depth: current.depth + 1})
			}
		}
	}
}

// renderMermaid outputs a Mermaid flowchart definition.
func renderMermaid(issueMap map[int]*model.Issue, edges []graphEdge) string {
	var sb strings.Builder
	sb.WriteString("graph TD\n")

	for _, e := range edges {
		fromID := model.FormatID(e.From)
		toID := model.FormatID(e.To)
		fromTitle := fromID
		toTitle := toID
		if iss, ok := issueMap[e.From]; ok {
			fromTitle = fmt.Sprintf("%s: %s", fromID, iss.Title)
		}
		if iss, ok := issueMap[e.To]; ok {
			toTitle = fmt.Sprintf("%s: %s", toID, iss.Title)
		}

		fmt.Fprintf(&sb, "    %s[\"%s\"] --> %s[\"%s\"]\n", fromID, fromTitle, toID, toTitle)
	}

	return sb.String()
}

// renderGraphTree renders the dependency graph as a human-readable tree.
func renderGraphTree(focalID int, issueMap map[int]*model.Issue, forward, backward map[int][]int, direction string, maxDepth int) string {
	focal := issueMap[focalID]
	if focal == nil {
		return ""
	}

	if !render.ColorsEnabled() {
		return renderGraphTreePlain(focalID, issueMap, forward, backward, direction, maxDepth)
	}

	rootLabel := formatGraphNode(focal, true)
	t := tree.New().Root(rootLabel)

	if direction == "up" || direction == "both" {
		if deps := backward[focalID]; len(deps) > 0 {
			sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
			upNode := tree.Root(sectionStyle.Render("Blocked by"))
			visited := map[int]bool{focalID: true}
			addGraphChildren(upNode, focalID, backward, issueMap, visited, 1, maxDepth)
			t.Child(upNode)
		}
	}

	if direction == "down" || direction == "both" {
		if deps := forward[focalID]; len(deps) > 0 {
			sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
			downNode := tree.Root(sectionStyle.Render("Blocks"))
			visited := map[int]bool{focalID: true}
			addGraphChildren(downNode, focalID, forward, issueMap, visited, 1, maxDepth)
			t.Child(downNode)
		}
	}

	return t.String()
}

// formatGraphNode formats an issue for tree display.
func formatGraphNode(issue *model.Issue, isFocal bool) string {
	if !render.ColorsEnabled() {
		prefix := ""
		if isFocal {
			prefix = "* "
		}
		return fmt.Sprintf("%s%s [%s] %s", prefix, model.FormatID(issue.ID), string(issue.Status), issue.Title)
	}

	idStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	statusStyle := lipgloss.NewStyle().Foreground(render.ColorFromName(issue.Status.Color()))
	titleStyle := lipgloss.NewStyle()
	if isFocal {
		titleStyle = titleStyle.Bold(true)
	}

	return fmt.Sprintf("%s %s %s",
		idStyle.Render(model.FormatID(issue.ID)),
		statusStyle.Render("["+string(issue.Status)+"]"),
		titleStyle.Render(issue.Title),
	)
}

// addGraphChildren recursively adds child nodes for BFS tree rendering.
func addGraphChildren(node *tree.Tree, parentID int, adj map[int][]int, issueMap map[int]*model.Issue, visited map[int]bool, currentDepth, maxDepth int) {
	if maxDepth > 0 && currentDepth > maxDepth {
		return
	}

	for _, childID := range adj[parentID] {
		if visited[childID] {
			continue
		}
		visited[childID] = true

		iss := issueMap[childID]
		if iss == nil {
			continue
		}

		childNode := tree.Root(formatGraphNode(iss, false))
		addGraphChildren(childNode, childID, adj, issueMap, visited, currentDepth+1, maxDepth)
		node.Child(childNode)
	}
}

// renderGraphTreePlain renders the graph tree without colors.
func renderGraphTreePlain(focalID int, issueMap map[int]*model.Issue, forward, backward map[int][]int, direction string, maxDepth int) string {
	focal := issueMap[focalID]
	if focal == nil {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "* %s [%s] %s\n", model.FormatID(focal.ID), string(focal.Status), focal.Title)

	if direction == "up" || direction == "both" {
		if deps := backward[focalID]; len(deps) > 0 {
			sb.WriteString("  Blocked by\n")
			visited := map[int]bool{focalID: true}
			renderPlainGraphChildren(&sb, focalID, backward, issueMap, visited, 2, 1, maxDepth)
		}
	}

	if direction == "down" || direction == "both" {
		if deps := forward[focalID]; len(deps) > 0 {
			sb.WriteString("  Blocks\n")
			visited := map[int]bool{focalID: true}
			renderPlainGraphChildren(&sb, focalID, forward, issueMap, visited, 2, 1, maxDepth)
		}
	}

	return sb.String()
}

// renderPlainGraphChildren renders children in plain text with indentation.
func renderPlainGraphChildren(sb *strings.Builder, parentID int, adj map[int][]int, issueMap map[int]*model.Issue, visited map[int]bool, indent, currentDepth, maxDepth int) {
	if maxDepth > 0 && currentDepth > maxDepth {
		return
	}

	for _, childID := range adj[parentID] {
		if visited[childID] {
			continue
		}
		visited[childID] = true

		iss := issueMap[childID]
		if iss == nil {
			continue
		}

		prefix := strings.Repeat("  ", indent)
		fmt.Fprintf(sb, "%s%s [%s] %s\n", prefix, model.FormatID(iss.ID), string(iss.Status), iss.Title)
		renderPlainGraphChildren(sb, childID, adj, issueMap, visited, indent+1, currentDepth+1, maxDepth)
	}
}

func init() {
	graphCmd.Flags().Int("depth", 0, "Maximum traversal depth (0 = unlimited)")
	graphCmd.Flags().String("direction", "both", "Traversal direction: up, down, or both")
	graphCmd.Flags().Bool("mermaid", false, "Output as Mermaid flowchart syntax")
	issueCmd.AddCommand(graphCmd)
}
