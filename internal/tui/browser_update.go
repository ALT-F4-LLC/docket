package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func (m browserModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		debugLogf("window size: %dx%d", msg.Width, msg.Height)
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case listLoadedMsg:
		return m.updateListLoaded(msg)
	case boardLoadedMsg:
		return m.updateBoardLoaded(msg)
	case detailLoadedMsg:
		return m.updateDetailLoaded(msg)
	case refreshTickMsg:
		return m.updateRefreshTick(msg)
	case tea.KeyMsg:
		debugLogf("key: %q view=%s focus=%s selected=%d expanded=%t", msg.String(), m.view, m.focus, m.selectedIssueID, m.detailExpanded)
		if isTerminalProbe(msg.String()) {
			debugLogf("ignoring terminal probe key: %q", msg.String())
			return m, nil
		}
		return m.handleKey(msg)
	default:
		return m, nil
	}
}

func (m browserModel) updateListLoaded(msg listLoadedMsg) (tea.Model, tea.Cmd) {
	debugLogf("list loaded: request=%d current=%d view=%s currentView=%s err=%v total=%d", msg.requestID, m.viewRequestID, msg.view, m.view, msg.err, msg.data.Total)
	if msg.requestID != m.viewRequestID || msg.view != m.view {
		debugEventf("stale_response_ignored", "kind=list request_id=%d current_request_id=%d view=%s current_view=%s", msg.requestID, m.viewRequestID, msg.view, m.view)
		return m, nil
	}
	m.loading = false
	m.refreshState.Pending = false
	if msg.err != nil {
		m.refreshState.LastError = msg.err.Error()
		debugEventf("refresh_failed", "view=%s request_id=%d err=%q", msg.view, msg.requestID, msg.err.Error())
		if !m.refreshState.LoadedOnce {
			m.errMsg = msg.err.Error()
			m.listData = listLoadedMsg{}.data
		}
		return m, m.nextRefreshCmd()
	}
	m.errMsg = ""
	m.refreshState.LoadedOnce = true
	m.refreshState.LastError = ""
	m.refreshState.LastSuccess = time.Now()
	m.listData = msg.data
	m.reconcileListSelection()
	debugEventf("refresh_completed", "view=%s request_id=%d issue_count=%d", msg.view, msg.requestID, len(msg.data.Issues))
	cmd := m.beginDetailLoad()
	return m, tea.Batch(cmd, m.nextRefreshCmd())
}

func (m browserModel) updateBoardLoaded(msg boardLoadedMsg) (tea.Model, tea.Cmd) {
	debugLogf("board loaded: request=%d current=%d view=%s currentView=%s err=%v total=%d", msg.requestID, m.viewRequestID, msg.view, m.view, msg.err, msg.data.Total)
	if msg.requestID != m.viewRequestID || msg.view != m.view {
		debugEventf("stale_response_ignored", "kind=board request_id=%d current_request_id=%d view=%s current_view=%s", msg.requestID, m.viewRequestID, msg.view, m.view)
		return m, nil
	}
	m.loading = false
	m.refreshState.Pending = false
	if msg.err != nil {
		m.refreshState.LastError = msg.err.Error()
		debugEventf("refresh_failed", "view=%s request_id=%d err=%q", msg.view, msg.requestID, msg.err.Error())
		if !m.refreshState.LoadedOnce {
			m.errMsg = msg.err.Error()
			m.boardData = boardLoadedMsg{}.data
			m.boardColumns = nil
		}
		return m, m.nextRefreshCmd()
	}
	m.errMsg = ""
	m.refreshState.LoadedOnce = true
	m.refreshState.LastError = ""
	m.refreshState.LastSuccess = time.Now()
	m.boardData = msg.data
	m.boardColumns = groupBoardColumns(msg.data.Issues)
	m.reconcileBoardSelection()
	debugEventf("refresh_completed", "view=%s request_id=%d issue_count=%d", msg.view, msg.requestID, len(msg.data.Issues))
	cmd := m.beginDetailLoad()
	return m, tea.Batch(cmd, m.nextRefreshCmd())
}

