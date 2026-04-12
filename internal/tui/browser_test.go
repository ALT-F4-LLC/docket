package tui

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ALT-F4-LLC/docket/internal/app"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/render"
)

func mustOpenBrowserDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Initialize(conn); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return conn
}

func createBrowserIssue(t *testing.T, conn *sql.DB, issue model.Issue) int {
	t.Helper()
	id, err := db.CreateIssue(conn, &issue, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue(%q): %v", issue.Title, err)
	}
	return id
}

func testIssue(id int, status model.Status) *model.Issue {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &model.Issue{
		ID:        id,
		Title:     model.FormatID(id) + " title",
		Status:    status,
		Priority:  model.PriorityMedium,
		Kind:      model.IssueKindTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func testIssues(startID, count int, status model.Status) []*model.Issue {
	issues := make([]*model.Issue, 0, count)
	for i := 0; i < count; i++ {
		issues = append(issues, testIssue(startID+i, status))
	}
	return issues
}

func batchCommandCount(cmd tea.Cmd) int {
	if cmd == nil {
		return 0
	}
	if msg, ok := cmd().(tea.BatchMsg); ok {
		return len(msg)
	}
	return 1
}

func testDetailModel(issue *model.Issue, subIssues ...*model.Issue) browserModel {
	return browserModel{
		width:           100,
		height:          30,
		focus:           focusDetail,
		selectedIssueID: issue.ID,
		detailTargetID:  issue.ID,
		detailIssueID:   issue.ID,
		detailFocus:     detailFocusBody,
		detailData: app.IssueDetailData{
			Issue:     issue,
			SubIssues: subIssues,
		},
	}
}

func TestMoveListSelection(t *testing.T) {
	m := browserModel{
		view:            viewList,
		focus:           focusBrowse,
		listData:        app.IssueListData{Issues: []*model.Issue{testIssue(1, model.StatusTodo), testIssue(2, model.StatusTodo)}},
		selectedIssueID: 1,
		listIndex:       0,
	}

	if !m.moveListSelection(1) {
		t.Fatalf("expected selection to move")
	}
	if m.listIndex != 1 || m.selectedIssueID != 2 {
		t.Fatalf("got index=%d id=%d, want 1/2", m.listIndex, m.selectedIssueID)
	}
	if m.moveListSelection(1) {
		t.Fatalf("expected move beyond end to be ignored")
	}
}

func TestNewBrowserStartsInListViewWithInitialLoad(t *testing.T) {
	model := NewBrowser(nil, "/tmp/example/.docket")
	browser, ok := model.(browserModel)
	if !ok {
		t.Fatalf("model = %T, want browserModel", model)
	}
	if browser.projectName != "example" {
		t.Fatalf("projectName = %q, want example", browser.projectName)
	}
	if browser.view != viewList {
		t.Fatalf("view = %s, want %s", browser.view, viewList)
	}
	if browser.focus != focusBrowse {
		t.Fatalf("focus = %s, want %s", browser.focus, focusBrowse)
	}
	if browser.listSort != defaultListSort {
		t.Fatalf("listSort = %#v, want %#v", browser.listSort, defaultListSort)
	}
	if !browser.loading {
		t.Fatal("expected initial loading state")
	}
	if browser.viewRequestID != 1 {
		t.Fatalf("viewRequestID = %d, want 1", browser.viewRequestID)
	}
	if browser.Init() == nil {
		t.Fatal("expected initial load command")
	}
}

func TestNewBrowserFallsBackToDefaultProjectName(t *testing.T) {
	model := NewBrowser(nil, "/.docket")
	browser := model.(browserModel)
	if browser.projectName != "docket" {
		t.Fatalf("projectName = %q, want docket", browser.projectName)
	}
}

func TestMoveBoardSelection(t *testing.T) {
	m := browserModel{
		view:  viewBoard,
		focus: focusBrowse,
		boardColumns: []boardColumn{
			{Status: model.StatusTodo, Issues: []*model.Issue{testIssue(1, model.StatusTodo), testIssue(2, model.StatusTodo)}},
			{Status: model.StatusReview, Issues: []*model.Issue{testIssue(3, model.StatusReview)}},
		},
		selectedIssueID: 1,
	}

	if !m.moveBoardRow(1) {
		t.Fatalf("expected board row move")
	}
	if m.boardRowIdx != 1 || m.selectedIssueID != 2 {
		t.Fatalf("got row=%d id=%d, want 1/2", m.boardRowIdx, m.selectedIssueID)
	}
	if !m.moveBoardColumn(1) {
		t.Fatalf("expected board column move")
	}
	if m.boardColumnIdx != 1 || m.boardRowIdx != 0 || m.selectedIssueID != 3 {
		t.Fatalf("got col=%d row=%d id=%d, want 1/0/3", m.boardColumnIdx, m.boardRowIdx, m.selectedIssueID)
	}
}

func TestSelectedIssueReturnsCurrentBrowseSelection(t *testing.T) {
	listIssue := testIssue(1, model.StatusTodo)
	boardIssue := testIssue(2, model.StatusReview)

	listModel := browserModel{
		view:      viewList,
		listData:  app.IssueListData{Issues: []*model.Issue{listIssue}},
		listIndex: 0,
	}
	if got := listModel.selectedIssue(); got == nil || got.ID != listIssue.ID {
		t.Fatalf("selectedIssue() = %#v, want %d", got, listIssue.ID)
	}

	boardModel := browserModel{
		view: viewBoard,
		boardColumns: []boardColumn{{
			Status: model.StatusReview,
			Issues: []*model.Issue{boardIssue},
		}},
	}
	if got := boardModel.selectedIssue(); got == nil || got.ID != boardIssue.ID {
		t.Fatalf("selectedIssue() = %#v, want %d", got, boardIssue.ID)
	}

	emptyModel := browserModel{view: viewList}
	if got := emptyModel.selectedIssue(); got != nil {
		t.Fatalf("selectedIssue() = %#v, want nil", got)
	}
}

func TestReconcileListSelectionKeepsMatchAndFallsBack(t *testing.T) {
	issues := []*model.Issue{testIssue(3, model.StatusTodo), testIssue(4, model.StatusReview)}

	keep := browserModel{
		focus:           focusBrowse,
		selectedIssueID: 4,
		listData:        app.IssueListData{Issues: issues},
	}
	keep.reconcileListSelection()
	if keep.listIndex != 1 || keep.selectedIssueID != 4 {
		t.Fatalf("kept selection = %d/%d, want 1/4", keep.listIndex, keep.selectedIssueID)
	}

	fallback := browserModel{
		focus:           focusDetail,
		selectedIssueID: 99,
		listData:        app.IssueListData{Issues: issues},
	}
	fallback.reconcileListSelection()
	if fallback.listIndex != 0 || fallback.selectedIssueID != 3 {
		t.Fatalf("fallback selection = %d/%d, want 0/3", fallback.listIndex, fallback.selectedIssueID)
	}
	if fallback.focus != focusBrowse {
		t.Fatalf("focus = %s, want %s", fallback.focus, focusBrowse)
	}

	empty := browserModel{
		selectedIssueID: 5,
		detailIssueID:   5,
		detailScroll:    3,
	}
	empty.reconcileListSelection()
	if empty.selectedIssueID != 0 || empty.listIndex != 0 || empty.detailIssueID != 0 || empty.detailScroll != 0 {
		t.Fatalf("empty reconcile = %#v", empty)
	}
}

func TestReconcileBoardSelectionFallsBackToFirstIssue(t *testing.T) {
	m := browserModel{
		view:            viewBoard,
		selectedIssueID: 99,
		boardColumns: []boardColumn{
			{Status: model.StatusBacklog, Issues: []*model.Issue{testIssue(5, model.StatusBacklog)}},
			{Status: model.StatusTodo, Issues: []*model.Issue{testIssue(6, model.StatusTodo)}},
		},
	}

	m.reconcileBoardSelection()
	if m.boardColumnIdx != 0 || m.boardRowIdx != 0 || m.selectedIssueID != 5 {
		t.Fatalf("got col=%d row=%d id=%d, want 0/0/5", m.boardColumnIdx, m.boardRowIdx, m.selectedIssueID)
	}
}

func TestReconcileBoardSelectionKeepsMatchAndClearsEmptyState(t *testing.T) {
	matched := browserModel{
		selectedIssueID: 6,
		boardColumns: []boardColumn{
			{Status: model.StatusBacklog, Issues: []*model.Issue{testIssue(5, model.StatusBacklog)}},
			{Status: model.StatusTodo, Issues: []*model.Issue{testIssue(6, model.StatusTodo)}},
		},
	}
	matched.reconcileBoardSelection()
	if matched.boardColumnIdx != 1 || matched.boardRowIdx != 0 || matched.selectedIssueID != 6 {
		t.Fatalf("matched selection = %d/%d/%d, want 1/0/6", matched.boardColumnIdx, matched.boardRowIdx, matched.selectedIssueID)
	}

	empty := browserModel{selectedIssueID: 5, detailIssueID: 5, detailScroll: 2}
	empty.reconcileBoardSelection()
	if empty.selectedIssueID != 0 || empty.boardColumnIdx != 0 || empty.boardRowIdx != 0 || empty.detailIssueID != 0 || empty.detailScroll != 0 {
		t.Fatalf("empty reconcile = %#v", empty)
	}
}

func TestStaleViewLoadMessagesAreIgnored(t *testing.T) {
	m := browserModel{
		view:          viewList,
		viewRequestID: 3,
		listData:      app.IssueListData{Issues: []*model.Issue{testIssue(1, model.StatusTodo)}},
	}

	updated, _ := m.Update(boardLoadedMsg{
		view:      viewBoard,
		requestID: 2,
		data: app.BoardData{Issues: []*model.Issue{
			testIssue(9, model.StatusReview),
		}},
	})

	next := updated.(browserModel)
	if next.view != viewList {
		t.Fatalf("expected view to remain list, got %s", next.view)
	}
	if len(next.boardColumns) != 0 {
		t.Fatalf("expected stale board payload to be ignored")
	}
}

func TestStaleListLoadMessagesAreIgnored(t *testing.T) {
	m := browserModel{
		view:          viewList,
		viewRequestID: 4,
		listData:      app.IssueListData{Issues: []*model.Issue{testIssue(1, model.StatusTodo)}},
	}

	updated, _ := m.Update(listLoadedMsg{
		view:      viewList,
		requestID: 3,
		data: app.IssueListData{Issues: []*model.Issue{
			testIssue(9, model.StatusDone),
		}},
	})

	next := updated.(browserModel)
	if len(next.listData.Issues) != 1 || next.listData.Issues[0].ID != 1 {
		t.Fatalf("stale list payload changed state: %#v", next.listData.Issues)
	}
	if next.view != viewList {
		t.Fatalf("expected view to remain list, got %s", next.view)
	}
	if next.viewRequestID != 4 {
		t.Fatalf("viewRequestID = %d, want 4", next.viewRequestID)
	}
}

func TestListLoadMessageUpdatesSelectionAndDetailTarget(t *testing.T) {
	m := browserModel{
		view:            viewList,
		viewRequestID:   2,
		loading:         true,
		selectedIssueID: 99,
		detailTargetID:  99,
	}

	updated, cmd := m.Update(listLoadedMsg{
		view:      viewList,
		requestID: 2,
		data: app.IssueListData{
			Issues: []*model.Issue{testIssue(5, model.StatusTodo), testIssue(6, model.StatusReview)},
			Total:  2,
		},
	})

	next := updated.(browserModel)
	if next.loading {
		t.Fatal("expected loading to stop")
	}
	if next.selectedIssueID != 5 || next.listIndex != 0 {
		t.Fatalf("selection = %d/%d, want 5/0", next.selectedIssueID, next.listIndex)
	}
	if next.detailTargetID != 5 {
		t.Fatalf("detailTargetID = %d, want 5", next.detailTargetID)
	}
	if next.refreshState.LastSuccess.IsZero() {
		t.Fatal("expected refresh success time to be set")
	}
	if cmd == nil {
		t.Fatal("expected follow-up detail load command")
	}
}

func TestRefreshTickReloadKeepsValidSelection(t *testing.T) {
	m := browserModel{
		view:            viewList,
		listSort:        defaultListSort,
		refreshPolicy:   refreshPolicy{Enabled: true, Interval: defaultRefreshInterval},
		refreshTickID:   4,
		viewRequestID:   7,
		selectedIssueID: 9,
		listIndex:       1,
		listData: app.IssueListData{Issues: []*model.Issue{
			testIssue(8, model.StatusTodo),
			testIssue(9, model.StatusInProgress),
		}},
		refreshState: refreshState{LoadedOnce: true},
	}

	updated, cmd := m.Update(refreshTickMsg{tickID: 4})
	afterTick := updated.(browserModel)
	if !afterTick.loading {
		t.Fatal("expected refresh tick to start a view load")
	}
	if !afterTick.refreshState.Pending {
		t.Fatal("expected refresh state to become pending")
	}
	if afterTick.viewRequestID != 8 {
		t.Fatalf("viewRequestID = %d, want 8", afterTick.viewRequestID)
	}
	if cmd == nil {
		t.Fatal("expected refresh tick to return load command")
	}

	updated, _ = afterTick.Update(listLoadedMsg{
		view:      viewList,
		requestID: 8,
		data: app.IssueListData{Issues: []*model.Issue{
			testIssue(9, model.StatusDone),
			testIssue(10, model.StatusTodo),
		}},
	})
	afterLoad := updated.(browserModel)
	if afterLoad.selectedIssueID != 9 {
		t.Fatalf("selectedIssueID = %d, want 9", afterLoad.selectedIssueID)
	}
	if afterLoad.listIndex != 0 {
		t.Fatalf("listIndex = %d, want 0", afterLoad.listIndex)
	}
	if afterLoad.refreshState.LastSuccess.IsZero() {
		t.Fatal("expected refresh success time to be set")
	}
	if afterLoad.refreshState.LastError != "" {
		t.Fatalf("LastError = %q, want empty", afterLoad.refreshState.LastError)
	}
}

func TestSuccessfulListLoadDoesNotScheduleInputFlush(t *testing.T) {
	m := browserModel{
		view:          viewList,
		viewRequestID: 2,
		loading:       true,
		refreshPolicy: refreshPolicy{Enabled: true, Interval: defaultRefreshInterval},
	}

	updated, cmd := m.Update(listLoadedMsg{
		view:      viewList,
		requestID: 2,
		data: app.IssueListData{
			Issues: []*model.Issue{testIssue(5, model.StatusTodo), testIssue(6, model.StatusReview)},
			Total:  2,
		},
	})

	next := updated.(browserModel)
	if next.selectedIssueID != 5 || next.listIndex != 0 {
		t.Fatalf("selection = %d/%d, want 5/0", next.selectedIssueID, next.listIndex)
	}
	if got := batchCommandCount(cmd); got != 2 {
		t.Fatalf("successful list load scheduled %d commands, want 2 without input flush", got)
	}
}

func TestSuccessfulDetailLoadDoesNotScheduleInputFlush(t *testing.T) {
	issue := testIssue(5, model.StatusTodo)
	m := browserModel{
		detailRequestID: 3,
		detailTargetID:  issue.ID,
		loadingDetail:   true,
	}

	updated, cmd := m.Update(detailLoadedMsg{
		requestID: 3,
		id:        issue.ID,
		data:      app.IssueDetailData{Issue: issue},
	})

	next := updated.(browserModel)
	if next.detailIssueID != issue.ID {
		t.Fatalf("detailIssueID = %d, want %d", next.detailIssueID, issue.ID)
	}
	if got := batchCommandCount(cmd); got != 0 {
		t.Fatalf("successful detail load scheduled %d commands, want 0 without input flush", got)
	}
}

func TestRapidListNavigationKeepsVisibleWindowSynchronized(t *testing.T) {
	issues := testIssues(100, 50, model.StatusTodo)
	m := browserModel{
		view:            viewList,
		focus:           focusBrowse,
		width:           100,
		height:          20,
		listData:        app.IssueListData{Issues: issues, Total: len(issues)},
		selectedIssueID: issues[0].ID,
	}

	for range 30 {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = updated.(browserModel)
	}

	if m.listIndex != 30 || m.selectedIssueID != issues[30].ID {
		t.Fatalf("selection = %d/%d, want 30/%d", m.listIndex, m.selectedIssueID, issues[30].ID)
	}
	start, end := windowBounds(m.listIndex, len(issues), 6)
	if m.listIndex < start || m.listIndex >= end {
		t.Fatalf("window = %d/%d does not include selected index %d", start, end, m.listIndex)
	}
	rendered := m.renderList(96, 6)
	if !strings.Contains(rendered, model.FormatID(issues[30].ID)) {
		t.Fatalf("rendered list does not include selected issue %s: %q", model.FormatID(issues[30].ID), rendered)
	}
	if lines := strings.Split(rendered, "\n"); len(lines) != 6 {
		t.Fatalf("rendered lines = %d, want 6", len(lines))
	}
}

func TestRapidListReverseNavigationRestoresExpectedWindow(t *testing.T) {
	issues := testIssues(200, 50, model.StatusTodo)
	m := browserModel{
		view:            viewList,
		focus:           focusBrowse,
		width:           100,
		height:          20,
		listData:        app.IssueListData{Issues: issues, Total: len(issues)},
		selectedIssueID: issues[40].ID,
		listIndex:       40,
	}

	for range 25 {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
		m = updated.(browserModel)
	}

	if m.listIndex != 15 || m.selectedIssueID != issues[15].ID {
		t.Fatalf("selection = %d/%d, want 15/%d", m.listIndex, m.selectedIssueID, issues[15].ID)
	}
	start, end := windowBounds(m.listIndex, len(issues), 6)
	if m.listIndex < start || m.listIndex >= end {
		t.Fatalf("window = %d/%d does not include selected index %d", start, end, m.listIndex)
	}
	rendered := m.renderList(96, 6)
	if !strings.Contains(rendered, model.FormatID(issues[15].ID)) {
		t.Fatalf("rendered list does not include selected issue %s: %q", model.FormatID(issues[15].ID), rendered)
	}
}

func TestBoardNavigationAcrossLargeColumnsKeepsSelectedIssueContext(t *testing.T) {
	todoIssues := testIssues(300, 24, model.StatusTodo)
	reviewIssues := testIssues(400, 24, model.StatusReview)
	m := browserModel{
		view:  viewBoard,
		focus: focusBrowse,
		boardColumns: []boardColumn{
			{Status: model.StatusTodo, Issues: todoIssues},
			{Status: model.StatusReview, Issues: reviewIssues},
		},
		selectedIssueID: todoIssues[18].ID,
		boardColumnIdx:  0,
		boardRowIdx:     18,
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	afterRight := updated.(browserModel)
	if afterRight.boardColumnIdx != 1 || afterRight.boardRowIdx != 18 {
		t.Fatalf("board position = %d/%d, want 1/18", afterRight.boardColumnIdx, afterRight.boardRowIdx)
	}
	if afterRight.selectedIssueID != reviewIssues[18].ID {
		t.Fatalf("selectedIssueID = %d, want %d", afterRight.selectedIssueID, reviewIssues[18].ID)
	}

	updated, _ = afterRight.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	afterLeft := updated.(browserModel)
	if afterLeft.boardColumnIdx != 0 || afterLeft.boardRowIdx != 18 {
		t.Fatalf("board position = %d/%d, want 0/18", afterLeft.boardColumnIdx, afterLeft.boardRowIdx)
	}
	if afterLeft.selectedIssueID != todoIssues[18].ID {
		t.Fatalf("selectedIssueID = %d, want %d", afterLeft.selectedIssueID, todoIssues[18].ID)
	}
}

func TestRefreshChurnDoesNotCauseLargeListSelectionJump(t *testing.T) {
	initial := testIssues(500, 50, model.StatusTodo)
	refreshed := append([]*model.Issue{testIssue(999, model.StatusTodo)}, initial...)
	m := browserModel{
		view:            viewList,
		focus:           focusBrowse,
		listSort:        defaultListSort,
		refreshPolicy:   refreshPolicy{Enabled: true, Interval: defaultRefreshInterval},
		refreshState:    refreshState{LoadedOnce: true},
		viewRequestID:   9,
		refreshTickID:   4,
		selectedIssueID: initial[10].ID,
		listIndex:       10,
		listData:        app.IssueListData{Issues: initial, Total: len(initial)},
	}

	updated, cmd := m.Update(refreshTickMsg{tickID: 4})
	afterTick := updated.(browserModel)
	if cmd == nil {
		t.Fatal("expected refresh tick to start a reload")
	}

	for range 7 {
		updated, _ = afterTick.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		afterTick = updated.(browserModel)
	}
	if afterTick.selectedIssueID != initial[17].ID || afterTick.listIndex != 17 {
		t.Fatalf("pre-refresh selection = %d/%d, want 17/%d", afterTick.listIndex, afterTick.selectedIssueID, initial[17].ID)
	}

	updated, _ = afterTick.Update(listLoadedMsg{
		view:      viewList,
		requestID: 10,
		data:      app.IssueListData{Issues: refreshed, Total: len(refreshed)},
	})
	afterLoad := updated.(browserModel)
	if afterLoad.selectedIssueID != initial[17].ID {
		t.Fatalf("selectedIssueID = %d, want %d", afterLoad.selectedIssueID, initial[17].ID)
	}
	if afterLoad.listIndex != 18 {
		t.Fatalf("listIndex = %d, want 18 after inserted row", afterLoad.listIndex)
	}
}

func TestLargeDatasetRemainsNavigableAcrossListAndBoardViews(t *testing.T) {
	conn := mustOpenBrowserDB(t)
	for i := 0; i < 40; i++ {
		status := model.StatusTodo
		if i%2 == 1 {
			status = model.StatusReview
		}
		createBrowserIssue(t, conn, model.Issue{
			Title:    model.FormatID(i+1) + " issue",
			Status:   status,
			Priority: model.PriorityMedium,
			Kind:     model.IssueKindTask,
		})
	}

	m := NewBrowser(conn, "/tmp/example/.docket").(browserModel)
	m.viewRequestID = 21
	updated, _ := m.Update(loadListCmd(conn, defaultListSort, 21)().(listLoadedMsg))
	afterListLoad := updated.(browserModel)
	if len(afterListLoad.listData.Issues) != 40 {
		t.Fatalf("list issues = %d, want 40", len(afterListLoad.listData.Issues))
	}

	for range 12 {
		updated, _ = afterListLoad.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		afterListLoad = updated.(browserModel)
	}
	if afterListLoad.listIndex != 12 {
		t.Fatalf("listIndex = %d, want 12", afterListLoad.listIndex)
	}

	updated, cmd := afterListLoad.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	afterBoardSwitch := updated.(browserModel)
	if afterBoardSwitch.view != viewBoard || cmd == nil {
		t.Fatalf("expected board switch with load command, view=%s cmd=%v", afterBoardSwitch.view, cmd != nil)
	}

	afterBoardSwitch.viewRequestID = 22
	updated, _ = afterBoardSwitch.Update(loadBoardCmd(conn, 22)().(boardLoadedMsg))
	afterBoardLoad := updated.(browserModel)
	if len(afterBoardLoad.boardColumns) == 0 {
		t.Fatal("expected populated board columns")
	}
	startRow := afterBoardLoad.boardRowIdx

	updated, _ = afterBoardLoad.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	afterBoardMove := updated.(browserModel)
	if afterBoardMove.boardRowIdx != startRow+1 {
		t.Fatalf("boardRowIdx = %d, want %d", afterBoardMove.boardRowIdx, startRow+1)
	}
	if afterBoardMove.selectedIssue() == nil {
		t.Fatal("expected board selection to remain valid")
	}
}

func TestRefreshErrorStaysNonFatalAfterSuccessfulLoad(t *testing.T) {
	m := browserModel{
		view:          viewList,
		viewRequestID: 3,
		loading:       true,
		listData: app.IssueListData{Issues: []*model.Issue{
			testIssue(5, model.StatusTodo),
		}},
		selectedIssueID: 5,
		refreshPolicy:   refreshPolicy{Enabled: true, Interval: defaultRefreshInterval},
		refreshState: refreshState{
			LoadedOnce:  true,
			LastSuccess: time.Date(2026, 3, 28, 12, 34, 56, 0, time.UTC),
		},
	}

	updated, cmd := m.Update(listLoadedMsg{view: viewList, requestID: 3, err: os.ErrPermission})
	next := updated.(browserModel)
	if next.errMsg != "" {
		t.Fatalf("errMsg = %q, want empty for non-fatal refresh failure", next.errMsg)
	}
	if next.refreshState.LastError == "" {
		t.Fatal("expected refresh error to be surfaced")
	}
	if len(next.listData.Issues) != 1 || next.listData.Issues[0].ID != 5 {
		t.Fatalf("expected prior list data to be preserved, got %#v", next.listData.Issues)
	}
	if cmd == nil {
		t.Fatal("expected refresh loop to continue after non-fatal failure")
	}
}

func TestAutoRefreshTogglePausesAndResumesPolling(t *testing.T) {
	m := browserModel{
		refreshPolicy: refreshPolicy{Enabled: true, Interval: defaultRefreshInterval},
		refreshTickID: 2,
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	paused := updated.(browserModel)
	if paused.refreshPolicy.Enabled {
		t.Fatal("expected auto-refresh to pause")
	}
	if paused.refreshTickID != 3 {
		t.Fatalf("refreshTickID = %d, want 3", paused.refreshTickID)
	}
	if cmd != nil {
		t.Fatal("expected pause to stop scheduling ticks")
	}

	updated, cmd = paused.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	resumed := updated.(browserModel)
	if !resumed.refreshPolicy.Enabled {
		t.Fatal("expected auto-refresh to resume")
	}
	if resumed.refreshTickID != 4 {
		t.Fatalf("refreshTickID = %d, want 4", resumed.refreshTickID)
	}
	if cmd == nil {
		t.Fatal("expected resume to schedule the next tick")
	}
}

func TestListLoadErrorClearsListData(t *testing.T) {
	m := browserModel{
		view:          viewList,
		viewRequestID: 2,
		listData:      app.IssueListData{Issues: []*model.Issue{testIssue(1, model.StatusTodo)}},
	}

	updated, _ := m.Update(listLoadedMsg{view: viewList, requestID: 2, err: os.ErrNotExist})
	next := updated.(browserModel)
	if next.errMsg == "" {
		t.Fatal("expected error message")
	}
	if len(next.listData.Issues) != 0 {
		t.Fatalf("listData = %#v, want empty", next.listData.Issues)
	}
}

func TestBoardProbeKeysDoNotBlockListSwitch(t *testing.T) {
	m := browserModel{
		view:            viewBoard,
		focus:           focusBrowse,
		viewRequestID:   2,
		selectedIssueID: 1,
		boardColumns: []boardColumn{
			{Status: model.StatusTodo, Issues: []*model.Issue{testIssue(1, model.StatusTodo)}},
		},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Alt: true, Runes: []rune{']'}})
	afterProbe := updated.(browserModel)
	if afterProbe.view != viewBoard || afterProbe.viewRequestID != 2 {
		t.Fatalf("probe key changed state: view=%s request=%d", afterProbe.view, afterProbe.viewRequestID)
	}

	updated, _ = afterProbe.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	afterListKey := updated.(browserModel)
	if afterListKey.view != viewList {
		t.Fatalf("view = %s, want %s", afterListKey.view, viewList)
	}
	if !afterListKey.loading {
		t.Fatalf("expected list reload to start")
	}
	if afterListKey.viewRequestID != 3 {
		t.Fatalf("viewRequestID = %d, want 3", afterListKey.viewRequestID)
	}
}

func TestViewSwitchClearsExpandedDetailState(t *testing.T) {
	tests := []struct {
		name      string
		startView viewMode
		key       rune
		wantView  viewMode
	}{
		{name: "list hotkey", startView: viewBoard, key: '1', wantView: viewList},
		{name: "board hotkey", startView: viewList, key: '2', wantView: viewBoard},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := browserModel{
				view:            tt.startView,
				focus:           focusDetail,
				detailExpanded:  true,
				selectedIssueID: 7,
				detailTargetID:  42,
				detailFocus:     detailFocusSubIssues,
				detailSubIndex:  2,
				detailHistory: []detailNavState{{
					issueID:       99,
					scroll:        4,
					subIssueIndex: 1,
					focusRegion:   detailFocusSubIssues,
				}},
				viewRequestID: 5,
			}

			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{tt.key}})
			afterSwitch := updated.(browserModel)

			if afterSwitch.view != tt.wantView {
				t.Fatalf("view = %s, want %s", afterSwitch.view, tt.wantView)
			}
			if afterSwitch.detailExpanded {
				t.Fatal("expected expanded detail to close on view switch")
			}
			if afterSwitch.focus != focusBrowse {
				t.Fatalf("focus = %s, want %s", afterSwitch.focus, focusBrowse)
			}
			if afterSwitch.detailTargetID != afterSwitch.selectedIssueID {
				t.Fatalf("detailTargetID = %d, want selected issue %d", afterSwitch.detailTargetID, afterSwitch.selectedIssueID)
			}
			if afterSwitch.detailFocus != detailFocusBody {
				t.Fatalf("detailFocus = %s, want %s", afterSwitch.detailFocus, detailFocusBody)
			}
			if afterSwitch.detailSubIndex != 0 {
				t.Fatalf("detailSubIndex = %d, want 0", afterSwitch.detailSubIndex)
			}
			if len(afterSwitch.detailHistory) != 0 {
				t.Fatalf("detailHistory = %#v, want empty", afterSwitch.detailHistory)
			}
			if !afterSwitch.loading {
				t.Fatal("expected view reload to start")
			}
			if afterSwitch.viewRequestID != 6 {
				t.Fatalf("viewRequestID = %d, want 6", afterSwitch.viewRequestID)
			}
			if cmd == nil {
				t.Fatal("expected load command")
			}
		})
	}
}

