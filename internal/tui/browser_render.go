package tui

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/render"
)

func (m browserModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading docket tui..."
	}

	if m.showHelp {
		return m.renderHelp()
	}

	header := m.renderHeader()
	footer := m.renderFooter()
	contentHeight := max(m.height-uiLineCount(header)-uiLineCount(footer), 1)
	content := m.renderContent(contentHeight)

	return render.JoinUIVertical(header, content, footer)
}

func uiLineCount(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

func (m browserModel) renderHeader() string {
	context := []string{}
	if m.view == viewList {
		context = append(context, fmt.Sprintf("SORT %s %s", strings.ToUpper(m.listSort.Field), strings.ToUpper(m.listSort.Dir)))
	}
	return render.RenderUIHeaderBar(m.projectName, string(m.view), m.refreshStatusValue(), m.width, context...)
}

func (m browserModel) renderFooter() string {
	if m.view == viewList {
		return render.RenderUIListFooterBar(m.detailExpanded, m.focus == focusDetail, m.focus == focusBrowse && !m.detailExpanded, m.refreshStatusValue(), m.width)
	}
	return render.RenderUIFooterBar(m.detailExpanded, m.focus == focusDetail, m.focus == focusBrowse && !m.detailExpanded, m.refreshStatusValue(), m.width)
}

func (m browserModel) renderContent(contentHeight int) string {
	if m.detailExpanded {
		return m.renderExpandedDetail(contentHeight)
	}

	browseWidth, detailWidth, stacked := m.paneWidths()
	if stacked {
		if contentHeight < minimumStackedContentHeight {
			if m.focus == focusDetail {
				return m.renderDetailPane(detailWidth, contentHeight)
			}
			return m.renderBrowsePane(browseWidth, contentHeight)
		}
		browseHeight, detailHeight := stackedPaneHeights(contentHeight)
		if detailHeight == 0 {
			if m.focus == focusDetail {
				return m.renderDetailPane(detailWidth, browseHeight)
			}
			return m.renderBrowsePane(browseWidth, browseHeight)
		}
		browse := m.renderBrowsePane(browseWidth, browseHeight)
		detail := m.renderDetailPane(detailWidth, detailHeight)
		return render.JoinUIVertical(browse, detail)
	}

	browse := m.renderBrowsePane(browseWidth, contentHeight)
	detail := m.renderDetailPane(detailWidth, contentHeight)
	return render.JoinUIHorizontal(browse, detail)
}

func (m browserModel) renderExpandedDetail(contentHeight int) string {
	return m.renderDetailPane(m.width, contentHeight)
}

func (m browserModel) renderBrowsePane(width, height int) string {
	title := fmt.Sprintf("Issues · %s", strings.ToUpper(string(m.view)))
	if m.view == viewList && m.listData.Total > 0 {
		title = fmt.Sprintf("Issues · %s · %d/%d", strings.ToUpper(string(m.view)), len(m.listData.Issues), m.listData.Total)
	}
	content := m.renderBrowseContent(max(width-4, 1), max(height-3, 1))
	return render.RenderUIPane(title, content, width, height, m.focus == focusBrowse && !m.detailExpanded)
}

func (m browserModel) renderDetailPane(width, height int) string {
	title := "Detail"
	if detailID := m.currentDetailTargetID(); detailID != 0 {
		title = fmt.Sprintf("Detail · %s", model.FormatID(detailID))
		if m.focus == focusDetail || m.detailExpanded {
			title += " · " + strings.ToUpper(string(m.detailFocus))
		}
	}
	content := m.renderDetailContent(max(width-4, 1), max(height-3, 1))
	return render.RenderUIPane(title, content, width, height, m.focus == focusDetail || m.detailExpanded)
}

func (m browserModel) renderBrowseContent(width, height int) string {
	if m.loading {
		return render.RenderUIDimText("Loading issues...")
	}
	if m.errMsg != "" {
		return render.RenderUIErrorText(m.errMsg)
	}

	switch m.view {
	case viewBoard:
		return m.renderBoard(width, height)
	default:
		return m.renderList(width, height)
	}
}

func (m browserModel) renderList(width, height int) string {
	issues := m.listData.Issues
	if len(issues) == 0 {
		return render.RenderUIDimText("No issues found.")
	}

	start, end := windowBounds(m.listIndex, len(issues), height)
	rows := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		rows = append(rows, render.RenderUIListRow(issues[i], m.listHierarchyDecoration(issues[i]), width, i == m.listIndex))
	}
	return strings.Join(rows, "\n")
}

