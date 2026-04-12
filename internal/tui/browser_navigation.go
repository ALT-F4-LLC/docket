package tui

import tea "github.com/charmbracelet/bubbletea"

func (m *browserModel) resetDetailNavigationToBrowseSelection() {
	m.detailTargetID = m.selectedIssueID
	m.detailScroll = 0
	m.detailFocus = detailFocusBody
	m.detailSubIndex = 0
	m.detailHistory = nil
	m.pendingDetailFocus = ""
}

func (m *browserModel) normalizeDetailState() {
	if len(m.detailData.SubIssues) == 0 {
		m.detailFocus = detailFocusBody
		m.detailSubIndex = 0
		return
	}
	m.detailSubIndex = clamp(m.detailSubIndex, 0, len(m.detailData.SubIssues)-1)
}

func (m *browserModel) navigateDetailToIssue(id int, pushCurrent bool) tea.Cmd {
	currentID := m.currentDetailTargetID()
	if id == 0 || id == currentID {
		return nil
	}
	if pushCurrent && currentID != 0 {
		m.detailHistory = append(m.detailHistory, detailNavState{
			issueID:       currentID,
			scroll:        m.detailScroll,
			subIssueIndex: m.detailSubIndex,
			focusRegion:   m.detailFocus,
		})
	}
	m.detailTargetID = id
	m.detailScroll = 0
	m.detailFocus = detailFocusBody
	m.detailSubIndex = 0
	m.pendingDetailFocus = ""
	return m.beginDetailLoad()
}

func (m *browserModel) navigateDetailBack() (tea.Cmd, bool) {
	if len(m.detailHistory) == 0 {
		return nil, false
	}
	prev := m.detailHistory[len(m.detailHistory)-1]
	m.detailHistory = m.detailHistory[:len(m.detailHistory)-1]
	m.detailTargetID = prev.issueID
	m.detailScroll = prev.scroll
	m.detailSubIndex = prev.subIssueIndex
	m.detailFocus = prev.focusRegion
	return m.beginDetailLoad(), true
}

func (m *browserModel) navigateDetailParentOrBack() (tea.Cmd, bool) {
	if cmd, ok := m.navigateDetailBack(); ok {
		return cmd, true
	}
	if m.detailData.Issue == nil || m.detailData.Issue.ParentID == nil {
		return nil, false
	}
	return m.navigateDetailToIssue(*m.detailData.Issue.ParentID, true), true
}

func (m *browserModel) openSelectedSubIssue() (tea.Cmd, bool) {
	if len(m.detailData.SubIssues) == 0 {
		return nil, false
	}
	child := m.detailData.SubIssues[clamp(m.detailSubIndex, 0, len(m.detailData.SubIssues)-1)]
	return m.navigateDetailToIssue(child.ID, true), true
}

func (m browserModel) selectedIssueHasHierarchy() bool {
	if issue := m.selectedIssue(); issue == nil {
		return false
	}
	if m.detailIssueID == m.selectedIssueID && m.currentDetailTargetID() == m.selectedIssueID && len(m.detailData.SubIssues) > 0 {
		return true
	}
	switch m.view {
	case viewBoard:
		prog, ok := m.boardData.Progress[m.selectedIssueID]
		return ok && prog.Total > 0
	default:
		prog, ok := m.listData.Progress[m.selectedIssueID]
		return ok && prog.Total > 0
	}
}

func (m *browserModel) enterSelectedIssueHierarchy() tea.Cmd {
	if !m.selectedIssueHasHierarchy() || m.selectedIssueID == 0 {
		return nil
	}
	m.focus = focusDetail
	m.pendingDetailFocus = detailFocusSubIssues
	if m.currentDetailTargetID() != m.selectedIssueID {
		m.resetDetailNavigationToBrowseSelection()
		m.focus = focusDetail
		m.pendingDetailFocus = detailFocusSubIssues
		return m.beginDetailLoad()
	}
	if m.loadingDetail || m.detailIssueID != m.selectedIssueID {
		return nil
	}
	m.detailFocus = detailFocusSubIssues
	m.pendingDetailFocus = ""
	return nil
}

func (m *browserModel) reconcileListSelection() {
	previousID := m.selectedIssueID
	issues := m.listData.Issues
	if len(issues) == 0 {
		m.selectedIssueID = 0
		m.listIndex = 0
		m.detailIssueID = 0
		m.detailScroll = 0
		debugEventf("selection_reconciled", "view=list selected_issue_id=0")
		return
	}

	if idx := findIssueIndex(issues, m.selectedIssueID); idx >= 0 {
		m.listIndex = idx
		m.selectedIssueID = issues[idx].ID
		debugEventf("selection_reconciled", "view=list selected_issue_id=%d list_index=%d", m.selectedIssueID, m.listIndex)
		m.reconcileDetailTarget(previousID)
		return
	}

	m.listIndex = 0
	m.selectedIssueID = issues[0].ID
	if m.focus == focusDetail {
		m.focus = focusBrowse
	}
	debugEventf("selection_reconciled", "view=list selected_issue_id=%d list_index=%d", m.selectedIssueID, m.listIndex)
	m.reconcileDetailTarget(previousID)
}

