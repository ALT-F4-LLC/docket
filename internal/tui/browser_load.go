package tui

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ALT-F4-LLC/docket/internal/app"
)

var (
	listIssues     = app.ListIssues
	loadBoard      = app.LoadBoard
	getIssueDetail = app.GetIssueDetail
)

func (m *browserModel) beginViewLoad() tea.Cmd {
	m.loading = true
	m.viewRequestID++
	m.refreshState.Pending = true
	return loadViewCmd(m.conn, m.view, m.listSort, m.viewRequestID)
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

func (m *browserModel) nextRefreshCmd() tea.Cmd {
	if !m.refreshPolicy.Enabled || m.refreshPolicy.Interval <= 0 {
		return nil
	}
	m.refreshTickID++
	return refreshTickCmd(m.refreshPolicy.Interval, m.refreshTickID)
}

func loadViewCmd(conn *sql.DB, view viewMode, listSort sortMode, requestID int) tea.Cmd {
	debugEventf("refresh_started", "view=%s sort_field=%s sort_dir=%s request_id=%d", view, listSort.Field, listSort.Dir, requestID)
	switch view {
	case viewBoard:
		return loadBoardCmd(conn, requestID)
	default:
		return loadListCmd(conn, listSort, requestID)
	}
}

func loadListCmd(conn *sql.DB, listSort sortMode, requestID int) tea.Cmd {
	return func() tea.Msg {
		debugEventf("refresh_query_list", "sort_field=%s sort_dir=%s request_id=%d", listSort.Field, listSort.Dir, requestID)
		data, err := listIssues(conn, app.ListIssuesParams{
			Sort:    listSort.Field,
			SortDir: listSort.Dir,
			Limit:   defaultListLimit,
		})
		return listLoadedMsg{view: viewList, requestID: requestID, data: data, err: err}
	}
}

func loadBoardCmd(conn *sql.DB, requestID int) tea.Cmd {
	return func() tea.Msg {
		debugEventf("refresh_query_board", "request_id=%d", requestID)
		data, err := loadBoard(conn, app.BoardParams{})
		return boardLoadedMsg{view: viewBoard, requestID: requestID, data: data, err: err}
	}
}

func loadDetailCmd(conn *sql.DB, id int, requestID int) tea.Cmd {
	return func() tea.Msg {
		debugEventf("detail_query", "request_id=%d issue_id=%d", requestID, id)
		data, err := getIssueDetail(conn, id)
		return detailLoadedMsg{requestID: requestID, id: id, data: data, err: err}
	}
}

func debugLogf(format string, args ...any) {
	path := os.Getenv("DOCKET_TUI_DEBUG_LOG")
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

func debugEventf(event string, format string, args ...any) {
	message := strings.TrimSpace(fmt.Sprintf(format, args...))
	if message == "" {
		debugLogf("event=%s", event)
		return
	}
	debugLogf("event=%s %s", event, message)
}

func refreshTickCmd(interval time.Duration, tickID int) tea.Cmd {
	if interval <= 0 {
		return nil
	}
	return tea.Tick(interval, func(time.Time) tea.Msg {
		return refreshTickMsg{tickID: tickID}
	})
}