func (m browserModel) renderBoard(width, height int) string {
	if len(m.boardColumns) == 0 {
		return render.RenderUIDimText("No issues on the board.")
	}

	colWidth := max((width-(len(m.boardColumns)-1))/len(m.boardColumns), 18)
	cols := make([]string, 0, len(m.boardColumns))
	decorations := m.boardHierarchyDecorations()
	for idx, col := range m.boardColumns {
		selected := idx == m.boardColumnIdx
		cols = append(cols, render.RenderUIBoardColumn(col.Status, col.Issues, decorations, colWidth, height, selected, m.focus == focusBrowse, m.boardRowIdx))
	}
	return render.JoinUIHorizontal(cols...)
}

func (m browserModel) renderDetailContent(width, height int) string {
	if m.currentDetailTargetID() == 0 {
		return render.RenderUIDimText("Select an issue to inspect it.")
	}
	if m.loadingDetail || m.detailIssueID != m.currentDetailTargetID() {
		return render.RenderUIDimText("Loading issue detail...")
	}
	if m.detailErr != "" {
		return render.RenderUIErrorText(m.detailErr)
	}

	bodyHeight, subIssueHeight := m.detailContentHeightsForHeight(height)
	body := m.renderDetailBody(width, bodyHeight)
	if subIssueHeight == 0 || len(m.detailData.SubIssues) == 0 {
		return body
	}

	subIssues := m.renderDetailSubIssues(width, subIssueHeight)
	return strings.Join([]string{body, "", subIssues}, "\n")
}

func (m browserModel) renderDetailBody(width, height int) string {
	detail := render.RenderDetail(
		m.detailData.Issue,
		nil,
		m.detailData.Relations,
		m.detailData.Comments,
		m.detailData.Activity,
	)
	wrapped := render.WrapUIContent(detail, width)
	lines := strings.Split(wrapped, "\n")
	start, end := detailWindow(m.detailScroll, len(lines), height)
	visible := lines[start:end]
	if len(visible) == 0 {
		return ""
	}
	return strings.Join(visible, "\n")
}

func (m browserModel) renderDetailSubIssues(width, height int) string {
	doneCount := 0
	for _, subIssue := range m.detailData.SubIssues {
		if subIssue.Status == model.StatusDone {
			doneCount++
		}
	}

	header := render.RenderUIDetailSubIssuesHeader(doneCount, len(m.detailData.SubIssues), m.detailFocus == detailFocusSubIssues && (m.focus == focusDetail || m.detailExpanded))
	rowHeight := max(height-1, 1)
	start, end := windowBounds(m.detailSubIndex, len(m.detailData.SubIssues), rowHeight)
	rows := []string{header}
	for i := start; i < end; i++ {
		rows = append(rows, render.RenderUIDetailSubIssueRow(m.detailData.SubIssues[i], width, i == m.detailSubIndex, m.detailFocus == detailFocusSubIssues && (m.focus == focusDetail || m.detailExpanded)))
	}
	return strings.Join(rows, "\n")
}

func (m browserModel) renderHelp() string {
	content := strings.Join([]string{
		"docket tui",
		"",
		"1        list view",
		"2        board view",
		"s        cycle list sort field",
		"S        toggle list sort direction",
		"j / k    move in focused pane",
		"J / K    move detail from browse pane",
		"o        drill into selected epic",
		"h / l    switch board column or detail region",
		"tab      switch browse/detail pane",
		"enter    expand detail or open selected sub-issue",
		"u        go to parent / back",
		"ctrl+u/d half-page in focused detail region",
		"esc      collapse expanded detail, then back out",
		"r        refresh current view",
		"p        pause/resume auto-refresh",
		"?        toggle help",
		"q        quit",
		"",
		"This preview is read-only.",
	}, "\n")

	box := render.RenderUIModal("Help", content, min(max(m.width-8, 40), 72), min(max(m.height-6, 14), 20))
	return render.PlaceUICentered(m.width, m.height, box)
}