func TestQuestionMarkStillOpensHelpAfterBoardLoad(t *testing.T) {
	m := browserModel{
		view:          viewBoard,
		focus:         focusBrowse,
		viewRequestID: 2,
	}

	updated, _ := m.Update(boardLoadedMsg{
		view:      viewBoard,
		requestID: 2,
		data: app.BoardData{Issues: []*model.Issue{
			testIssue(1, model.StatusTodo),
		}},
	})
	afterBoard := updated.(browserModel)
	if afterBoard.selectedIssueID != 1 {
		t.Fatalf("selectedIssueID = %d, want 1", afterBoard.selectedIssueID)
	}

	updated, _ = afterBoard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	afterHelp := updated.(browserModel)
	if !afterHelp.showHelp {
		t.Fatalf("expected help overlay to open")
	}

	updated, _ = afterHelp.Update(tea.KeyMsg{Type: tea.KeyEsc})
	afterEsc := updated.(browserModel)
	if afterEsc.showHelp {
		t.Fatalf("expected help overlay to close")
	}
}

func TestLoaderCommandsReturnExpectedMessages(t *testing.T) {
	conn := mustOpenBrowserDB(t)
	parentID := createBrowserIssue(t, conn, model.Issue{
		Title:    "Priority-first issue",
		Status:   model.StatusInProgress,
		Priority: model.PriorityCritical,
		Kind:     model.IssueKindEpic,
	})
	createBrowserIssue(t, conn, model.Issue{
		ParentID: &parentID,
		Title:    "Child",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	})
	newestID := createBrowserIssue(t, conn, model.Issue{
		Title:    "Newest by id",
		Status:   model.StatusBacklog,
		Priority: model.PriorityLow,
		Kind:     model.IssueKindTask,
	})

	listMsg, ok := loadListCmd(conn, defaultListSort, 11)().(listLoadedMsg)
	if !ok || listMsg.requestID != 11 || listMsg.err != nil || len(listMsg.data.Issues) == 0 {
		t.Fatalf("loadListCmd() = %#v", listMsg)
	}
	if listMsg.data.Issues[0].ID != newestID {
		t.Fatalf("loadListCmd() first issue = %d, want newest id %d", listMsg.data.Issues[0].ID, newestID)
	}

	boardMsg, ok := loadBoardCmd(conn, 12)().(boardLoadedMsg)
	if !ok || boardMsg.requestID != 12 || boardMsg.err != nil || len(boardMsg.data.Issues) == 0 {
		t.Fatalf("loadBoardCmd() = %#v", boardMsg)
	}

	detailMsg, ok := loadDetailCmd(conn, parentID, 13)().(detailLoadedMsg)
	if !ok || detailMsg.requestID != 13 || detailMsg.id != parentID || detailMsg.err != nil || detailMsg.data.Issue == nil {
		t.Fatalf("loadDetailCmd() = %#v", detailMsg)
	}

	if _, ok := loadViewCmd(conn, viewList, defaultListSort, 14)().(listLoadedMsg); !ok {
		t.Fatal("expected list view loader message")
	}
	if _, ok := loadViewCmd(conn, viewBoard, defaultListSort, 15)().(boardLoadedMsg); !ok {
		t.Fatal("expected board view loader message")
	}
}

