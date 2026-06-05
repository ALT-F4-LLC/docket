package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

func TestIssueShow_RendersLinkedDocsSection(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue with docs", model.StatusTodo, model.PriorityHigh)
	docB := createDoc(t, conn, "Beta TDD", "tdd", "approved")
	docA := createDoc(t, conn, "Alpha UX", "ux", "draft")
	linkDocIssue(t, conn, docB, issueID)
	linkDocIssue(t, conn, docA, issueID)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	if err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w); err != nil {
		t.Fatalf("runIssueShow: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Linked Docs") {
		t.Fatalf("output missing Linked Docs header:\n%s", out)
	}
	if !strings.Contains(out, "▸") {
		t.Errorf("styled output missing ▸ prefix:\n%s", out)
	}
	for _, want := range []string{"DOC-1", "DOC-2", "tdd", "ux", "approved", "draft", "Beta TDD", "Alpha UX"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "DOC-1") > strings.Index(out, "DOC-2") {
		t.Errorf("docs not ordered by id ascending:\n%s", out)
	}
}

func TestIssueShow_RendersLinkedDocsSectionPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue with docs", model.StatusTodo, model.PriorityHigh)
	doc := createDoc(t, conn, "Docket Doc CLI", "tdd", "approved")
	linkDocIssue(t, conn, doc, issueID)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	if err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w); err != nil {
		t.Fatalf("runIssueShow: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Linked Docs") {
		t.Fatalf("plain output missing Linked Docs header:\n%s", out)
	}
	if !strings.Contains(out, "  > DOC-1   tdd   approved   Docket Doc CLI") {
		t.Errorf("plain output missing expected doc line:\n%s", out)
	}
	if strings.Contains(out, "▸") {
		t.Errorf("plain output should not contain ▸:\n%s", out)
	}
}

func TestIssueShow_OmitsLinkedDocsWhenEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noColor bool
	}{
		{"styled", false},
		{"plain", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noColor {
				t.Setenv("NO_COLOR", "1")
			} else {
				t.Setenv("TERM", "xterm-256color")
			}
			conn := newTestDB(t)
			issueID := createIssue(t, conn, "no docs", model.StatusTodo, model.PriorityHigh)

			cmd := cmdWithDB(conn)
			w, buf := bufWriter(false)
			if err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w); err != nil {
				t.Fatalf("runIssueShow: %v", err)
			}
			if strings.Contains(buf.String(), "Linked Docs") {
				t.Errorf("empty issue should omit Linked Docs section:\n%s", buf.String())
			}
		})
	}
}

func TestIssueShowJSON_DocsArrayShapeAndOrder(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue", model.StatusTodo, model.PriorityHigh)
	docB := createDoc(t, conn, "Beta", "adr", "accepted")
	docA := createDoc(t, conn, "Alpha", "tdd", "approved")
	linkDocIssue(t, conn, docB, issueID)
	linkDocIssue(t, conn, docA, issueID)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	if err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w); err != nil {
		t.Fatalf("runIssueShow: %v", err)
	}

	var env struct {
		Data struct {
			Docs []struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"docs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	docs := env.Data.Docs
	if len(docs) != 2 {
		t.Fatalf("len(docs) = %d, want 2:\n%s", len(docs), buf.String())
	}
	if docs[0].ID != "DOC-1" || docs[1].ID != "DOC-2" {
		t.Errorf("docs not ordered by id asc: got %s, %s", docs[0].ID, docs[1].ID)
	}
	if docs[0].Type != "adr" || docs[0].Title != "Beta" || docs[0].Status != "accepted" {
		t.Errorf("doc[0] shape wrong: %+v", docs[0])
	}
}

func TestIssueShowJSON_DocsEmptyIsArray(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue", model.StatusTodo, model.PriorityHigh)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	if err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w); err != nil {
		t.Fatalf("runIssueShow: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw["data"], &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	docsRaw, ok := data["docs"]
	if !ok {
		t.Fatalf("docs key absent:\n%s", buf.String())
	}
	if string(docsRaw) != "[]" {
		t.Errorf("empty docs = %s, want []", docsRaw)
	}
}
