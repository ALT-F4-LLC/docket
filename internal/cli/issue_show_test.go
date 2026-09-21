package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestIssueShow_RendersLinkedDocsSection(t *testing.T) {
	unsetNoColor(t)
	t.Setenv("TERM", "xterm-256color")
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue with docs", model.StatusTodo, model.PriorityHigh)
	docB := createDoc(t, conn, "Beta TDD", "tdd", "approved")
	docA := createDoc(t, conn, "Alpha UX", "ux", "draft")
	linkDocIssue(t, conn, docB, issueID)
	linkDocIssue(t, conn, docA, issueID)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

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
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

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
			err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
			testsupport.Must(t, err, "runIssueShow: %v", err)
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
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

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

func createProposal(t *testing.T, conn *sql.DB, description, status string) int {
	t.Helper()
	id, err := db.CreateProposal(conn, &model.Proposal{
		Description:    description,
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatus(status),
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	if err != nil {
		t.Fatalf("CreateProposal(%q): %v", description, err)
	}
	return id
}

func linkProposalIssue(t *testing.T, conn *sql.DB, proposalID, issueID int) {
	t.Helper()
	if err := db.LinkProposalIssue(conn, proposalID, issueID); err != nil {
		t.Fatalf("LinkProposalIssue(%d,%d): %v", proposalID, issueID, err)
	}
}

func TestIssueShow_LinksLinkedProposals(t *testing.T) {
	unsetNoColor(t)
	t.Setenv("TERM", "xterm-256color")
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue with proposals", model.StatusTodo, model.PriorityHigh)
	p1 := createProposal(t, conn, "Adopt new schema", string(model.ProposalStatusOpen))
	p2 := createProposal(t, conn, "Deprecate old API", string(model.ProposalStatusApproved))
	linkProposalIssue(t, conn, p2, issueID)
	linkProposalIssue(t, conn, p1, issueID)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

	out := buf.String()
	if !strings.Contains(out, "Linked Proposals") {
		t.Fatalf("output missing Linked Proposals header:\n%s", out)
	}
	if !strings.Contains(out, "▸") {
		t.Errorf("styled output missing ▸ prefix:\n%s", out)
	}
	for _, want := range []string{"DKT-V1", "DKT-V2", "open", "approved", "Adopt new schema", "Deprecate old API"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "DKT-V1") > strings.Index(out, "DKT-V2") {
		t.Errorf("proposals not ordered by id ascending:\n%s", out)
	}
}

func TestIssueShow_LinksLinkedProposalsPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue with proposals", model.StatusTodo, model.PriorityHigh)
	pid := createProposal(t, conn, "Adopt new schema", string(model.ProposalStatusOpen))
	linkProposalIssue(t, conn, pid, issueID)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

	out := buf.String()
	if !strings.Contains(out, "Linked Proposals") {
		t.Fatalf("plain output missing Linked Proposals header:\n%s", out)
	}
	if !strings.Contains(out, "  > DKT-V1   open   Adopt new schema") {
		t.Errorf("plain output missing expected proposal line:\n%s", out)
	}
	if strings.Contains(out, "▸") {
		t.Errorf("plain output should not contain ▸:\n%s", out)
	}
}

func TestIssueShow_LinkedProposalsDescriptionTruncated(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue", model.StatusTodo, model.PriorityHigh)
	longDesc := "This proposal description is far longer than forty characters and must be truncated"
	pid := createProposal(t, conn, longDesc, string(model.ProposalStatusOpen))
	linkProposalIssue(t, conn, pid, issueID)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

	out := buf.String()
	if strings.Contains(out, longDesc) {
		t.Errorf("long description should be truncated, got full text:\n%s", out)
	}
	if !strings.Contains(out, "This proposal description is far long...") {
		t.Errorf("expected truncated description with ellipsis:\n%s", out)
	}
}

func TestIssueShow_OmitsLinkedProposalsWhenEmpty(t *testing.T) {
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
			issueID := createIssue(t, conn, "no proposals", model.StatusTodo, model.PriorityHigh)

			cmd := cmdWithDB(conn)
			w, buf := bufWriter(false)
			err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
			testsupport.Must(t, err, "runIssueShow: %v", err)
			if strings.Contains(buf.String(), "Linked Proposals") {
				t.Errorf("empty issue should omit Linked Proposals section:\n%s", buf.String())
			}
		})
	}
}