func TestLoadListCmdPassesExplicitSortState(t *testing.T) {
	originalListIssues := listIssues
	t.Cleanup(func() {
		listIssues = originalListIssues
	})

	var got app.ListIssuesParams
	listIssues = func(conn *sql.DB, params app.ListIssuesParams) (app.IssueListData, error) {
		got = params
		return app.IssueListData{}, nil
	}

	msg, ok := loadListCmd(nil, defaultListSort, 23)().(listLoadedMsg)
	if !ok {
		t.Fatalf("loadListCmd() returned %T, want listLoadedMsg", msg)
	}
	if msg.requestID != 23 {
		t.Fatalf("requestID = %d, want 23", msg.requestID)
	}
	if got.Sort != defaultListSort.Field || got.SortDir != defaultListSort.Dir {
		t.Fatalf("sort params = %q/%q, want %q/%q", got.Sort, got.SortDir, defaultListSort.Field, defaultListSort.Dir)
	}
	if got.Limit != defaultListLimit {
		t.Fatalf("limit = %d, want %d", got.Limit, defaultListLimit)
	}
	if msg.err != nil {
		t.Fatalf("loadListCmd() error = %v", msg.err)
	}
	if msg.view != viewList {
		t.Fatalf("view = %s, want %s", msg.view, viewList)
	}
}

