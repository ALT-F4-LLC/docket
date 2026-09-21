package tui

import (
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/render"
)

var listSortFields = []string{"id", "title", "status", "priority", "kind", "assignee", "created_at", "updated_at"}

func isTerminalProbe(key string) bool {
	if key == "alt+\\" || key == "alt+]" {
		return true
	}
	return strings.Contains(key, "rgb:") || strings.Contains(key, "]11;")
}

func groupBoardColumns(issues []*model.Issue) []boardColumn {
	grouped := make(map[model.Status][]*model.Issue)
	for _, issue := range issues {
		grouped[issue.Status] = append(grouped[issue.Status], issue)
	}

	cols := make([]boardColumn, 0, len(render.StatusOrder))
	for _, status := range render.StatusOrder {
		if len(grouped[status]) == 0 {
			continue
		}
		cols = append(cols, boardColumn{Status: status, Issues: grouped[status]})
	}
	return cols
}

func findIssueIndex(issues []*model.Issue, id int) int {
	for i, issue := range issues {
		if issue.ID == id {
			return i
		}
	}
	return -1
}

func findBoardIssue(columns []boardColumn, id int) (int, int) {
	for colIdx, col := range columns {
		for rowIdx, issue := range col.Issues {
			if issue.ID == id {
				return colIdx, rowIdx
			}
		}
	}
	return -1, -1
}

func windowBounds(selected, total, height int) (int, int) {
	if total == 0 {
		return 0, 0
	}
	if height >= total {
		return 0, total
	}
	start := selected - height/2
	if start < 0 {
		start = 0
	}
	end := start + height
	if end > total {
		end = total
		start = end - height
	}
	return start, end
}

func detailWindow(scroll, total, height int) (int, int) {
	if total == 0 {
		return 0, 0
	}
	if height >= total {
		return 0, total
	}
	start := clamp(scroll, 0, max(total-height, 0))
	return start, min(start+height, total)
}

func clamp(n, low, high int) int {
	if n < low {
		return low
	}
	if n > high {
		return high
	}
	return n
}

func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

func (m *browserModel) cycleListSortField(delta int) bool {
	if len(listSortFields) == 0 {
		return false
	}
	idx := 0
	for i, field := range listSortFields {
		if field == m.listSort.Field {
			idx = i
			break
		}
	}
	next := (idx + delta) % len(listSortFields)
	if next < 0 {
		next += len(listSortFields)
	}
	if listSortFields[next] == m.listSort.Field {
		return false
	}
	m.listSort.Field = listSortFields[next]
	return true
}

func (m *browserModel) toggleListSortDirection() {
	if strings.EqualFold(m.listSort.Dir, "asc") {
		m.listSort.Dir = "desc"
		return
	}
	m.listSort.Dir = "asc"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
