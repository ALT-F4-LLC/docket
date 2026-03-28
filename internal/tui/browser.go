package tui

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/sys/unix"

	"github.com/ALT-F4-LLC/docket/internal/app"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/render"
)

type viewMode string

const (
	viewList  viewMode = "list"
	viewBoard viewMode = "board"
)

type paneFocus string

const (
	focusBrowse paneFocus = "browse"
	focusDetail paneFocus = "detail"
)

type detailFocusRegion string

const (
	detailFocusBody      detailFocusRegion = "body"
	detailFocusSubIssues detailFocusRegion = "subissues"
)

const defaultListLimit = 50

type detailNavState struct {
	issueID       int
	scroll        int
	subIssueIndex int
	focusRegion   detailFocusRegion
}

type boardColumn struct {
	Status model.Status
	Issues []*model.Issue
}

type listLoadedMsg struct {
	view      viewMode
	requestID int
	data      app.IssueListData
	err       error
}

type boardLoadedMsg struct {
	view      viewMode
	requestID int
	data      app.BoardData
	err       error
}

type detailLoadedMsg struct {
	requestID int
	id        int
	data      app.IssueDetailData
	err       error
}

type browserModel struct {
	conn        *sql.DB
	projectName string

	width  int
	height int

	view           viewMode
	focus          paneFocus
	showHelp       bool
	detailExpanded bool

	loading       bool
	loadingDetail bool
	errMsg        string
	detailErr     string

	listData     app.IssueListData
	boardData    app.BoardData
	boardColumns []boardColumn

	selectedIssueID int
	listIndex       int
	boardColumnIdx  int
	boardRowIdx     int
	detailScroll    int
	detailTargetID  int
	detailIssueID   int
	detailData      app.IssueDetailData
	detailFocus     detailFocusRegion
	detailSubIndex  int
	detailHistory   []detailNavState
	viewRequestID   int
	detailRequestID int

	lastRefreshed time.Time
}

func NewBrowser(conn *sql.DB, docketDir string) tea.Model {
	projectName := filepath.Base(filepath.Dir(docketDir))
	if projectName == "." || projectName == string(filepath.Separator) || projectName == "" {
		projectName = "docket"
	}

	return browserModel{
		conn:          conn,
		projectName:   projectName,
		view:          viewList,
		focus:         focusBrowse,
		detailFocus:   detailFocusBody,
		loading:       true,
		viewRequestID: 1,
	}
}

func (m browserModel) Init() tea.Cmd {
	return loadViewCmd(m.conn, m.view, m.viewRequestID)
}