func TestListLoadMessageKeepsSelectionAcrossExplicitOrderChange(t *testing.T) {
	selected := testIssue(4, model.StatusTodo)
	m := browserModel{
		view:            viewList,
		listSort:        defaultListSort,
		viewRequestID:   5,
		selectedIssueID: selected.ID,
		listIndex:       2,
		listData: app.IssueListData{Issues: []*model.Issue{
			testIssue(2, model.StatusTodo),
			testIssue(3, model.StatusTodo),
			selected,
		}},
	}

	updated, _ := m.Update(listLoadedMsg{
		view:      viewList,
		requestID: 5,
		data: app.IssueListData{Issues: []*model.Issue{
			selected,
			testIssue(3, model.StatusTodo),
			testIssue(2, model.StatusTodo),
		}},
	})

	next := updated.(browserModel)
	if next.selectedIssueID != selected.ID {
		t.Fatalf("selectedIssueID = %d, want %d", next.selectedIssueID, selected.ID)
	}
	if next.listIndex != 0 {
		t.Fatalf("listIndex = %d, want 0", next.listIndex)
	}
	if next.detailTargetID != selected.ID {
		t.Fatalf("detailTargetID = %d, want %d", next.detailTargetID, selected.ID)
	}
}

func TestListBrowseKeyCyclesSortFieldAndReloads(t *testing.T) {
	m := browserModel{
		view:          viewList,
		focus:         focusBrowse,
		listSort:      defaultListSort,
		viewRequestID: 2,
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	next := updated.(browserModel)
	if next.listSort.Field != "title" {
		t.Fatalf("listSort.Field = %q, want title", next.listSort.Field)
	}
	if next.listSort.Dir != defaultListSort.Dir {
		t.Fatalf("listSort.Dir = %q, want %q", next.listSort.Dir, defaultListSort.Dir)
	}
	if !next.loading {
		t.Fatal("expected list sort change to start a reload")
	}
	if next.viewRequestID != 3 {
		t.Fatalf("viewRequestID = %d, want 3", next.viewRequestID)
	}
	if cmd == nil {
		t.Fatal("expected sort field change to return a load command")
	}
}

func TestListBrowseKeyTogglesSortDirectionAndReloads(t *testing.T) {
	m := browserModel{
		view:          viewList,
		focus:         focusBrowse,
		listSort:      defaultListSort,
		viewRequestID: 4,
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	next := updated.(browserModel)
	if next.listSort.Field != defaultListSort.Field {
		t.Fatalf("listSort.Field = %q, want %q", next.listSort.Field, defaultListSort.Field)
	}
	if next.listSort.Dir != "asc" {
		t.Fatalf("listSort.Dir = %q, want asc", next.listSort.Dir)
	}
	if !next.loading {
		t.Fatal("expected sort direction change to start a reload")
	}
	if next.viewRequestID != 5 {
		t.Fatalf("viewRequestID = %d, want 5", next.viewRequestID)
	}
	if cmd == nil {
		t.Fatal("expected sort direction change to return a load command")
	}
}

func TestHelpOverlayViewRendersKeyboardReference(t *testing.T) {
	m := browserModel{width: 100, height: 20, showHelp: true}
	rendered := m.View()
	for _, fragment := range []string{"docket tui", "s        cycle list sort field", "S        toggle list sort direction", "ctrl+u/d half-page", "This preview is read-only."} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("rendered help missing %q: %q", fragment, rendered)
		}
	}
}

func TestBoardViewNarrowLayoutUsesSingleColumnHeader(t *testing.T) {
	m := browserModel{
		width:         90,
		height:        18,
		view:          viewBoard,
		focus:         focusBrowse,
		refreshPolicy: refreshPolicy{Enabled: true, Interval: defaultRefreshInterval},
		boardColumns: []boardColumn{{
			Status: model.StatusTodo,
			Issues: []*model.Issue{testIssue(1, model.StatusTodo)},
		}},
		selectedIssueID: 1,
	}

	rendered := m.View()
	if !strings.Contains(rendered, "refresh auto 5s not loaded") {
		t.Fatalf("expected refresh context in board view, got %q", rendered)
	}
	if strings.Count(rendered, "TODO (1)") != 1 {
		t.Fatalf("expected a single TODO board column title in narrow board view, got %q", rendered)
	}
	if !strings.Contains(rendered, "TODO (1)") {
		t.Fatalf("expected board column title with count, got %q", rendered)
	}
	if !strings.Contains(rendered, "DKT-1") {
		t.Fatalf("expected selected issue to remain visible, got %q", rendered)
	}

	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		if strings.Contains(line, "Issues · BOARD") {
			if i+1 >= len(lines) {
				t.Fatalf("expected columns after board title, got %q", rendered)
			}
			if !strings.Contains(lines[i+1], "╭") {
				t.Fatalf("expected board columns immediately after board title, got %q", rendered)
			}
			break
		}
	}
}