func (m *browserModel) reconcileBoardSelection() {
	previousID := m.selectedIssueID
	if len(m.boardColumns) == 0 {
		m.selectedIssueID = 0
		m.boardColumnIdx = 0
		m.boardRowIdx = 0
		m.detailIssueID = 0
		m.detailScroll = 0
		debugEventf("selection_reconciled", "view=board selected_issue_id=0")
		return
	}

	if colIdx, rowIdx := findBoardIssue(m.boardColumns, m.selectedIssueID); colIdx >= 0 {
		m.boardColumnIdx = colIdx
		m.boardRowIdx = rowIdx
		m.selectedIssueID = m.boardColumns[colIdx].Issues[rowIdx].ID
		debugEventf("selection_reconciled", "view=board selected_issue_id=%d column_index=%d row_index=%d", m.selectedIssueID, m.boardColumnIdx, m.boardRowIdx)
		m.reconcileDetailTarget(previousID)
		return
	}

	m.boardColumnIdx = 0
	m.boardRowIdx = 0
	m.selectedIssueID = m.boardColumns[0].Issues[0].ID
	if m.focus == focusDetail {
		m.focus = focusBrowse
	}
	debugEventf("selection_reconciled", "view=board selected_issue_id=%d column_index=%d row_index=%d", m.selectedIssueID, m.boardColumnIdx, m.boardRowIdx)
	m.reconcileDetailTarget(previousID)
}

func (m *browserModel) reconcileDetailTarget(previousSelectionID int) {
	if m.selectedIssueID == 0 {
		m.resetDetailNavigationToBrowseSelection()
		return
	}
	if len(m.detailHistory) > 0 {
		return
	}
	if m.detailTargetID == 0 {
		m.detailTargetID = m.selectedIssueID
		return
	}
	if m.detailTargetID != previousSelectionID {
		return
	}
	if m.selectedIssueID == previousSelectionID {
		return
	}
	m.resetDetailNavigationToBrowseSelection()
}

func (m *browserModel) moveListSelection(delta int) bool {
	issues := m.listData.Issues
	if len(issues) == 0 {
		return false
	}
	next := clamp(m.listIndex+delta, 0, len(issues)-1)
	if next == m.listIndex {
		return false
	}
	m.listIndex = next
	m.selectedIssueID = issues[next].ID
	m.detailScroll = 0
	return true
}

func (m *browserModel) moveBoardColumn(delta int) bool {
	if len(m.boardColumns) == 0 {
		return false
	}
	next := clamp(m.boardColumnIdx+delta, 0, len(m.boardColumns)-1)
	if next == m.boardColumnIdx {
		return false
	}
	m.boardColumnIdx = next
	col := m.boardColumns[next]
	m.boardRowIdx = clamp(m.boardRowIdx, 0, len(col.Issues)-1)
	m.selectedIssueID = col.Issues[m.boardRowIdx].ID
	m.detailScroll = 0
	return true
}

func (m *browserModel) moveBoardRow(delta int) bool {
	if len(m.boardColumns) == 0 {
		return false
	}
	col := m.boardColumns[m.boardColumnIdx]
	next := clamp(m.boardRowIdx+delta, 0, len(col.Issues)-1)
	if next == m.boardRowIdx {
		return false
	}
	m.boardRowIdx = next
	m.selectedIssueID = col.Issues[next].ID
	m.detailScroll = 0
	return true
}

func (m *browserModel) moveDetailSubIssueSelection(delta int) bool {
	if len(m.detailData.SubIssues) == 0 {
		return false
	}
	next := clamp(m.detailSubIndex+delta, 0, len(m.detailData.SubIssues)-1)
	if next == m.detailSubIndex {
		return false
	}
	m.detailSubIndex = next
	return true
}

func (m *browserModel) proxyDetailMovement(delta int) bool {
	if m.loadingDetail || m.detailErr != "" || m.detailIssueID != m.currentDetailTargetID() {
		return false
	}
	if m.detailFocus == detailFocusSubIssues {
		return m.moveDetailSubIssueSelection(delta)
	}
	bodyVisibleLines, _ := m.detailContentHeights()
	maxScroll := max(m.maxDetailScroll(bodyVisibleLines), 0)
	next := clamp(m.detailScroll+delta, 0, maxScroll)
	if next == m.detailScroll {
		return false
	}
	m.detailScroll = next
	return true
}
