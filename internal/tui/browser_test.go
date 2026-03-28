package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ALT-F4-LLC/docket/internal/app"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

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

func TestBoardViewNarrowLayoutUsesSingleColumnHeader(t *testing.T) {
	m := browserModel{
		width:  90,
		height: 18,
		view:   viewBoard,
		focus:  focusBrowse,
		boardColumns: []boardColumn{{
			Status: model.StatusTodo,
			Issues: []*model.Issue{testIssue(1, model.StatusTodo)},
		}},
		selectedIssueID: 1,
	}

	rendered := m.View()
	if strings.Count(rendered, "TODO") != 1 {
		t.Fatalf("expected a single TODO header in narrow board view, got %q", rendered)
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