func TestStackedLayoutStaysWithinShortTerminalHeight(t *testing.T) {
	m := browserModel{
		width:           90,
		height:          10,
		view:            viewList,
		focus:           focusBrowse,
		selectedIssueID: 1,
		listData:        app.IssueListData{Issues: []*model.Issue{testIssue(1, model.StatusTodo)}, Total: 1},
		detailTargetID:  1,
		detailIssueID:   1,
		detailData:      app.IssueDetailData{Issue: testIssue(1, model.StatusTodo)},
	}

	rendered := m.View()
	if got := len(strings.Split(rendered, "\n")); got > m.height {
		t.Fatalf("rendered %d lines for %d-row terminal: %q", got, m.height, rendered)
	}

	_, detailHeight := m.detailPaneDimensions()
	if detailHeight != 0 {
		t.Fatalf("detailPaneDimensions() height = %d, want 0", detailHeight)
	}
	if strings.Count(rendered, "Issues · LIST") != 1 {
		t.Fatalf("expected stacked browse pane title once, got %q", rendered)
	}
	if strings.Contains(rendered, "Detail · DKT-1") {
		t.Fatalf("expected short stacked layout to collapse to the focused pane, got %q", rendered)
	}
}

func TestListViewShowsActiveSortAndRefreshCue(t *testing.T) {
	m := browserModel{
		width:         100,
		height:        24,
		view:          viewList,
		focus:         focusBrowse,
		listSort:      defaultListSort,
		refreshPolicy: refreshPolicy{Enabled: true, Interval: defaultRefreshInterval},
		refreshState:  refreshState{LastSuccess: time.Date(2026, 3, 28, 12, 34, 56, 0, time.UTC)},
		listData: app.IssueListData{Issues: []*model.Issue{{
			ID:       14,
			Title:    "Render richer list metadata without wrapping rows",
			Status:   model.StatusInProgress,
			Priority: model.PriorityHigh,
			Kind:     model.IssueKindFeature,
		}}, Total: 1},
	}

	rendered := m.View()
	for _, fragment := range []string{"SORT ID DESC", "refreshed 12:34:56", "s sort-field", "S sort-dir", "INPROG", "FEATURE"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected list view to include %q: %q", fragment, rendered)
		}
	}
}

func TestListViewWrapsFooterHintsAtNarrowWidths(t *testing.T) {
	m := browserModel{
		width:         78,
		height:        16,
		view:          viewList,
		focus:         focusBrowse,
		listSort:      defaultListSort,
		refreshPolicy: refreshPolicy{Enabled: true, Interval: defaultRefreshInterval},
		listData: app.IssueListData{Issues: []*model.Issue{{
			ID:       14,
			Title:    "Render richer list metadata without wrapping rows",
			Status:   model.StatusInProgress,
			Priority: model.PriorityHigh,
			Kind:     model.IssueKindFeature,
		}}, Total: 1},
	}

	rendered := m.View()
	if got := len(strings.Split(rendered, "\n")); got != m.height {
		t.Fatalf("view lines = %d, want %d", got, m.height)
	}
	for _, fragment := range []string{"s sort-field", "S sort-dir", "J/K detail", "o drill-down", "q quit"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected narrow list view to include %q: %q", fragment, rendered)
		}
	}
	if !strings.Contains(rendered, "\n J/K detail") {
		t.Fatalf("expected footer hints to wrap onto a dedicated continuation line: %q", rendered)
	}
}

func TestBrowseDrillDownFocusesEpicSubIssuesAndKeepsSelection(t *testing.T) {
	epic := testIssue(7, model.StatusTodo)
	epic.Kind = model.IssueKindEpic
	childA := testIssue(8, model.StatusTodo)
	childB := testIssue(9, model.StatusDone)
	m := browserModel{
		view:            viewList,
		focus:           focusBrowse,
		selectedIssueID: epic.ID,
		listIndex:       0,
		listData: app.IssueListData{
			Issues:   []*model.Issue{epic},
			Total:    1,
			Progress: map[int]render.SubIssueProgress{epic.ID: {Done: 1, Total: 2}},
		},
		detailIssueID:  epic.ID,
		detailTargetID: epic.ID,
		detailData:     app.IssueDetailData{Issue: epic, SubIssues: []*model.Issue{childA, childB}},
	}

	updated, _ := m.handleBrowseKey("o")
	next := updated.(browserModel)
	if next.focus != focusDetail {
		t.Fatalf("focus = %s, want %s", next.focus, focusDetail)
	}
	if next.detailFocus != detailFocusSubIssues {
		t.Fatalf("detailFocus = %s, want %s", next.detailFocus, detailFocusSubIssues)
	}
	if next.selectedIssueID != epic.ID {
		t.Fatalf("selectedIssueID = %d, want %d", next.selectedIssueID, epic.ID)
	}
}

func TestBrowseDrillDownPendingFocusAppliesAfterDetailLoad(t *testing.T) {
	epic := testIssue(11, model.StatusTodo)
	epic.Kind = model.IssueKindEpic
	child := testIssue(12, model.StatusTodo)
	m := browserModel{
		view:            viewList,
		focus:           focusBrowse,
		selectedIssueID: epic.ID,
		listData: app.IssueListData{
			Issues:   []*model.Issue{epic},
			Progress: map[int]render.SubIssueProgress{epic.ID: {Done: 0, Total: 1}},
		},
		detailTargetID: epic.ID,
		loadingDetail:  true,
	}

	updated, _ := m.handleBrowseKey("o")
	queued := updated.(browserModel)
	if queued.pendingDetailFocus != detailFocusSubIssues {
		t.Fatalf("pendingDetailFocus = %s, want %s", queued.pendingDetailFocus, detailFocusSubIssues)
	}

	updated, _ = queued.Update(detailLoadedMsg{
		requestID: queued.detailRequestID,
		id:        epic.ID,
		data:      app.IssueDetailData{Issue: epic, SubIssues: []*model.Issue{child}},
	})
	afterLoad := updated.(browserModel)
	if afterLoad.detailFocus != detailFocusSubIssues {
		t.Fatalf("detailFocus = %s, want %s", afterLoad.detailFocus, detailFocusSubIssues)
	}
	if afterLoad.pendingDetailFocus != "" {
		t.Fatalf("pendingDetailFocus = %q, want empty", afterLoad.pendingDetailFocus)
	}
}

func TestBrowseDrillDownFocusesNonEpicParentSubIssues(t *testing.T) {
	parent := testIssue(13, model.StatusTodo)
	parent.Kind = model.IssueKindFeature
	child := testIssue(14, model.StatusTodo)
	m := browserModel{
		view:            viewList,
		focus:           focusBrowse,
		selectedIssueID: parent.ID,
		listIndex:       0,
		listData: app.IssueListData{
			Issues:   []*model.Issue{parent},
			Total:    1,
			Progress: map[int]render.SubIssueProgress{parent.ID: {Done: 0, Total: 1}},
		},
		detailIssueID:  parent.ID,
		detailTargetID: parent.ID,
		detailData:     app.IssueDetailData{Issue: parent, SubIssues: []*model.Issue{child}},
	}

	updated, _ := m.handleBrowseKey("o")
	next := updated.(browserModel)
	if next.focus != focusDetail {
		t.Fatalf("focus = %s, want %s", next.focus, focusDetail)
	}
	if next.detailFocus != detailFocusSubIssues {
		t.Fatalf("detailFocus = %s, want %s", next.detailFocus, detailFocusSubIssues)
	}
	if next.selectedIssueID != parent.ID {
		t.Fatalf("selectedIssueID = %d, want %d", next.selectedIssueID, parent.ID)
	}
}

func TestBrowseDrillDownPendingFocusAppliesForNonEpicParent(t *testing.T) {
	parent := testIssue(15, model.StatusTodo)
	parent.Kind = model.IssueKindTask
	child := testIssue(16, model.StatusTodo)
	m := browserModel{
		view:            viewList,
		focus:           focusBrowse,
		selectedIssueID: parent.ID,
		listData: app.IssueListData{
			Issues:   []*model.Issue{parent},
			Progress: map[int]render.SubIssueProgress{parent.ID: {Done: 0, Total: 1}},
		},
		detailTargetID: parent.ID,
		loadingDetail:  true,
	}

	updated, _ := m.handleBrowseKey("o")
	queued := updated.(browserModel)
	if queued.pendingDetailFocus != detailFocusSubIssues {
		t.Fatalf("pendingDetailFocus = %s, want %s", queued.pendingDetailFocus, detailFocusSubIssues)
	}

	updated, _ = queued.Update(detailLoadedMsg{
		requestID: queued.detailRequestID,
		id:        parent.ID,
		data:      app.IssueDetailData{Issue: parent, SubIssues: []*model.Issue{child}},
	})
	afterLoad := updated.(browserModel)
	if afterLoad.detailFocus != detailFocusSubIssues {
		t.Fatalf("detailFocus = %s, want %s", afterLoad.detailFocus, detailFocusSubIssues)
	}
	if afterLoad.pendingDetailFocus != "" {
		t.Fatalf("pendingDetailFocus = %q, want empty", afterLoad.pendingDetailFocus)
	}
}

