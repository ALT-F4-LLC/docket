package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

func docListCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().StringSliceP("type", "T", nil, "")
	cmd.Flags().StringSliceP("status", "s", nil, "")
	cmd.Flags().StringP("author", "a", "", "")
	cmd.Flags().String("sort", "", "")
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Bool("with-body", false, "")
	return cmd
}

// resumePromptBody is a stand-in for the real thing: a multi-kilobyte handoff
// document. Size is what makes this corpus a reproduction of DKT-1045 rather
// than a shape test, so the body is deliberately long.
func resumePromptBody(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Resume prompt %d\n\n", n)
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&b, "Step %d: the conductor dispatched the wave, the executor "+
			"reported, and the gate seated a panel of three judges.\n", i)
	}
	return b.String()
}

// seedResumePrompts creates 11 resume-prompt docs with real bodies plus one
// document of another type, so the type filter has something to exclude.
func seedResumePrompts(t *testing.T, conn *sql.DB) []string {
	t.Helper()

	bodies := make([]string, 0, 11)
	for i := 1; i <= 11; i++ {
		body := resumePromptBody(i)
		bodies = append(bodies, body)
		_, err := db.CreateDoc(conn, &model.Doc{
			Type:   "resume-prompt",
			Status: "active",
			Title:  fmt.Sprintf("Resume prompt for RUN-%d", i),
			Body:   body,
			Author: "conduct",
		})
		testsupport.Must(t, err, "CreateDoc(%d): %v", i, err)
	}

	_, err := db.CreateDoc(conn, &model.Doc{
		Type:   "tdd",
		Status: "approved",
		Title:  "Not a resume prompt",
		Body:   resumePromptBody(99),
		Author: "tester",
	})
	testsupport.Must(t, err, "CreateDoc(other type): %v", err)

	return bodies
}

