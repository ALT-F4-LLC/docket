package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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
	Use:         "graph [id]",
	Short:       "Show dependency graph for an issue",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runIssueGraph)
	},
}

func runIssueGraph(cmd *cobra.Command, args []string, w *output.Writer) error {
	direction, _ := cmd.Flags().GetString("direction")
	depth, _ := cmd.Flags().GetInt("depth")

	graph, err := verb.IssueGraph(cmd.Context(), verbEnv(cmd), verb.IssueGraphInput{
		Issue:     args[0],
		Direction: direction,
		Depth:     depth,
	})
	if err != nil {
		return verbErr(err)
	}
	id, issueMap := graph.Issue.ID, graph.Issues

	var edges []graphEdge
	for _, e := range graph.Edges {
		edges = append(edges, graphEdge{From: e.From, To: e.To, Type: e.Type})
	}

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
	if w.JSONMode {
		w.Success(result, "")
		return nil
	}

	if mermaidMode {
		w.Success(result, renderMermaid(issueMap, edges))
		return nil
	}

	w.Success(result, renderGraphTree(id, issueMap, graph.Forward, graph.Backward, direction, depth))
	return nil
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