func TestBrowseToEpicToSubIssueFlowWithSQLite(t *testing.T) {
	conn := mustOpenBrowserDB(t)
	epicID := createBrowserIssue(t, conn, model.Issue{
		Title:    "Epic root",
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindEpic,
	})
	childEpicID := createBrowserIssue(t, conn, model.Issue{
		ParentID: &epicID,
		Title:    "Child epic",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindEpic,
	})
	createBrowserIssue(t, conn, model.Issue{
		ParentID: &childEpicID,
		Title:    "Leaf task",
		Status:   model.StatusDone,
		Priority: model.PriorityLow,
		Kind:     model.IssueKindTask,
	})

	listMsg := loadListCmd(conn, defaultListSort, 41)().(listLoadedMsg)
	m := NewBrowser(conn, "/tmp/example/.docket").(browserModel)
	m.viewRequestID = 41
	updated, cmd := m.Update(listMsg)
	loaded := updated.(browserModel)
	if cmd == nil {
		t.Fatal("expected follow-up detail load after list load")
	}
	loaded.selectedIssueID = epicID
	loaded.listIndex = findIssueIndex(loaded.listData.Issues, epicID)
	loaded.resetDetailNavigationToBrowseSelection()

	detailEpic := loadDetailCmd(conn, epicID, 1)().(detailLoadedMsg)
	loaded.detailRequestID = 1
	updated, _ = loaded.Update(detailEpic)
	afterEpicLoad := updated.(browserModel)

	updated, _ = afterEpicLoad.handleBrowseKey("o")
	drilled := updated.(browserModel)
	if drilled.focus != focusDetail || drilled.detailFocus != detailFocusSubIssues {
		t.Fatalf("drill-down focus = %s/%s, want %s/%s", drilled.focus, drilled.detailFocus, focusDetail, detailFocusSubIssues)
	}

	updated, cmd = drilled.handleDetailKey("enter")
	afterChildOpen := updated.(browserModel)
	if cmd == nil {
		t.Fatal("expected child detail load command")
	}
	if afterChildOpen.detailTargetID != childEpicID {
		t.Fatalf("detailTargetID = %d, want %d", afterChildOpen.detailTargetID, childEpicID)
	}

	detailChild := loadDetailCmd(conn, childEpicID, afterChildOpen.detailRequestID)().(detailLoadedMsg)
	updated, _ = afterChildOpen.Update(detailChild)
	afterChildLoad := updated.(browserModel)

	updated, _ = afterChildLoad.handleDetailKey("l")
	afterChildFocus := updated.(browserModel)
	if afterChildFocus.detailFocus != detailFocusSubIssues {
		t.Fatalf("detailFocus = %s, want %s", afterChildFocus.detailFocus, detailFocusSubIssues)
	}

	updated, cmd = afterChildFocus.handleDetailKey("enter")
	afterLeafOpen := updated.(browserModel)
	if cmd == nil {
		t.Fatal("expected leaf detail load command")
	}
	if afterLeafOpen.detailTargetID == childEpicID {
		t.Fatalf("expected navigation to leaf issue, still at %d", afterLeafOpen.detailTargetID)
	}
	if len(afterLeafOpen.detailHistory) < 2 {
		t.Fatalf("expected stacked detail history, got %#v", afterLeafOpen.detailHistory)
	}
}

func TestHierarchyDecorationsStayConsistentBetweenListAndBoard(t *testing.T) {
	conn := mustOpenBrowserDB(t)
	epicID := createBrowserIssue(t, conn, model.Issue{
		Title:    "Epic row",
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindEpic,
	})
	createBrowserIssue(t, conn, model.Issue{ParentID: &epicID, Title: "Child A", Status: model.StatusDone, Priority: model.PriorityMedium, Kind: model.IssueKindTask})
	createBrowserIssue(t, conn, model.Issue{ParentID: &epicID, Title: "Child B", Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask})

	listMsg := loadListCmd(conn, defaultListSort, 51)().(listLoadedMsg)
	boardMsg := loadBoardCmd(conn, 52)().(boardLoadedMsg)
	epic := listMsg.data.ParentMap[epicID]
	if epic == nil {
		for _, issue := range listMsg.data.Issues {
			if issue.ID == epicID {
				epic = issue
				break
			}
		}
	}
	if epic == nil {
		t.Fatalf("expected epic %d in list data", epicID)
	}

	listRendered := render.RenderUIListRow(epic, render.HierarchyDecoration{IsEpic: true, ChildCount: listMsg.data.Progress[epicID].Total, Done: listMsg.data.Progress[epicID].Done, Total: listMsg.data.Progress[epicID].Total}, 64, false)
	boardRendered := render.RenderUIBoardColumn(model.StatusTodo, []*model.Issue{epic}, map[int]render.HierarchyDecoration{epicID: {IsEpic: true, ChildCount: boardMsg.data.Progress[epicID].Total, Done: boardMsg.data.Progress[epicID].Done, Total: boardMsg.data.Progress[epicID].Total}}, 64, 8, true, true, 0)

	for _, fragment := range []string{"[2 sub 1/2]"} {
		if !strings.Contains(listRendered, fragment) {
			t.Fatalf("expected list row to include %q: %q", fragment, listRendered)
		}
		if !strings.Contains(boardRendered, fragment) {
			t.Fatalf("expected board row to include %q: %q", fragment, boardRendered)
		}
	}
}

func TestDetailFocusDoesNotIncreaseViewHeightAtNarrowWidths(t *testing.T) {
	issueA := testIssue(7, model.StatusTodo)
	issueA.Title = "Epic: full read-only docket ui roadmap"
	issueB := testIssue(9, model.StatusTodo)
	issueB.Title = "Implement: refactor docket ui into a routeable multi-surface read browser"

	m := browserModel{
		width:           90,
		height:          28,
		view:            viewList,
		focus:           focusDetail,
		selectedIssueID: issueA.ID,
		listData:        app.IssueListData{Issues: []*model.Issue{issueB, issueA}, Total: 15},
		listIndex:       1,
		detailIssueID:   issueA.ID,
		detailTargetID:  issueA.ID,
		detailData:      app.IssueDetailData{Issue: issueA},
		detailFocus:     detailFocusBody,
	}

	rendered := m.View()
	if got := len(strings.Split(rendered, "\n")); got != m.height {
		t.Fatalf("view lines = %d, want %d", got, m.height)
	}
}

func TestSubIssueFocusDoesNotIncreaseViewHeight(t *testing.T) {
	parent := testIssue(7, model.StatusTodo)
	parent.Title = "Epic: full read-only docket ui roadmap"
	childA := testIssue(8, model.StatusTodo)
	childA.Title = "Phase 1: full-read tui foundation and shared navigation"
	childB := testIssue(12, model.StatusTodo)
	childB.Title = "Phase 2: issue-centric read parity inside docket ui"
	childB.Kind = model.IssueKindEpic

	m := browserModel{
		width:           120,
		height:          28,
		view:            viewList,
		focus:           focusDetail,
		selectedIssueID: parent.ID,
		listData:        app.IssueListData{Issues: []*model.Issue{parent}, Total: 15},
		detailIssueID:   parent.ID,
		detailTargetID:  parent.ID,
		detailData:      app.IssueDetailData{Issue: parent, SubIssues: []*model.Issue{childA, childB}},
		detailFocus:     detailFocusSubIssues,
		detailSubIndex:  1,
	}

	rendered := m.View()
	if got := len(strings.Split(rendered, "\n")); got != m.height {
		t.Fatalf("view lines = %d, want %d", got, m.height)
	}
}

func TestBoardSubIssueFocusDoesNotIncreaseViewHeight(t *testing.T) {
	parent := testIssue(7, model.StatusTodo)
	parent.Title = "Epic: full read-only docket ui roadmap"
	childA := testIssue(8, model.StatusTodo)
	childA.Title = "Phase 1: full-read tui foundation and shared navigation"
	childA.Kind = model.IssueKindEpic
	done := testIssue(1, model.StatusDone)
	done.Title = "Feature: read-only interaction"

	m := browserModel{
		width:           120,
		height:          28,
		view:            viewBoard,
		focus:           focusDetail,
		selectedIssueID: parent.ID,
		boardColumns: []boardColumn{
			{Status: model.StatusTodo, Issues: []*model.Issue{parent}},
			{Status: model.StatusDone, Issues: []*model.Issue{done}},
		},
		detailIssueID:  parent.ID,
		detailTargetID: parent.ID,
		detailData:     app.IssueDetailData{Issue: parent, SubIssues: []*model.Issue{childA}},
		detailFocus:    detailFocusSubIssues,
		detailSubIndex: 0,
	}

	rendered := m.View()
	if got := len(strings.Split(rendered, "\n")); got != m.height {
		t.Fatalf("view lines = %d, want %d", got, m.height)
	}
}

func TestBoardDetailFocusDoesNotIncreaseViewHeight(t *testing.T) {
	parent := testIssue(7, model.StatusTodo)
	parent.Title = "Epic: full read-only docket ui roadmap"
	done := testIssue(1, model.StatusDone)
	done.Title = "Feature: read-only interaction"

	m := browserModel{
		width:           120,
		height:          28,
		view:            viewBoard,
		focus:           focusDetail,
		selectedIssueID: parent.ID,
		boardColumns: []boardColumn{
			{Status: model.StatusTodo, Issues: []*model.Issue{parent}},
			{Status: model.StatusDone, Issues: []*model.Issue{done}},
		},
		detailIssueID:  parent.ID,
		detailTargetID: parent.ID,
		detailData:     app.IssueDetailData{Issue: parent},
		detailFocus:    detailFocusBody,
	}

	rendered := m.View()
	if got := len(strings.Split(rendered, "\n")); got != m.height {
		t.Fatalf("view lines = %d, want %d", got, m.height)
	}
}