func (m browserModel) refreshStatusValue() render.RefreshStatus {
	return render.RefreshStatus{
		Enabled:     m.refreshPolicy.Enabled,
		Pending:     m.refreshState.Pending,
		LastSuccess: m.refreshState.LastSuccess,
		LastError:   m.refreshState.LastError,
		Interval:    m.refreshPolicy.Interval,
	}
}

func (m browserModel) paneWidths() (browseWidth, detailWidth int, stacked bool) {
	if m.width < 110 {
		return m.width, m.width, true
	}
	browseWidth = max(int(float64(m.width)*0.44), 36)
	detailWidth = max(m.width-browseWidth, 32)
	return browseWidth, detailWidth, false
}

func (m browserModel) detailPaneDimensions() (width, height int) {
	contentHeight := m.availableContentHeight()
	if m.detailExpanded {
		return m.width, contentHeight
	}
	_, detailWidth, stacked := m.paneWidths()
	if stacked {
		if contentHeight < minimumStackedContentHeight {
			if m.focus == focusDetail {
				return detailWidth, contentHeight
			}
			return detailWidth, 0
		}
		_, detailHeight := stackedPaneHeights(contentHeight)
		return detailWidth, detailHeight
	}
	return detailWidth, contentHeight
}

func (m browserModel) availableContentHeight() int {
	return max(m.height-uiLineCount(m.renderHeader())-uiLineCount(m.renderFooter()), 1)
}

func stackedPaneHeights(contentHeight int) (browseHeight, detailHeight int) {
	if contentHeight < minimumStackedContentHeight {
		return max(contentHeight, 1), 0
	}
	if contentHeight <= 1 {
		return max(contentHeight, 1), 0
	}
	if contentHeight < 12 {
		browseHeight = (contentHeight + 1) / 2
		return browseHeight, contentHeight - browseHeight
	}
	browseHeight = max(contentHeight/2, 6)
	return browseHeight, contentHeight - browseHeight
}

const minimumStackedContentHeight = 8

func (m browserModel) maxDetailScroll(visibleLines int) int {
	width, _ := m.detailPaneDimensions()
	innerWidth := max(width-4, 1)
	if m.detailIssueID == 0 || m.detailIssueID != m.currentDetailTargetID() || m.detailErr != "" {
		return 0
	}
	detail := render.RenderDetail(
		m.detailData.Issue,
		nil,
		m.detailData.Relations,
		m.detailData.Comments,
		m.detailData.Activity,
	)
	wrapped := render.WrapUIContent(detail, innerWidth)
	lines := strings.Split(wrapped, "\n")
	return max(len(lines)-visibleLines, 0)
}

func (m browserModel) detailContentHeights() (bodyHeight, subIssueHeight int) {
	_, detailHeight := m.detailPaneDimensions()
	return m.detailContentHeightsForHeight(max(detailHeight-4, 1))
}

func (m browserModel) detailContentHeightsForHeight(height int) (bodyHeight, subIssueHeight int) {
	bodyHeight = max(height, 1)
	if len(m.detailData.SubIssues) == 0 || height < 6 {
		return bodyHeight, 0
	}
	maxSubIssueHeight := max(height/3, 4)
	subIssueHeight = min(len(m.detailData.SubIssues)+1, maxSubIssueHeight)
	bodyHeight = max(height-subIssueHeight-1, 2)
	return bodyHeight, subIssueHeight
}

func (m browserModel) listHierarchyDecoration(issue *model.Issue) render.HierarchyDecoration {
	decoration := render.HierarchyDecoration{
		IsEpic:  issue.Kind == model.IssueKindEpic,
		IsChild: issue.ParentID != nil,
	}
	if prog, ok := m.listData.Progress[issue.ID]; ok {
		decoration.ChildCount = prog.Total
		decoration.Done = prog.Done
		decoration.Total = prog.Total
	}
	return decoration
}

func (m browserModel) boardHierarchyDecorations() map[int]render.HierarchyDecoration {
	decorations := make(map[int]render.HierarchyDecoration, len(m.boardData.Issues))
	for _, issue := range m.boardData.Issues {
		decoration := render.HierarchyDecoration{
			IsEpic:  issue.Kind == model.IssueKindEpic,
			IsChild: issue.ParentID != nil,
		}
		if prog, ok := m.boardData.Progress[issue.ID]; ok {
			decoration.ChildCount = prog.Total
			decoration.Done = prog.Done
			decoration.Total = prog.Total
		}
		decorations[issue.ID] = decoration
	}
	return decorations
}