func (m browserModel) updateDetailLoaded(msg detailLoadedMsg) (tea.Model, tea.Cmd) {
	debugLogf("detail loaded: request=%d current=%d id=%d target=%d err=%v", msg.requestID, m.detailRequestID, msg.id, m.currentDetailTargetID(), msg.err)
	if msg.requestID != m.detailRequestID || msg.id != m.currentDetailTargetID() {
		debugEventf("stale_response_ignored", "kind=detail request_id=%d current_request_id=%d issue_id=%d target_issue_id=%d", msg.requestID, m.detailRequestID, msg.id, m.currentDetailTargetID())
		return m, nil
	}
	m.loadingDetail = false
	if msg.err != nil {
		m.detailErr = msg.err.Error()
		m.detailIssueID = 0
		return m, nil
	}
	m.detailErr = ""
	m.detailIssueID = msg.id
	m.detailData = msg.data
	m.normalizeDetailState()
	if m.pendingDetailFocus != "" {
		if m.pendingDetailFocus == detailFocusSubIssues && len(m.detailData.SubIssues) > 0 {
			m.detailFocus = detailFocusSubIssues
		} else {
			m.detailFocus = detailFocusBody
		}
		m.pendingDetailFocus = ""
	}
	return m, nil
}

func (m browserModel) updateRefreshTick(msg refreshTickMsg) (tea.Model, tea.Cmd) {
	if msg.tickID != m.refreshTickID {
		debugEventf("stale_response_ignored", "kind=refresh_tick tick_id=%d current_tick_id=%d", msg.tickID, m.refreshTickID)
		return m, nil
	}
	if !m.refreshPolicy.Enabled || m.loading {
		return m, nil
	}
	return m, m.beginViewLoad()
}

func (m browserModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" || key == "q" {
		return m, tea.Quit
	}

	if m.showHelp {
		switch key {
		case "?", "esc", "enter":
			m.showHelp = false
		}
		return m, nil
	}

	switch key {
	case "?":
		m.showHelp = true
		return m, nil
	case "r":
		return m, m.beginViewLoad()
	case "p":
		m.refreshPolicy.Enabled = !m.refreshPolicy.Enabled
		debugEventf("auto_refresh_toggled", "enabled=%t interval=%s", m.refreshPolicy.Enabled, m.refreshPolicy.Interval)
		if !m.refreshPolicy.Enabled {
			m.refreshTickID++
			return m, nil
		}
		return m, m.nextRefreshCmd()
	case "1":
		if m.view == viewList {
			return m, nil
		}
		m.view = viewList
		m.detailExpanded = false
		m.focus = focusBrowse
		m.resetDetailNavigationToBrowseSelection()
		return m, m.beginViewLoad()
	case "2":
		if m.view == viewBoard {
			return m, nil
		}
		m.view = viewBoard
		m.detailExpanded = false
		m.focus = focusBrowse
		m.resetDetailNavigationToBrowseSelection()
		return m, m.beginViewLoad()
	case "tab":
		if m.selectedIssueID == 0 || m.detailExpanded {
			return m, nil
		}
		if m.focus == focusBrowse {
			m.focus = focusDetail
		} else {
			m.focus = focusBrowse
		}
		return m, nil
	case "J":
		if m.focus == focusBrowse && !m.detailExpanded && m.proxyDetailMovement(1) {
			return m, nil
		}
	case "K":
		if m.focus == focusBrowse && !m.detailExpanded && m.proxyDetailMovement(-1) {
			return m, nil
		}
	}

	if m.focus == focusDetail || m.detailExpanded {
		return m.handleDetailKey(key)
	}

	return m.handleBrowseKey(key)
}