func TestTerminalProbeDetection(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{name: "osc response with prefix", key: "]11;rgb:1616/1818/1a1a", want: true},
		{name: "osc response body", key: "11;rgb:1616/1818/1a1a", want: true},
		{name: "alt close bracket", key: "alt+]", want: true},
		{name: "alt backslash", key: "alt+\\", want: true},
		{name: "normal view hotkey", key: "1", want: false},
		{name: "help hotkey", key: "?", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTerminalProbe(tt.key); got != tt.want {
				t.Fatalf("isTerminalProbe(%q) = %t, want %t", tt.key, got, tt.want)
			}
		})
	}
}

func TestStaleDetailMessagesAreIgnored(t *testing.T) {
	m := browserModel{
		view:            viewBoard,
		selectedIssueID: 2,
		detailTargetID:  2,
		detailRequestID: 4,
		detailIssueID:   2,
		detailData: app.IssueDetailData{
			Issue: testIssue(2, model.StatusReview),
		},
	}

	updated, _ := m.Update(detailLoadedMsg{
		requestID: 3,
		id:        1,
		data: app.IssueDetailData{
			Issue: testIssue(1, model.StatusTodo),
		},
	})

	next := updated.(browserModel)
	if next.detailIssueID != 2 {
		t.Fatalf("detailIssueID = %d, want 2", next.detailIssueID)
	}
	if next.detailData.Issue == nil || next.detailData.Issue.ID != 2 {
		t.Fatalf("detail issue changed unexpectedly: %#v", next.detailData.Issue)
	}
}

func TestDetailLoadMessageUpdatesDataAndNormalizesSubIssueState(t *testing.T) {
	parent := testIssue(10, model.StatusTodo)
	child := testIssue(11, model.StatusDone)
	m := browserModel{
		selectedIssueID: 10,
		detailTargetID:  10,
		detailRequestID: 7,
		loadingDetail:   true,
		detailFocus:     detailFocusSubIssues,
		detailSubIndex:  9,
	}

	updated, cmd := m.Update(detailLoadedMsg{
		requestID: 7,
		id:        10,
		data: app.IssueDetailData{
			Issue:     parent,
			SubIssues: []*model.Issue{child},
		},
	})

	next := updated.(browserModel)
	if next.loadingDetail {
		t.Fatal("expected detail loading to stop")
	}
	if next.detailIssueID != 10 {
		t.Fatalf("detailIssueID = %d, want 10", next.detailIssueID)
	}
	if next.detailSubIndex != 0 {
		t.Fatalf("detailSubIndex = %d, want 0", next.detailSubIndex)
	}
	if next.detailFocus != detailFocusSubIssues {
		t.Fatalf("detailFocus = %s, want %s", next.detailFocus, detailFocusSubIssues)
	}
	if cmd != nil {
		t.Fatal("expected no follow-up command after successful detail load")
	}
}

func TestDetailLoadErrorClearsDetailIssueID(t *testing.T) {
	m := browserModel{
		selectedIssueID: 2,
		detailTargetID:  2,
		detailRequestID: 3,
		loadingDetail:   true,
		detailIssueID:   2,
	}

	updated, _ := m.Update(detailLoadedMsg{requestID: 3, id: 2, err: os.ErrPermission})
	next := updated.(browserModel)
	if next.detailErr == "" {
		t.Fatal("expected detail error")
	}
	if next.detailIssueID != 0 {
		t.Fatalf("detailIssueID = %d, want 0", next.detailIssueID)
	}
}

func TestBeginDetailLoadWithoutTargetClearsDetailState(t *testing.T) {
	m := browserModel{
		detailIssueID:   5,
		loadingDetail:   true,
		detailData:      app.IssueDetailData{Issue: testIssue(5, model.StatusTodo)},
		selectedIssueID: 0,
	}
	if cmd := m.beginDetailLoad(); cmd != nil {
		t.Fatal("expected nil detail load command")
	}
	if m.loadingDetail || m.detailIssueID != 0 || m.detailData.Issue != nil {
		t.Fatalf("detail state = %#v", m)
	}
}

func TestNormalizeDetailStateResetsBodyWhenNoSubIssues(t *testing.T) {
	m := browserModel{
		detailFocus:    detailFocusSubIssues,
		detailSubIndex: 3,
	}
	m.normalizeDetailState()
	if m.detailFocus != detailFocusBody || m.detailSubIndex != 0 {
		t.Fatalf("normalizeDetailState() = focus:%s index:%d", m.detailFocus, m.detailSubIndex)
	}
}

func TestDetailSubIssueNavigationOpensChild(t *testing.T) {
	parent := testIssue(1, model.StatusTodo)
	childA := testIssue(2, model.StatusTodo)
	childB := testIssue(3, model.StatusReview)
	m := testDetailModel(parent, childA, childB)

	updated, _ := m.handleDetailKey("l")
	afterFocus := updated.(browserModel)
	if afterFocus.detailFocus != detailFocusSubIssues {
		t.Fatalf("detailFocus = %s, want %s", afterFocus.detailFocus, detailFocusSubIssues)
	}

	updated, _ = afterFocus.handleDetailKey("j")
	afterMove := updated.(browserModel)
	if afterMove.detailSubIndex != 1 {
		t.Fatalf("detailSubIndex = %d, want 1", afterMove.detailSubIndex)
	}

	updated, _ = afterMove.handleDetailKey("enter")
	afterOpen := updated.(browserModel)
	if afterOpen.detailTargetID != childB.ID {
		t.Fatalf("detailTargetID = %d, want %d", afterOpen.detailTargetID, childB.ID)
	}
	if !afterOpen.loadingDetail {
		t.Fatalf("expected loadingDetail to be true")
	}
	if len(afterOpen.detailHistory) != 1 || afterOpen.detailHistory[0].issueID != parent.ID {
		t.Fatalf("detailHistory = %#v, want parent issue on stack", afterOpen.detailHistory)
	}
	if afterOpen.detailFocus != detailFocusBody {
		t.Fatalf("detailFocus = %s, want %s", afterOpen.detailFocus, detailFocusBody)
	}
}

func TestEscClosesExpandedBeforeGoingBack(t *testing.T) {
	parent := testIssue(1, model.StatusTodo)
	child := testIssue(2, model.StatusTodo)
	m := testDetailModel(child)
	m.detailExpanded = true
	m.detailHistory = []detailNavState{{issueID: parent.ID, focusRegion: detailFocusSubIssues, subIssueIndex: 0}}

	updated, _ := m.handleDetailKey("esc")
	afterClose := updated.(browserModel)
	if afterClose.detailExpanded {
		t.Fatalf("expected expanded detail to close first")
	}
	if afterClose.detailTargetID != child.ID {
		t.Fatalf("detailTargetID = %d, want %d", afterClose.detailTargetID, child.ID)
	}
	if len(afterClose.detailHistory) != 1 {
		t.Fatalf("history length = %d, want 1", len(afterClose.detailHistory))
	}
	if afterClose.focus != focusDetail {
		t.Fatalf("focus = %s, want %s", afterClose.focus, focusDetail)
	}

	updated, _ = afterClose.handleDetailKey("esc")
	afterBack := updated.(browserModel)
	if afterBack.detailTargetID != parent.ID {
		t.Fatalf("detailTargetID = %d, want %d", afterBack.detailTargetID, parent.ID)
	}
	if len(afterBack.detailHistory) != 0 {
		t.Fatalf("history length = %d, want 0", len(afterBack.detailHistory))
	}
	if !afterBack.loadingDetail {
		t.Fatalf("expected loadingDetail to be true after back navigation")
	}
	if afterBack.detailFocus != detailFocusSubIssues {
		t.Fatalf("detailFocus = %s, want %s", afterBack.detailFocus, detailFocusSubIssues)
	}
}

func TestCtrlDAndCtrlUPageFocusedDetailRegion(t *testing.T) {
	issue := testIssue(1, model.StatusTodo)
	issue.Description = strings.Repeat("line\n", 80)
	childA := testIssue(2, model.StatusTodo)
	childB := testIssue(3, model.StatusTodo)
	childC := testIssue(4, model.StatusTodo)
	childD := testIssue(5, model.StatusTodo)
	m := testDetailModel(issue, childA, childB, childC, childD)

	updated, _ := m.handleDetailKey("ctrl+d")
	afterPageDown := updated.(browserModel)
	if afterPageDown.detailScroll <= 0 {
		t.Fatalf("detailScroll = %d, want > 0", afterPageDown.detailScroll)
	}

	updated, _ = afterPageDown.handleDetailKey("ctrl+u")
	afterPageUp := updated.(browserModel)
	if afterPageUp.detailScroll >= afterPageDown.detailScroll {
		t.Fatalf("detailScroll = %d, want < %d", afterPageUp.detailScroll, afterPageDown.detailScroll)
	}

	updated, _ = afterPageUp.handleDetailKey("l")
	afterFocus := updated.(browserModel)
	updated, _ = afterFocus.handleDetailKey("ctrl+d")
	afterSubPageDown := updated.(browserModel)
	if afterSubPageDown.detailSubIndex <= 0 {
		t.Fatalf("detailSubIndex = %d, want > 0", afterSubPageDown.detailSubIndex)
	}

	updated, _ = afterSubPageDown.handleDetailKey("ctrl+u")
	afterSubPageUp := updated.(browserModel)
	if afterSubPageUp.detailSubIndex >= afterSubPageDown.detailSubIndex {
		t.Fatalf("detailSubIndex = %d, want < %d", afterSubPageUp.detailSubIndex, afterSubPageDown.detailSubIndex)
	}
}

