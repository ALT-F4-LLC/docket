package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

func linkProposalDoc(t *testing.T, conn *sql.DB, proposalID, docID int) {
	t.Helper()
	if err := db.LinkProposalDoc(conn, proposalID, docID); err != nil {
		t.Fatalf("LinkProposalDoc(%d,%d): %v", proposalID, docID, err)
	}
}

func TestVoteShowJSON_LinkedDocsArrayShapeAndOrder(t *testing.T) {
	conn := newTestDB(t)
	pid := createProposal(t, conn, "Ratify the TDD", string(model.ProposalStatusOpen))
	docB := createDoc(t, conn, "Beta", "adr", "accepted")
	docA := createDoc(t, conn, "Alpha", "tdd", "approved")
	linkProposalDoc(t, conn, pid, docB)
	linkProposalDoc(t, conn, pid, docA)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	if err := runVoteShow(cmd, []string{model.FormatProposalID(pid)}, w); err != nil {
		t.Fatalf("runVoteShow: %v", err)
	}

	var env struct {
		Data struct {
			LinkedDocs []string `json:"linked_docs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	got := env.Data.LinkedDocs
	want := []string{"DOC-1", "DOC-2"}
	if len(got) != len(want) {
		t.Fatalf("len(linked_docs) = %d, want %d:\n%s", len(got), len(want), buf.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("linked_docs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestVoteShowJSON_LinkedDocsEmptyIsArray(t *testing.T) {
	conn := newTestDB(t)
	pid := createProposal(t, conn, "No docs", string(model.ProposalStatusOpen))

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	if err := runVoteShow(cmd, []string{model.FormatProposalID(pid)}, w); err != nil {
		t.Fatalf("runVoteShow: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw["data"], &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	docsRaw, ok := data["linked_docs"]
	if !ok {
		t.Fatalf("linked_docs key absent:\n%s", buf.String())
	}
	if string(docsRaw) != "[]" {
		t.Errorf("empty linked_docs = %s, want []", docsRaw)
	}
}

func TestVoteShow_RendersLinkedDocsSection(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	conn := newTestDB(t)
	pid := createProposal(t, conn, "Ratify the TDD", string(model.ProposalStatusOpen))
	doc := createDoc(t, conn, "Docket Doc CLI", "tdd", "approved")
	linkProposalDoc(t, conn, pid, doc)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	if err := runVoteShow(cmd, []string{model.FormatProposalID(pid)}, w); err != nil {
		t.Fatalf("runVoteShow: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Linked Docs") {
		t.Fatalf("styled output missing Linked Docs header:\n%s", out)
	}
	if !strings.Contains(out, "DOC-1") {
		t.Errorf("styled output missing DOC-1:\n%s", out)
	}
}

func TestVoteShow_OmitsLinkedDocsWhenEmpty(t *testing.T) {
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
			pid := createProposal(t, conn, "No docs", string(model.ProposalStatusOpen))

			cmd := cmdWithDB(conn)
			w, buf := bufWriter(false)
			if err := runVoteShow(cmd, []string{model.FormatProposalID(pid)}, w); err != nil {
				t.Fatalf("runVoteShow: %v", err)
			}
			if strings.Contains(buf.String(), "Linked Docs") {
				t.Errorf("empty proposal should omit Linked Docs section:\n%s", buf.String())
			}
		})
	}
}
