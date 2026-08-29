package tui

import (
	"database/sql"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ALT-F4-LLC/docket/internal/app"
	"github.com/ALT-F4-LLC/docket/internal/model"
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

type sortMode struct {
	Field string
	Dir   string
}

var defaultListSort = sortMode{Field: "id", Dir: "desc"}

type refreshPolicy struct {
	Enabled  bool
	Interval time.Duration
}

type refreshState struct {
	Pending     bool
	LastSuccess time.Time
	LastError   string
	LoadedOnce  bool
}

const defaultRefreshInterval = 5 * time.Second

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

type refreshTickMsg struct {
	tickID int
}

type issueCopiedMsg struct {
	noticeID int
	issueID  int
	err      error
}

type copyNoticeExpiredMsg struct {
	noticeID int
}

type browserModel struct {
	conn        *sql.DB
	projectID   int
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

	listData      app.IssueListData
	boardData     app.BoardData
	boardColumns  []boardColumn
	listSort      sortMode
	refreshPolicy refreshPolicy
	refreshState  refreshState

	selectedIssueID    int
	listIndex          int
	boardColumnIdx     int
	boardRowIdx        int
	detailScroll       int
	detailTargetID     int
	detailIssueID      int
	detailData         app.IssueDetailData
	detailFocus        detailFocusRegion
	detailSubIndex     int
	detailHistory      []detailNavState
	pendingDetailFocus detailFocusRegion
	viewRequestID      int
	detailRequestID    int
	refreshTickID      int
	copyNotice         string
	copyNoticeID       int
}

func NewBrowser(conn *sql.DB, docketDir string, projectIDs ...int) tea.Model {
	projectName := filepath.Base(filepath.Dir(docketDir))
	if projectName == "." || projectName == string(filepath.Separator) || projectName == "" {
		projectName = "docket"
	}

	projectID := 0
	if len(projectIDs) > 0 {
		projectID = projectIDs[0]
	}

	return browserModel{
		conn:          conn,
		projectID:     projectID,
		projectName:   projectName,
		view:          viewList,
		focus:         focusBrowse,
		listSort:      defaultListSort,
		refreshPolicy: refreshPolicy{Enabled: true, Interval: defaultRefreshInterval},
		detailFocus:   detailFocusBody,
		loading:       true,
		viewRequestID: 1,
	}
}

func (m browserModel) currentDetailTargetID() int {
	if m.detailTargetID != 0 {
		return m.detailTargetID
	}
	return m.selectedIssueID
}

func (m browserModel) focusedIssueID() int {
	if m.focus == focusBrowse && !m.detailExpanded {
		return m.selectedIssueID
	}
	if m.detailFocus == detailFocusSubIssues && len(m.detailData.SubIssues) > 0 {
		return m.detailData.SubIssues[clamp(m.detailSubIndex, 0, len(m.detailData.SubIssues)-1)].ID
	}
	return m.currentDetailTargetID()
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