func TestUGoesToParentWhenHistoryIsEmpty(t *testing.T) {
	parentID := 1
	child := testIssue(2, model.StatusTodo)
	child.ParentID = &parentID
	m := testDetailModel(child)

	updated, _ := m.handleDetailKey("u")
	afterParent := updated.(browserModel)
	if afterParent.detailTargetID != parentID {
		t.Fatalf("detailTargetID = %d, want %d", afterParent.detailTargetID, parentID)
	}
	if len(afterParent.detailHistory) != 1 || afterParent.detailHistory[0].issueID != child.ID {
		t.Fatalf("detailHistory = %#v, want child issue on stack", afterParent.detailHistory)
	}
	if !afterParent.loadingDetail {
		t.Fatalf("expected loadingDetail to be true")
	}
}

func TestEscLeavesSubIssueFocusBeforeNavigatingBack(t *testing.T) {
	parent := testIssue(1, model.StatusTodo)
	child := testIssue(2, model.StatusTodo)
	m := testDetailModel(parent, child)
	m.detailFocus = detailFocusSubIssues
	m.detailHistory = []detailNavState{{issueID: 99, focusRegion: detailFocusBody}}

	updated, _ := m.handleDetailKey("esc")
	afterEsc := updated.(browserModel)
	if afterEsc.detailFocus != detailFocusBody {
		t.Fatalf("detailFocus = %s, want %s", afterEsc.detailFocus, detailFocusBody)
	}
	if len(afterEsc.detailHistory) != 1 {
		t.Fatalf("history length = %d, want 1", len(afterEsc.detailHistory))
	}
	if afterEsc.detailTargetID != parent.ID {
		t.Fatalf("detailTargetID = %d, want %d", afterEsc.detailTargetID, parent.ID)
	}
}

func TestUPrefersHistoryBeforeParent(t *testing.T) {
	parentID := 1
	otherID := 99
	child := testIssue(2, model.StatusTodo)
	child.ParentID = &parentID
	m := testDetailModel(child)
	m.detailHistory = []detailNavState{{issueID: otherID, focusRegion: detailFocusBody}}

	updated, _ := m.handleDetailKey("u")
	afterBack := updated.(browserModel)
	if afterBack.detailTargetID != otherID {
		t.Fatalf("detailTargetID = %d, want %d", afterBack.detailTargetID, otherID)
	}
	if len(afterBack.detailHistory) != 0 {
		t.Fatalf("history length = %d, want 0", len(afterBack.detailHistory))
	}
}

func TestTabIgnoredWhileDetailExpanded(t *testing.T) {
	m := testDetailModel(testIssue(1, model.StatusTodo))
	m.detailExpanded = true
	m.focus = focusDetail

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	afterTab := updated.(browserModel)
	if afterTab.focus != focusDetail {
		t.Fatalf("focus = %s, want %s", afterTab.focus, focusDetail)
	}
	if !afterTab.detailExpanded {
		t.Fatalf("expected detailExpanded to remain true")
	}
}

func TestTabSwitchesBetweenBrowseAndDetail(t *testing.T) {
	m := testDetailModel(testIssue(1, model.StatusTodo))
	m.focus = focusBrowse

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	afterDetail := updated.(browserModel)
	if afterDetail.focus != focusDetail {
		t.Fatalf("focus = %s, want %s", afterDetail.focus, focusDetail)
	}

	updated, _ = afterDetail.Update(tea.KeyMsg{Type: tea.KeyTab})
	afterBrowse := updated.(browserModel)
	if afterBrowse.focus != focusBrowse {
		t.Fatalf("focus = %s, want %s", afterBrowse.focus, focusBrowse)
	}
}

func TestBrowseEnterExpandsDetailInListAndBoardViews(t *testing.T) {
	listModel := browserModel{
		view:            viewList,
		focus:           focusBrowse,
		selectedIssueID: 1,
		listData:        app.IssueListData{Issues: []*model.Issue{testIssue(1, model.StatusTodo)}},
	}
	updated, _ := listModel.handleBrowseKey("enter")
	afterList := updated.(browserModel)
	if !afterList.detailExpanded || afterList.focus != focusDetail {
		t.Fatalf("list expand = expanded:%t focus:%s", afterList.detailExpanded, afterList.focus)
	}

	boardModel := browserModel{
		view:            viewBoard,
		focus:           focusBrowse,
		selectedIssueID: 2,
		boardColumns: []boardColumn{{
			Status: model.StatusTodo,
			Issues: []*model.Issue{testIssue(2, model.StatusTodo)},
		}},
	}
	updated, _ = boardModel.handleBrowseKey("enter")
	afterBoard := updated.(browserModel)
	if !afterBoard.detailExpanded || afterBoard.focus != focusDetail {
		t.Fatalf("board expand = expanded:%t focus:%s", afterBoard.detailExpanded, afterBoard.focus)
	}
}

func TestUtilityHelpersCoverEdgeCases(t *testing.T) {
	issues := []*model.Issue{testIssue(1, model.StatusTodo), testIssue(2, model.StatusDone)}
	if got := findIssueIndex(issues, 2); got != 1 {
		t.Fatalf("findIssueIndex = %d, want 1", got)
	}
	if got := findIssueIndex(issues, 99); got != -1 {
		t.Fatalf("findIssueIndex = %d, want -1", got)
	}
	if got := truncate("abcdef", 4); got != "a..." {
		t.Fatalf("truncate = %q, want %q", got, "a...")
	}
	if got := truncate("abcdef", 3); got != "abc" {
		t.Fatalf("truncate short = %q, want %q", got, "abc")
	}
	start, end := windowBounds(5, 10, 3)
	if start != 4 || end != 7 {
		t.Fatalf("windowBounds = %d/%d, want 4/7", start, end)
	}
	start, end = detailWindow(20, 10, 3)
	if start != 7 || end != 10 {
		t.Fatalf("detailWindow = %d/%d, want 7/10", start, end)
	}
}

func TestDebugLogfWritesWhenPathConfigured(t *testing.T) {
	path := t.TempDir() + "/tui.log"
	t.Setenv("DOCKET_TUI_DEBUG_LOG", path)
	debugLogf("hello %s", "world")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Fatalf("log = %q, want message", string(data))
	}
}

func TestDebugLogfIgnoresLegacyUIDebugPath(t *testing.T) {
	legacyPath := t.TempDir() + "/ui.log"
	t.Setenv("DOCKET_UI_DEBUG_LOG", legacyPath)
	debugLogf("legacy path should stay unused")

	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy ui debug path to remain unused, stat err = %v", err)
	}
}

func TestBrowseFocusedJScrollsDetailBody(t *testing.T) {
	issue := testIssue(1, model.StatusTodo)
	issue.Description = strings.Repeat("line\n", 80)
	m := testDetailModel(issue)
	m.focus = focusBrowse

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
	afterProxy := updated.(browserModel)
	if afterProxy.detailScroll <= 0 {
		t.Fatalf("detailScroll = %d, want > 0", afterProxy.detailScroll)
	}
	if afterProxy.focus != focusBrowse {
		t.Fatalf("focus = %s, want %s", afterProxy.focus, focusBrowse)
	}

	updated, _ = afterProxy.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}})
	afterReverse := updated.(browserModel)
	if afterReverse.detailScroll >= afterProxy.detailScroll {
		t.Fatalf("detailScroll = %d, want < %d", afterReverse.detailScroll, afterProxy.detailScroll)
	}
}

func TestBrowseFocusedJMovesCurrentDetailSubIssueRegion(t *testing.T) {
	parent := testIssue(1, model.StatusTodo)
	childA := testIssue(2, model.StatusTodo)
	childB := testIssue(3, model.StatusTodo)
	m := testDetailModel(parent, childA, childB)
	m.focus = focusBrowse
	m.detailFocus = detailFocusSubIssues

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
	afterProxy := updated.(browserModel)
	if afterProxy.detailSubIndex != 1 {
		t.Fatalf("detailSubIndex = %d, want 1", afterProxy.detailSubIndex)
	}
	if afterProxy.focus != focusBrowse {
		t.Fatalf("focus = %s, want %s", afterProxy.focus, focusBrowse)
	}

	updated, _ = afterProxy.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}})
	afterReverse := updated.(browserModel)
	if afterReverse.detailSubIndex != 0 {
		t.Fatalf("detailSubIndex = %d, want 0", afterReverse.detailSubIndex)
	}
}

func TestDetailFocusedJDoesNotMoveBrowsePane(t *testing.T) {
	issueA := testIssue(1, model.StatusTodo)
	issueB := testIssue(2, model.StatusTodo)
	m := testDetailModel(issueA)
	m.view = viewList
	m.focus = focusDetail
	m.listData = app.IssueListData{Issues: []*model.Issue{issueA, issueB}}
	m.listIndex = 0
	m.selectedIssueID = issueA.ID

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
	afterKey := updated.(browserModel)
	if afterKey.listIndex != 0 {
		t.Fatalf("listIndex = %d, want 0", afterKey.listIndex)
	}
	if afterKey.selectedIssueID != issueA.ID {
		t.Fatalf("selectedIssueID = %d, want %d", afterKey.selectedIssueID, issueA.ID)
	}
}

func TestBrowseFocusedJNoOpWhenDetailExpanded(t *testing.T) {
	issue := testIssue(1, model.StatusTodo)
	issue.Description = strings.Repeat("line\n", 80)
	m := testDetailModel(issue)
	m.focus = focusBrowse
	m.detailExpanded = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
	afterKey := updated.(browserModel)
	if afterKey.detailScroll != 0 {
		t.Fatalf("detailScroll = %d, want 0", afterKey.detailScroll)
	}
	if !afterKey.detailExpanded {
		t.Fatalf("expected detailExpanded to remain true")
	}
}