// TestDocListJSON_IsSummaryRows is DKT-1045's acceptance criterion, measured:
// `doc list -T resume-prompt --json` over eleven real resume prompts must come
// back under 4KB, and no row may carry a body. Before the fix the same corpus
// produced tens of kilobytes — too large for a harness tool result — to answer
// a question that needs only ids and titles.
func TestDocListJSON_IsSummaryRows(t *testing.T) {
	conn := newTestDB(t)
	bodies := seedResumePrompts(t, conn)

	cmd := docListCmdWithDB(conn)
	setFlags(t, cmd, map[string]string{"type": "resume-prompt", "sort": "id:asc"})
	w, buf := bufWriter(true)
	if err := runDocList(cmd, nil, w); err != nil {
		t.Fatalf("runDocList: %v", err)
	}

	if buf.Len() >= 4096 {
		t.Errorf("doc list -T resume-prompt --json = %d bytes, want < 4096", buf.Len())
	}

	var env struct {
		Data struct {
			Docs  []map[string]json.RawMessage `json:"docs"`
			Total int                          `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	if len(env.Data.Docs) != 11 || env.Data.Total != 11 {
		t.Fatalf("docs = %d, total = %d, want 11 and 11", len(env.Data.Docs), env.Data.Total)
	}

	for i, row := range env.Data.Docs {
		if _, ok := row["body"]; ok {
			t.Errorf("row %d carries a body key; listings must be body-free", i)
		}
		for _, key := range []string{"id", "type", "status", "title", "author", "created_at", "updated_at", "body_bytes"} {
			if _, ok := row[key]; !ok {
				t.Errorf("row %d is missing %q; got keys %v", i, key, docRowKeys(row))
			}
		}
		var bodyBytes int
		if err := json.Unmarshal(row["body_bytes"], &bodyBytes); err != nil {
			t.Fatalf("row %d body_bytes: %v", i, err)
		}
		if bodyBytes != len(bodies[i]) {
			t.Errorf("row %d body_bytes = %d, want %d", i, bodyBytes, len(bodies[i]))
		}
	}

	// The body is absent, not emptied: no row may smuggle it back as a value.
	if strings.Contains(buf.String(), "the conductor dispatched the wave") {
		t.Error("listing output contains body prose; the body must live in doc show")
	}
}

// TestDocListJSON_WithBodyRestoresBodies pins the escape hatch: --with-body
// re-emits the pre-DKT-1045 row, body and all, for the caller that genuinely
// wants every body in one call.
func TestDocListJSON_WithBodyRestoresBodies(t *testing.T) {
	conn := newTestDB(t)
	bodies := seedResumePrompts(t, conn)

	cmd := docListCmdWithDB(conn)
	setFlags(t, cmd, map[string]string{
		"type": "resume-prompt", "sort": "id:asc", "with-body": "true",
	})
	w, buf := bufWriter(true)
	if err := runDocList(cmd, nil, w); err != nil {
		t.Fatalf("runDocList --with-body: %v", err)
	}

	var env struct {
		Data struct {
			Docs []struct {
				ID   string `json:"id"`
				Body string `json:"body"`
			} `json:"docs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	if len(env.Data.Docs) != 11 {
		t.Fatalf("docs = %d, want 11", len(env.Data.Docs))
	}
	for i, row := range env.Data.Docs {
		if row.Body != bodies[i] {
			t.Errorf("row %d body = %d bytes, want the full %d-byte body",
				i, len(row.Body), len(bodies[i]))
		}
	}
	if buf.Len() < 4096 {
		t.Errorf("--with-body output = %d bytes; the bodies did not come back", buf.Len())
	}
}

// TestDocListJSON_EmptyIsAnArray guards the nil-slice trap: a listing that
// matches nothing must emit `[]`, not `null`.
func TestDocListJSON_EmptyIsAnArray(t *testing.T) {
	conn := newTestDB(t)

	cmd := docListCmdWithDB(conn)
	w, buf := bufWriter(true)
	if err := runDocList(cmd, nil, w); err != nil {
		t.Fatalf("runDocList: %v", err)
	}

	var env struct {
		Data struct {
			Docs json.RawMessage `json:"docs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if got := string(env.Data.Docs); got != "[]" {
		t.Errorf("docs = %s, want []", got)
	}
}

// TestDocListJSONV2_EnvelopeStillCounts guards the v2 collection envelope
// across the payload change: `docs` is now `any`, so truncation is computed
// from a stored row count rather than len() on a typed slice, and a wrong
// count would be visible only here.
func TestDocListJSONV2_EnvelopeStillCounts(t *testing.T) {
	conn := newTestDB(t)
	seedResumePrompts(t, conn)

	cmd := docListCmdWithDB(conn)
	setFlags(t, cmd, map[string]string{"type": "resume-prompt", "limit": "5"})
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	if err := runDocList(cmd, nil, w); err != nil {
		t.Fatalf("runDocList --json=v2 --limit 5: %v", err)
	}

	var env struct {
		Data struct {
			Items     []map[string]json.RawMessage `json:"items"`
			Total     int                          `json:"total"`
			Truncated bool                         `json:"truncated"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(env.Data.Items) != 5 || env.Data.Total != 11 || !env.Data.Truncated {
		t.Errorf("v2 envelope = %d items, total %d, truncated %v; want 5, 11, true",
			len(env.Data.Items), env.Data.Total, env.Data.Truncated)
	}
	if _, ok := env.Data.Items[0]["body"]; ok {
		t.Error("v2 item carries a body key; listings must be body-free")
	}
}

// TestDocShowJSON_StillCarriesTheBody is the other half of DKT-1045: the body
// moved OUT of the listing on the promise that `doc show` still has it. This
// test is what makes that promise checkable.
func TestDocShowJSON_StillCarriesTheBody(t *testing.T) {
	conn := newTestDB(t)
	body := resumePromptBody(1)
	id, err := db.CreateDoc(conn, &model.Doc{
		Type:   "resume-prompt",
		Status: "active",
		Title:  "Resume prompt for RUN-1",
		Body:   body,
		Author: "conduct",
	})
	testsupport.Must(t, err, "CreateDoc: %v", err)

	cmd := cmdWithDB(conn)
	cmd.Flags().Int("rev", 0, "")
	w, buf := bufWriter(true)
	if err := runDocShow(cmd, []string{model.FormatDocID(id)}, w); err != nil {
		t.Fatalf("runDocShow: %v", err)
	}

	var env struct {
		Data struct {
			ID              string          `json:"id"`
			Title           string          `json:"title"`
			Body            string          `json:"body"`
			Revisions       json.RawMessage `json:"revisions"`
			Comments        json.RawMessage `json:"comments"`
			LinkedIssues    json.RawMessage `json:"linked_issues"`
			LinkedProposals json.RawMessage `json:"linked_proposals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	if env.Data.ID != model.FormatDocID(id) || env.Data.Title != "Resume prompt for RUN-1" {
		t.Errorf("doc show identity = %q / %q, want %q / %q",
			env.Data.ID, env.Data.Title, model.FormatDocID(id), "Resume prompt for RUN-1")
	}
	if env.Data.Body != body {
		t.Errorf("doc show body = %d bytes, want the full %d-byte body",
			len(env.Data.Body), len(body))
	}
	// The rest of the show shape is untouched by this change.
	for name, raw := range map[string]json.RawMessage{
		"comments":         env.Data.Comments,
		"linked_issues":    env.Data.LinkedIssues,
		"linked_proposals": env.Data.LinkedProposals,
	} {
		if string(raw) != "[]" {
			t.Errorf("doc show %s = %s, want []", name, raw)
		}
	}
	if string(env.Data.Revisions) == "[]" {
		t.Error("doc show revisions = [], want the create revision")
	}
}

// setFlags sets several flags on cmd, failing the test on an unknown one.
func setFlags(t *testing.T, cmd *cobra.Command, values map[string]string) {
	t.Helper()
	for name, value := range values {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s=%s: %v", name, value, err)
		}
	}
}

func docRowKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