func (m browserModel) handleBrowseKey(key string) (tea.Model, tea.Cmd) {
	var changed bool

	switch m.view {
	case viewList:
		switch key {
		case "j", "down":
			changed = m.moveListSelection(1)
		case "k", "up":
			changed = m.moveListSelection(-1)
		case "s":
			if !m.cycleListSortField(1) {
				return m, nil
			}
			return m, m.beginViewLoad()
		case "S":
			m.toggleListSortDirection()
			return m, m.beginViewLoad()
		case "o":
			return m, m.enterSelectedIssueHierarchy()
		case "enter":
			if m.selectedIssueID == 0 {
				return m, nil
			}
			m.detailExpanded = !m.detailExpanded
			if m.detailExpanded {
				m.focus = focusDetail
			}
			return m, nil
		}
	case viewBoard:
		switch key {
		case "j", "down":
			changed = m.moveBoardRow(1)
		case "k", "up":
			changed = m.moveBoardRow(-1)
		case "o":
			return m, m.enterSelectedIssueHierarchy()
		case "h", "left":
			changed = m.moveBoardColumn(-1)
		case "l", "right":
			changed = m.moveBoardColumn(1)
		case "enter":
			if m.selectedIssueID == 0 {
				return m, nil
			}
			m.detailExpanded = !m.detailExpanded
			if m.detailExpanded {
				m.focus = focusDetail
			}
			return m, nil
		}
	}

	if !changed {
		return m, nil
	}

	m.resetDetailNavigationToBrowseSelection()
	return m, m.beginDetailLoad()
}

func (m browserModel) handleDetailKey(key string) (tea.Model, tea.Cmd) {
	bodyVisibleLines, subIssueVisibleRows := m.detailContentHeights()
	maxScroll := max(m.maxDetailScroll(bodyVisibleLines), 0)

	switch key {
	case "esc":
		if m.detailExpanded {
			m.detailExpanded = false
			m.focus = focusDetail
			return m, nil
		}
		if m.detailFocus == detailFocusSubIssues {
			m.detailFocus = detailFocusBody
			return m, nil
		}
		if cmd, ok := m.navigateDetailBack(); ok {
			return m, cmd
		}
		return m, nil
	case "u":
		if cmd, ok := m.navigateDetailParentOrBack(); ok {
			return m, cmd
		}
		return m, nil
	case "h", "left":
		if m.detailFocus == detailFocusSubIssues {
			m.detailFocus = detailFocusBody
		}
		return m, nil
	case "l", "right":
		if len(m.detailData.SubIssues) > 0 {
			m.detailFocus = detailFocusSubIssues
		}
		return m, nil
	case "enter":
		if m.detailFocus == detailFocusSubIssues {
			if cmd, ok := m.openSelectedSubIssue(); ok {
				return m, cmd
			}
			return m, nil
		}
		if m.currentDetailTargetID() == 0 {
			return m, nil
		}
		m.detailExpanded = !m.detailExpanded
		m.focus = focusDetail
		return m, nil
	case "j", "down":
		if m.detailFocus == detailFocusSubIssues {
			m.moveDetailSubIssueSelection(1)
			return m, nil
		}
		m.detailScroll = min(m.detailScroll+1, maxScroll)
	case "k", "up":
		if m.detailFocus == detailFocusSubIssues {
			m.moveDetailSubIssueSelection(-1)
			return m, nil
		}
		m.detailScroll = max(m.detailScroll-1, 0)
	case "pgdown":
		if m.detailFocus == detailFocusSubIssues {
			m.moveDetailSubIssueSelection(subIssueVisibleRows)
			return m, nil
		}
		m.detailScroll = min(m.detailScroll+bodyVisibleLines, maxScroll)
	case "pgup":
		if m.detailFocus == detailFocusSubIssues {
			m.moveDetailSubIssueSelection(-subIssueVisibleRows)
			return m, nil
		}
		m.detailScroll = max(m.detailScroll-bodyVisibleLines, 0)
	case "ctrl+d":
		if m.detailFocus == detailFocusSubIssues {
			m.moveDetailSubIssueSelection(max(subIssueVisibleRows/2, 1))
			return m, nil
		}
		m.detailScroll = min(m.detailScroll+max(bodyVisibleLines/2, 1), maxScroll)
	case "ctrl+u":
		if m.detailFocus == detailFocusSubIssues {
			m.moveDetailSubIssueSelection(-max(subIssueVisibleRows/2, 1))
			return m, nil
		}
		m.detailScroll = max(m.detailScroll-max(bodyVisibleLines/2, 1), 0)
	case "home":
		if m.detailFocus == detailFocusSubIssues {
			m.detailSubIndex = 0
			return m, nil
		}
		m.detailScroll = 0
	case "end":
		if m.detailFocus == detailFocusSubIssues {
			m.detailSubIndex = max(len(m.detailData.SubIssues)-1, 0)
			return m, nil
		}
		m.detailScroll = maxScroll
	}

	return m, nil
}