func (m browserModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		debugLogf("window size: %dx%d", msg.Width, msg.Height)
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case listLoadedMsg:
		debugLogf("list loaded: request=%d current=%d view=%s currentView=%s err=%v total=%d", msg.requestID, m.viewRequestID, msg.view, m.view, msg.err, msg.data.Total)
		if msg.requestID != m.viewRequestID || msg.view != m.view {
			debugLogf("ignoring stale list load")
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.errMsg = msg.err.Error()
			m.listData = app.IssueListData{}
			return m, nil
		}
		m.errMsg = ""
		m.listData = msg.data
		m.reconcileListSelection()
		m.resetDetailNavigationToBrowseSelection()
		m.lastRefreshed = time.Now()
		cmd := m.beginDetailLoad()
		return m, tea.Batch(cmd, flushPendingInputCmd("list-loaded"))
	case boardLoadedMsg:
		debugLogf("board loaded: request=%d current=%d view=%s currentView=%s err=%v total=%d", msg.requestID, m.viewRequestID, msg.view, m.view, msg.err, msg.data.Total)
		if msg.requestID != m.viewRequestID || msg.view != m.view {
			debugLogf("ignoring stale board load")
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.errMsg = msg.err.Error()
			m.boardData = app.BoardData{}
			m.boardColumns = nil
			return m, nil
		}
		m.errMsg = ""
		m.boardData = msg.data
		m.boardColumns = groupBoardColumns(msg.data.Issues)
		m.reconcileBoardSelection()
		m.resetDetailNavigationToBrowseSelection()
		m.lastRefreshed = time.Now()
		cmd := m.beginDetailLoad()
		return m, tea.Batch(cmd, flushPendingInputCmd("board-loaded"))
	case detailLoadedMsg:
		debugLogf("detail loaded: request=%d current=%d id=%d target=%d err=%v", msg.requestID, m.detailRequestID, msg.id, m.currentDetailTargetID(), msg.err)
		if msg.requestID != m.detailRequestID || msg.id != m.currentDetailTargetID() {
			debugLogf("ignoring stale detail load")
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
		return m, flushPendingInputCmd("detail-loaded")
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

func (m browserModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading docket ui..."
	}

	if m.showHelp {
		return m.renderHelp()
	}

	header := m.renderHeader()
	footer := m.renderFooter()
	contentHeight := max(m.height-2, 1)
	content := m.renderContent(contentHeight)

	return render.JoinUIVertical(header, content, footer)
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
		cmd := m.beginViewLoad()
		return m, cmd
	case "1":
		if m.view == viewList {
			return m, nil
		}
		m.view = viewList
		m.focus = focusBrowse
		cmd := m.beginViewLoad()
		return m, cmd
	case "2":
		if m.view == viewBoard {
			return m, nil
		}
		m.view = viewBoard
		m.focus = focusBrowse
		cmd := m.beginViewLoad()
		return m, cmd
	case "tab":
		if m.selectedIssueID == 0 {
			return m, nil
		}
		if m.detailExpanded {
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
	cmd := m.beginDetailLoad()
	return m, cmd
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
		if m.detailExpanded {
			m.focus = focusDetail
		} else {
			m.focus = focusDetail
		}
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

func (m browserModel) renderHeader() string {
	return render.RenderUIHeaderBar(m.projectName, string(m.view), m.lastRefreshed, m.width)
}

func (m browserModel) renderFooter() string {
	return render.RenderUIFooterBar(m.detailExpanded, m.focus == focusDetail, m.focus == focusBrowse && !m.detailExpanded, m.width)
}

func (m browserModel) renderContent(contentHeight int) string {
	if m.detailExpanded {
		return m.renderExpandedDetail(contentHeight)
	}

	browseWidth, detailWidth, stacked := m.paneWidths()
	if stacked {
		browseHeight := max(contentHeight/2, 6)
		detailHeight := max(contentHeight-browseHeight, 6)
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
		rows = append(rows, render.RenderUIListRow(issues[i], width, i == m.listIndex))
	}
	return strings.Join(rows, "\n")
}

func (m browserModel) renderBoard(width, height int) string {
	if len(m.boardColumns) == 0 {
		return render.RenderUIDimText("No issues on the board.")
	}

	colWidth := max((width-(len(m.boardColumns)-1))/len(m.boardColumns), 18)
	cols := make([]string, 0, len(m.boardColumns))
	for idx, col := range m.boardColumns {
		selected := idx == m.boardColumnIdx
		cols = append(cols, render.RenderUIBoardColumn(col.Status, col.Issues, colWidth, height, selected, m.focus == focusBrowse, m.boardRowIdx))
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
		"docket ui",
		"",
		"1        list view",
		"2        board view",
		"j / k    move in focused pane",
		"J / K    move detail from browse pane",
		"h / l    switch board column or detail region",
		"tab      switch browse/detail pane",
		"enter    expand detail or open selected sub-issue",
		"u        go to parent / back",
		"ctrl+u/d half-page in focused detail region",
		"esc      collapse expanded detail, then back out",
		"r        refresh current view",
		"?        toggle help",
		"q        quit",
		"",
		"This preview is read-only.",
	}, "\n")

	box := render.RenderUIModal("Help", content, min(max(m.width-8, 40), 72), min(max(m.height-6, 14), 20))
	return render.PlaceUICentered(m.width, m.height, box)
}

func (m *browserModel) beginViewLoad() tea.Cmd {
	m.loading = true
	m.errMsg = ""
	m.viewRequestID++
	return loadViewCmd(m.conn, m.view, m.viewRequestID)
}

func (m *browserModel) beginDetailLoad() tea.Cmd {
	targetID := m.currentDetailTargetID()
	if targetID == 0 {
		m.loadingDetail = false
		m.detailIssueID = 0
		m.detailData = app.IssueDetailData{}
		return nil
	}
	m.loadingDetail = true
	m.detailErr = ""
	m.detailRequestID++
	return loadDetailCmd(m.conn, targetID, m.detailRequestID)
}

func (m *browserModel) resetDetailNavigationToBrowseSelection() {
	m.detailTargetID = m.selectedIssueID
	m.detailScroll = 0
	m.detailFocus = detailFocusBody
	m.detailSubIndex = 0
	m.detailHistory = nil
}

func (m *browserModel) normalizeDetailState() {
	if len(m.detailData.SubIssues) == 0 {
		m.detailFocus = detailFocusBody
		m.detailSubIndex = 0
		return
	}
	m.detailSubIndex = clamp(m.detailSubIndex, 0, len(m.detailData.SubIssues)-1)
}

func (m browserModel) currentDetailTargetID() int {
	if m.detailTargetID != 0 {
		return m.detailTargetID
	}
	return m.selectedIssueID
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

func (m *browserModel) reconcileListSelection() {
	issues := m.listData.Issues
	if len(issues) == 0 {
		m.selectedIssueID = 0
		m.listIndex = 0
		m.detailIssueID = 0
		m.detailScroll = 0
		return
	}

	if idx := findIssueIndex(issues, m.selectedIssueID); idx >= 0 {
		m.listIndex = idx
		m.selectedIssueID = issues[idx].ID
		return
	}

	m.listIndex = 0
	m.selectedIssueID = issues[0].ID
	if m.focus == focusDetail {
		m.focus = focusBrowse
	}
}

func (m *browserModel) reconcileBoardSelection() {
	if len(m.boardColumns) == 0 {
		m.selectedIssueID = 0
		m.boardColumnIdx = 0
		m.boardRowIdx = 0
		m.detailIssueID = 0
		m.detailScroll = 0
		return
	}

	if colIdx, rowIdx := findBoardIssue(m.boardColumns, m.selectedIssueID); colIdx >= 0 {
		m.boardColumnIdx = colIdx
		m.boardRowIdx = rowIdx
		m.selectedIssueID = m.boardColumns[colIdx].Issues[rowIdx].ID
		return
	}

	m.boardColumnIdx = 0
	m.boardRowIdx = 0
	m.selectedIssueID = m.boardColumns[0].Issues[0].ID
	if m.focus == focusDetail {
		m.focus = focusBrowse
	}
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

func (m browserModel) selectedIssue() *model.Issue {
	switch m.view {
	case viewBoard:
		if len(m.boardColumns) == 0 {
			return nil
		}
		col := m.boardColumns[m.boardColumnIdx]
		if len(col.Issues) == 0 {
			return nil
		}
		return col.Issues[m.boardRowIdx]
	default:
		if len(m.listData.Issues) == 0 {
			return nil
		}
		return m.listData.Issues[m.listIndex]
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
	contentHeight := max(m.height-2, 1)
	if m.detailExpanded {
		return m.width, contentHeight
	}
	browseWidth, detailWidth, stacked := m.paneWidths()
	_ = browseWidth
	if stacked {
		browseHeight := max(contentHeight/2, 6)
		return detailWidth, max(contentHeight-browseHeight, 6)
	}
	return detailWidth, contentHeight
}

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

func loadViewCmd(conn *sql.DB, view viewMode, requestID int) tea.Cmd {
	debugLogf("load view cmd: view=%s request=%d", view, requestID)
	switch view {
	case viewBoard:
		return loadBoardCmd(conn, requestID)
	default:
		return loadListCmd(conn, requestID)
	}
}

func loadListCmd(conn *sql.DB, requestID int) tea.Cmd {
	return func() tea.Msg {
		debugLogf("running list query: request=%d", requestID)
		data, err := app.ListIssues(conn, app.ListIssuesParams{Limit: defaultListLimit})
		return listLoadedMsg{view: viewList, requestID: requestID, data: data, err: err}
	}
}

func loadBoardCmd(conn *sql.DB, requestID int) tea.Cmd {
	return func() tea.Msg {
		debugLogf("running board query: request=%d", requestID)
		data, err := app.LoadBoard(conn, app.BoardParams{})
		return boardLoadedMsg{view: viewBoard, requestID: requestID, data: data, err: err}
	}
}

func loadDetailCmd(conn *sql.DB, id int, requestID int) tea.Cmd {
	return func() tea.Msg {
		debugLogf("running detail query: request=%d id=%d", requestID, id)
		data, err := app.GetIssueDetail(conn, id)
		return detailLoadedMsg{requestID: requestID, id: id, data: data, err: err}
	}
}

func debugLogf(format string, args ...any) {
	path := os.Getenv("DOCKET_UI_DEBUG_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	stamp := time.Now().Format("2006-01-02 15:04:05.000")
	_, _ = fmt.Fprintf(f, "%s %s\n", stamp, fmt.Sprintf(format, args...))
}

func flushPendingInputCmd(reason string) tea.Cmd {
	return func() tea.Msg {
		const flushReadWrite = 0x3
		delays := []time.Duration{25 * time.Millisecond, 150 * time.Millisecond, 500 * time.Millisecond}
		for i, delay := range delays {
			time.Sleep(delay)
			if err := unix.IoctlSetPointerInt(int(os.Stdin.Fd()), uint(syscall.TIOCFLUSH), flushReadWrite); err != nil {
				debugLogf("input flush failed (%s #%d): %v", reason, i+1, err)
				continue
			}
			debugLogf("flushed tty input (%s #%d)", reason, i+1)
		}
		return nil
	}
}

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