func TestIssueShowJSON_LinkedProposalsArrayShapeAndOrder(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue", model.StatusTodo, model.PriorityHigh)
	p1 := createProposal(t, conn, "Adopt new schema", string(model.ProposalStatusOpen))
	p2 := createProposal(t, conn, "Deprecate old API", string(model.ProposalStatusApproved))
	linkProposalIssue(t, conn, p2, issueID)
	linkProposalIssue(t, conn, p1, issueID)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

	var env struct {
		Data struct {
			LinkedProposals []string `json:"linked_proposals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	got := env.Data.LinkedProposals
	want := []string{"DKT-V1", "DKT-V2"}
	if len(got) != len(want) {
		t.Fatalf("len(linked_proposals) = %d, want %d:\n%s", len(got), len(want), buf.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("linked_proposals[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIssueShowJSON_LinkedProposalsEmptyIsArray(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue", model.StatusTodo, model.PriorityHigh)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

	var raw map[string]json.RawMessage
	err = json.Unmarshal(buf.Bytes(), &raw)
	testsupport.Must(t, err, "unmarshal envelope: %v", err)
	var data map[string]json.RawMessage
	err = json.Unmarshal(raw["data"], &data)
	testsupport.Must(t, err, "unmarshal data: %v", err)
	proposalsRaw, ok := data["linked_proposals"]
	if !ok {
		t.Fatalf("linked_proposals key absent:\n%s", buf.String())
	}
	if string(proposalsRaw) != "[]" {
		t.Errorf("empty linked_proposals = %s, want []", proposalsRaw)
	}
}

func TestIssueShowJSON_DocsEmptyIsArray(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "issue", model.StatusTodo, model.PriorityHigh)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

	var raw map[string]json.RawMessage
	err = json.Unmarshal(buf.Bytes(), &raw)
	testsupport.Must(t, err, "unmarshal envelope: %v", err)
	var data map[string]json.RawMessage
	err = json.Unmarshal(raw["data"], &data)
	testsupport.Must(t, err, "unmarshal data: %v", err)
	docsRaw, ok := data["docs"]
	if !ok {
		t.Fatalf("docs key absent:\n%s", buf.String())
	}
	if string(docsRaw) != "[]" {
		t.Errorf("empty docs = %s, want []", docsRaw)
	}
}

// showScopeData runs `issue show` at the given JSON version and returns the
// decoded `.data` object.
func showScopeData(t *testing.T, conn *sql.DB, issueID int, version output.JSONVersion) map[string]json.RawMessage {
	t.Helper()

	buf := &bytes.Buffer{}
	w := &output.Writer{
		JSONMode:    true,
		JSONVersion: version,
		Stdout:      buf,
		Stderr:      &bytes.Buffer{},
	}
	err := runIssueShow(cmdWithDB(conn), []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, buf.String())
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw["data"], &data); err != nil {
		t.Fatalf("unmarshal data: %v\n%s", err, buf.String())
	}
	return data
}

// TestIssueShowV2EmitsScope pins the addition: `scope` is a v2 read surface, and it
// distinguishes the three states `issues.scope_globs` can hold.
//
// The three cases are asserted together deliberately. NULL and `[]` are
// different facts — "no scope declared" versus "declared to touch nothing"
// (internal/cli/issue_scope.go) — so a marshaler that collapsed them, which is
// exactly what `omitempty` on a []string does, would still pass a test that
// only checked the populated case.
func TestIssueShowV2EmitsScope(t *testing.T) {
	tests := []struct {
		name  string
		globs string // as stored in scope_globs; "" means SQL NULL
		want  string
	}{
		{"declared", `["x/**","y/*.go"]`, `["x/**","y/*.go"]`},
		{"undeclared renders null", "", `null`},
		{"declared empty renders []", `[]`, `[]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := newTestDB(t)
			issueID := createIssue(t, conn, "scoped", model.StatusTodo, model.PriorityHigh)
			err := db.SetIssueScopeGlobs(conn, issueID, tt.globs)
			testsupport.Must(t, err, "SetIssueScopeGlobs: %v", err)

			data := showScopeData(t, conn, issueID, output.JSONV2)

			scope, ok := data["scope"]
			if !ok {
				t.Fatalf("v2 payload has no scope key; keys = %v", keysOf(data))
			}
			if got := string(scope); got != tt.want {
				t.Errorf("scope = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestIssueShowV1ScopeWhenDeclared pins the DKT-55 amendment to the v1
// freeze: a DECLARED scope reaches v1 — the surface everyone checks first —
// while an issue with no declared scope keeps the byte-identical frozen shape
// (no `scope` key at all), so dormancy holds for every repo that never used
// --scope.
func TestIssueShowV1ScopeWhenDeclared(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "scoped", model.StatusTodo, model.PriorityHigh)
	err := db.SetIssueScopeGlobs(conn, issueID, `["x/**"]`)
	testsupport.Must(t, err, "SetIssueScopeGlobs: %v", err)

	data := showScopeData(t, conn, issueID, output.JSONV1)
	scope, ok := data["scope"]
	if !ok {
		t.Fatalf("v1 payload omits a DECLARED scope (DKT-55); keys = %v", keysOf(data))
	}
	if got := string(scope); got != `["x/**"]` {
		t.Errorf("scope = %s, want [\"x/**\"]", got)
	}

	bare := createIssue(t, conn, "bare", model.StatusTodo, model.PriorityHigh)
	data = showScopeData(t, conn, bare, output.JSONV1)
	if _, ok := data["scope"]; ok {
		t.Errorf("v1 payload gained a scope key on an issue with NO declared "+
			"scope; the dormant shape is frozen. keys = %v", keysOf(data))
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestIssueShow_SingleIDShapeUnchanged is DKT-97's compatibility half: a
// single id must keep emitting the exact frozen flat object, not a
// one-element array — no caller parsing `docket issue show DKT-1` today
// should see its shape move.
func TestIssueShow_SingleIDShapeUnchanged(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "solo", model.StatusTodo, model.PriorityHigh)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	err := runIssueShow(cmd, []string{model.FormatID(issueID)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

	var env struct {
		Data struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.Data.ID != model.FormatID(issueID) || env.Data.Title != "solo" {
		t.Errorf("data = %+v, want a flat single-issue object", env.Data)
	}
}

// TestIssueShow_MultipleIDsEmitAnArray is DKT-97: two or more ids emit a
// JSON array of the same per-issue shape under data, so a conductor's batch
// read is one call instead of a shell for-loop plus jq.
func TestIssueShow_MultipleIDsEmitAnArray(t *testing.T) {
	conn := newTestDB(t)
	a := createIssue(t, conn, "first", model.StatusTodo, model.PriorityHigh)
	b := createIssue(t, conn, "second", model.StatusTodo, model.PriorityLow)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	err := runIssueShow(cmd, []string{model.FormatID(a), model.FormatID(b)}, w)
	testsupport.Must(t, err, "runIssueShow: %v", err)

	var env struct {
		Data []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(env.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2:\n%s", len(env.Data), buf.String())
	}
	if env.Data[0].ID != model.FormatID(a) || env.Data[0].Title != "first" {
		t.Errorf("data[0] = %+v, want issue %s", env.Data[0], model.FormatID(a))
	}
	if env.Data[1].ID != model.FormatID(b) || env.Data[1].Title != "second" {
		t.Errorf("data[1] = %+v, want issue %s", env.Data[1], model.FormatID(b))
	}
}

// TestIssueShow_MultipleIDsRefusesUnknownID confirms a batch naming one
// nonexistent id refuses NOT_FOUND rather than silently returning fewer
// results than requested.
func TestIssueShow_MultipleIDsRefusesUnknownID(t *testing.T) {
	conn := newTestDB(t)
	a := createIssue(t, conn, "first", model.StatusTodo, model.PriorityHigh)

	cmd := cmdWithDB(conn)
	w, _ := bufWriter(true)
	err := runIssueShow(cmd, []string{model.FormatID(a), "DKT-9999"}, w)
	if err == nil {
		t.Fatal("a batch naming a nonexistent id succeeded")
	}
	var cmdError *CmdError
	if !asCmdError(err, &cmdError) || cmdError.Code != output.ErrNotFound {
		t.Errorf("error = %v, want NOT_FOUND", err)
	}
}

// abandonInRun records the fact `run abandon --issue` records — a run, and an
// `issue-abandoned` event naming the issue and carrying the operator's ruling.
//
// It writes the rows rather than driving the engine verb because what is under
// test here is the READ: `issue show` has to reach the disposition on its own,
// and a fixture that activated a run and drove it to a park would be testing
// activation. The payload is the shape engine.AbandonIssueInRun marshals, and
// internal/engine's own tests cover that the two agree.
func abandonInRun(t *testing.T, conn *sql.DB, issueID int, reason string, atMS int64) int {
	t.Helper()
	run, err := db.InsertRun(conn, 0, "a run", 0, atMS)
	testsupport.Must(t, err, "InsertRun: %v", err)
	payload, err := json.Marshal(map[string]any{
		"issue": model.FormatID(issueID), "reason": reason,
	})
	testsupport.Must(t, err, "marshalling the payload: %v", err)
	_, err = conn.Exec(
		`INSERT INTO events (at_ms, kind, run_id, issue_id, data)
		 VALUES (?, 'issue-abandoned', ?, ?, ?)`,
		atMS, run.ID, issueID, string(payload))
	testsupport.Must(t, err, "recording the abandonment: %v", err)
	return run.ID
}

// DKT-404 — `issue show` gave no hint that a run had abandoned its work.
//
// The status an abandon leaves frozen (`todo`, `review`) is indistinguishable
// from work nobody has started, and the disposition lived only in the event
// log. A conduct session read two such issues as "still parked on a gate that
// was never resolved" and re-presented both decisions to the operator.

func TestIssueShow_SurfacesTheRunDisposition(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	conn := newTestDB(t)
	// The evidence's own shape: the issue is still open at `review`.
	issueID := createIssue(t, conn, "the abandoned work", model.StatusReview, model.PriorityHigh)
	const ruling = "operator selected: Stop the issue, re-plan it later — " +
		"findings preserved as a follow-up"
	runID := abandonInRun(t, conn, issueID, ruling, 1755000000000)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	testsupport.Must(t, runIssueShow(cmd, []string{model.FormatID(issueID)}, w),
		"runIssueShow: %v", nil)

	out := buf.String()
	for _, want := range []string{
		render.RunDispositionHeading,
		"work abandoned in " + model.FormatRunID(runID),
		ruling,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`issue show` output is missing %q; a reader still has to "+
				"run `events list` to learn the issue was abandoned:\n%s", want, out)
		}
	}
	// The date, so the line is a key into `run report` and `events list`
	// rather than a bare assertion.
	if !strings.Contains(out, "2025-08-12") {
		t.Errorf("the disposition line carries no date:\n%s", out)
	}
}

func TestIssueShow_OmitsTheRunDispositionWhenNoRunAbandonedIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noColor bool
	}{{"styled", false}, {"plain", true}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noColor {
				t.Setenv("NO_COLOR", "1")
			} else {
				t.Setenv("TERM", "xterm-256color")
			}
			conn := newTestDB(t)
			issueID := createIssue(t, conn, "ordinary work", model.StatusTodo, model.PriorityMedium)

			cmd := cmdWithDB(conn)
			w, buf := bufWriter(false)
			testsupport.Must(t, runIssueShow(cmd, []string{model.FormatID(issueID)}, w),
				"runIssueShow: %v", nil)

			if strings.Contains(buf.String(), render.RunDispositionHeading) {
				t.Errorf("an issue no run abandoned still prints a disposition:\n%s",
					buf.String())
			}
		})
	}
}

func TestIssueShowJSON_CarriesTheRunDisposition(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "the abandoned work", model.StatusTodo, model.PriorityHigh)
	const ruling = "pin drift; re-planned as a replacement issue"
	runID := abandonInRun(t, conn, issueID, ruling, 1755000000000)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	testsupport.Must(t, runIssueShow(cmd, []string{model.FormatID(issueID)}, w),
		"runIssueShow: %v", nil)

	var env struct {
		Data struct {
			Status         string `json:"status"`
			RunDisposition *struct {
				Run         string `json:"run"`
				Disposition string `json:"disposition"`
				By          string `json:"by"`
				Reason      string `json:"reason"`
				At          string `json:"at"`
			} `json:"run_disposition"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	d := env.Data.RunDisposition
	if d == nil {
		t.Fatalf("--json carries no run_disposition:\n%s", buf.String())
	}
	if d.Run != model.FormatRunID(runID) {
		t.Errorf("run = %q, want %q", d.Run, model.FormatRunID(runID))
	}
	if d.Disposition != "abandoned" {
		t.Errorf("disposition = %q, want %q", d.Disposition, "abandoned")
	}
	if d.Reason != ruling {
		t.Errorf("reason = %q, want the recorded ruling %q", d.Reason, ruling)
	}
	if d.By != "" {
		t.Errorf("by = %q; no step decided a run-level abandon", d.By)
	}
	if d.At != "2025-08-12T12:00:00Z" {
		t.Errorf("at = %q, want the event's own time as RFC3339", d.At)
	}
	// The status is still what the run left it at — the disposition explains
	// the frozen row, it does not overwrite it.
	if env.Data.Status != string(model.StatusTodo) {
		t.Errorf("status = %q, want %q", env.Data.Status, model.StatusTodo)
	}
}

// TestIssueShowJSON_OmitsTheRunDispositionKey keeps the frozen v1 shape frozen
// for every issue no run abandoned, which is nearly all of them (DKT-55's
// rule, applied a third time).
func TestIssueShowJSON_OmitsTheRunDispositionKey(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "ordinary work", model.StatusTodo, model.PriorityMedium)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	testsupport.Must(t, runIssueShow(cmd, []string{model.FormatID(issueID)}, w),
		"runIssueShow: %v", nil)

	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if _, ok := env.Data["run_disposition"]; ok {
		t.Errorf("an issue no run abandoned emits a run_disposition key:\n%s",
			buf.String())
	}
}

// TestIssueShowPrimaryKeyAlias pins DKT-452: `issue show` serves the issue's id
// under BOTH `id` — the original spelling — and `issue`, the noun every other
// verb keys its primary entity by (`run status` -> run, `step show` -> step,
// `dispatch open` -> dispatch). A caller that had just parsed a run or a step
// reached for `.data.issue` here, got null, and had to dump the key set to
// recover.
//
// Both versions are asserted because the alias is UNCONDITIONAL — the one
// amendment to the v1 freeze that is not gated on a fact being present, since a
// conditional alias would be absent in precisely the case it exists to serve.
// The nested sub-issue is asserted too: `sub_issues` rows are marshaled
// model.Issue values, a different code path from this payload's own struct, and
// a reader walking the tree hits the same guess there.
func TestIssueShowPrimaryKeyAlias(t *testing.T) {
	versions := []struct {
		name    string
		version output.JSONVersion
	}{
		{"v1", output.JSONV1},
		{"v2", output.JSONV2},
	}

	for _, tt := range versions {
		t.Run(tt.name, func(t *testing.T) {
			conn := newTestDB(t)
			parentID := createIssue(t, conn, "parent", model.StatusTodo, model.PriorityHigh)
			childID, err := db.CreateIssue(conn, &model.Issue{
				Title:    "child",
				Status:   model.StatusTodo,
				Priority: model.PriorityHigh,
				Kind:     model.IssueKindFeature,
				ParentID: &parentID,
			}, nil, nil)
			testsupport.Must(t, err, "CreateIssue(child): %v", err)

			data := showScopeData(t, conn, parentID, tt.version)

			want := `"` + model.FormatID(parentID) + `"`
			for _, key := range []string{"id", "issue"} {
				raw, ok := data[key]
				if !ok {
					t.Fatalf("payload has no %q key; keys = %v", key, keysOf(data))
				}
				if string(raw) != want {
					t.Errorf("%s = %s, want %s", key, raw, want)
				}
			}

			var subs []map[string]json.RawMessage
			if err := json.Unmarshal(data["sub_issues"], &subs); err != nil {
				t.Fatalf("unmarshal sub_issues: %v", err)
			}
			if len(subs) != 1 {
				t.Fatalf("sub_issues = %d rows, want 1", len(subs))
			}
			wantChild := `"` + model.FormatID(childID) + `"`
			for _, key := range []string{"id", "issue"} {
				if got := string(subs[0][key]); got != wantChild {
					t.Errorf("sub_issues[0].%s = %s, want %s", key, got, wantChild)
				}
			}
		})
	}
}
