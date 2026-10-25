package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func listCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().StringSlice("status", nil, "")
	cmd.Flags().StringSlice("priority", nil, "")
	cmd.Flags().StringSlice("label", nil, "")
	cmd.Flags().StringSlice("type", nil, "")
	cmd.Flags().String("assignee", "", "")
	cmd.Flags().String("parent", "", "")
	cmd.Flags().Bool("roots", false, "")
	cmd.Flags().Bool("tree", false, "")
	cmd.Flags().String("sort", "", "")
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Bool("all", false, "")
	return cmd
}

// listJSON decodes the v2 collection envelope an `issue list` emits.
type listJSON struct {
	Data struct {
		Items []struct {
			ID    string   `json:"id"`
			Files []string `json:"files"`
			Docs  []struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"docs"`
		} `json:"items"`
		Total     int  `json:"total"`
		Truncated bool `json:"truncated"`
	} `json:"data"`
}

func TestListJSON_HydratesFilesAndDocs(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssueWithFile(t, conn, "ready", "internal/db/doc_links.go")
	doc := createDoc(t, conn, "Docket Doc CLI", "tdd", "approved")
	linkDocIssue(t, conn, doc, issueID)

	cmd := listCmdWithDB(conn)
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err := runIssueList(cmd, nil, w)
	testsupport.Must(t, err, "runIssueList: %v", err)

	var lj listJSON
	if err := json.Unmarshal(buf.Bytes(), &lj); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(lj.Data.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(lj.Data.Items))
	}
	iss := lj.Data.Items[0]
	if len(iss.Files) != 1 || iss.Files[0] != "internal/db/doc_links.go" {
		t.Errorf("files = %v, want [internal/db/doc_links.go]", iss.Files)
	}
	if len(iss.Docs) != 1 {
		t.Fatalf("docs = %d, want 1", len(iss.Docs))
	}
	if iss.Docs[0].ID != "DOC-1" || iss.Docs[0].Type != "tdd" || iss.Docs[0].Status != "approved" || iss.Docs[0].Title != "Docket Doc CLI" {
		t.Errorf("doc shape wrong: %+v", iss.Docs[0])
	}
}

func TestListJSON_FilesAndDocsEmptyAreArrays(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "ready no context", model.StatusTodo, model.PriorityHigh)

	cmd := listCmdWithDB(conn)
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err := runIssueList(cmd, nil, w)
	testsupport.Must(t, err, "runIssueList: %v", err)

	var env struct {
		Data struct {
			Items []map[string]json.RawMessage `json:"items"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if len(env.Data.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(env.Data.Items))
	}
	for _, key := range []string{"files", "docs"} {
		if got := string(env.Data.Items[0][key]); got != "[]" {
			t.Errorf("%s = %s, want []", key, got)
		}
	}
}

// DKT-405 item 3 — `issue list --run RUN-N`.
//
// A conductor's first reach for a run's roster is `docket issue list --run
// RUN-14`, and it answered `unknown flag: --run`. The roster existed only
// inside `run status --json`, which is a document to parse rather than a
// listing to read.

// listCmdWithRun is listCmdWithDB plus the flags the run filter reads.
func listCmdWithRun(conn *sql.DB, runRef string) *cobra.Command {
	cmd := listCmdWithDB(conn)
	cmd.Flags().String("run", "", "")
	cmd.Flags().String("project", "", "")
	if runRef != "" {
		if err := cmd.Flags().Set("run", runRef); err != nil {
			panic(err)
		}
	}
	return cmd
}

// TestIssueListRunFlagListsTheRoster is the acceptance criterion: the bound
// issues, and nothing else in the store.
func TestIssueListRunFlagListsTheRoster(t *testing.T) {
	conn := newTestDB(t)
	bound := createIssue(t, conn, "bound work", model.StatusTodo, model.PriorityHigh)
	alsoBound := createIssue(t, conn, "more bound work", model.StatusInProgress, model.PriorityHigh)
	createIssue(t, conn, "nothing to do with the run", model.StatusTodo, model.PriorityHigh)
	run := planningRun(t, conn, bound, alsoBound)

	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	if err := runIssueList(listCmdWithRun(conn, run.Ref()), nil, w); err != nil {
		t.Fatalf("runIssueList --run: %v", err)
	}

	var lj listJSON
	if err := json.Unmarshal(buf.Bytes(), &lj); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	got := make(map[string]bool, len(lj.Data.Items))
	for _, i := range lj.Data.Items {
		got[i.ID] = true
	}
	if len(got) != 2 {
		t.Fatalf("listed %d issues, want the run's two: %s", len(got), buf.String())
	}
	for _, want := range []int{bound, alsoBound} {
		if !got[model.FormatID(want)] {
			t.Errorf("%s is bound to %s and is not in the listing: %v",
				model.FormatID(want), run.Ref(), got)
		}
	}
	if lj.Data.Total != 2 {
		t.Errorf("total = %d, want 2 — the pre-limit count is not run-scoped",
			lj.Data.Total)
	}
}

// TestIssueListRunFlagIncludesDoneIssues: a roster is a CLOSED SET, and the
// post-mortem case this flag exists for is mostly finished issues. Hiding them
// by default would answer "which issues did RUN-14 carry" with "the ones it
// did not finish".
func TestIssueListRunFlagIncludesDoneIssues(t *testing.T) {
	conn := newTestDB(t)
	finished := createIssue(t, conn, "finished work", model.StatusDone, model.PriorityHigh)
	open := createIssue(t, conn, "open work", model.StatusTodo, model.PriorityHigh)
	run := planningRun(t, conn, finished, open)

	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	if err := runIssueList(listCmdWithRun(conn, run.Ref()), nil, w); err != nil {
		t.Fatalf("runIssueList --run: %v", err)
	}

	var lj listJSON
	if err := json.Unmarshal(buf.Bytes(), &lj); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(lj.Data.Items) != 2 {
		t.Fatalf("listed %d issues, want the whole roster including the done "+
			"one: %s", len(lj.Data.Items), buf.String())
	}
}

// TestIssueListRunFlagOnAnUnknownRun: a typo reads as a miss, not as an
// unfiltered listing of the whole project.
func TestIssueListRunFlagOnAnUnknownRun(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "some work", model.StatusTodo, model.PriorityHigh)

	w, _ := bufWriter(true)
	err := runIssueList(listCmdWithRun(conn, "RUN-404"), nil, w)
	if err == nil {
		t.Fatal("an unknown run listed something instead of failing")
	}
	if !strings.Contains(err.Error(), "RUN-404") {
		t.Errorf("the refusal does not name the run: %v", err)
	}
	assertErrCode(t, err, output.ErrNotFound)
}

// TestIssueListRunFlagRejectsAMalformedRef keeps a bad ref a validation error
// rather than a lookup for run 0.
func TestIssueListRunFlagRejectsAMalformedRef(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "some work", model.StatusTodo, model.PriorityHigh)

	w, _ := bufWriter(true)
	err := runIssueList(listCmdWithRun(conn, "not-a-run"), nil, w)
	if err == nil {
		t.Fatal("a malformed run ref listed something instead of failing")
	}
	assertErrCode(t, err, output.ErrValidation)
}

// TestIssueListRunAndProjectAreRefused: a run belongs to one project, so the
// two scopes cannot both be honored and silently picking one would be a lie.
func TestIssueListRunAndProjectAreRefused(t *testing.T) {
	conn := newTestDB(t)
	issue := createIssue(t, conn, "bound work", model.StatusTodo, model.PriorityHigh)
	run := planningRun(t, conn, issue)

	cmd := listCmdWithRun(conn, run.Ref())
	if err := cmd.Flags().Set("project", "somewhere.git"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}

	w, _ := bufWriter(true)
	err := runIssueList(cmd, nil, w)
	if err == nil {
		t.Fatal("--run and --project were both accepted")
	}
	assertErrCode(t, err, output.ErrValidation)
}

// TestIssueListRunFlagOnAnEmptyRoster answers the question the empty listing
// actually raises — about the RUN, not about the store.
func TestIssueListRunFlagOnAnEmptyRoster(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "unbound work", model.StatusTodo, model.PriorityHigh)
	run := planningRun(t, conn)

	w, buf := bufWriter(false)
	if err := runIssueList(listCmdWithRun(conn, run.Ref()), nil, w); err != nil {
		t.Fatalf("runIssueList --run: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, run.Ref()) {
		t.Errorf("the empty state does not name the run:\n%s", out)
	}
	if strings.Contains(out, "docket issue create") {
		t.Errorf("an empty ROSTER is answered with advice about an empty "+
			"STORE:\n%s", out)
	}
}

// assertErrCode is the exit-code half of a refusal: a listing that fails with
// the wrong code is one a script cannot tell apart from a different failure.
func assertErrCode(t *testing.T, err error, want output.ErrorCode) {
	t.Helper()
	ce, ok := err.(*CmdError)
	if !ok {
		t.Fatalf("error is %T, want *CmdError: %v", err, err)
	}
	if ce.Code != want {
		t.Errorf("error code = %v, want %v (%v)", ce.Code, want, err)
	}
}

// runIssueListThroughRoot drives `docket issue list <jsonArg>` through the
// real root command, so the --json flag's registration and parsing are what
// pick the envelope. Cobra keeps flag values across Execute calls, so every
// changed flag goes back to its default afterwards.
func runIssueListThroughRoot(t *testing.T, jsonArg string) (string, error) {
	t.Helper()
	rootCmd.SetArgs([]string{"issue", "list", jsonArg})
	defer func() {
		rootCmd.SetArgs(nil)
		reset := func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
		listCmd.Flags().Visit(reset)
		rootCmd.PersistentFlags().Visit(reset)
	}()
	stdout := captureStdout(t)
	err := rootCmd.Execute()
	return stdout(), err
}

// TestIssueListBareJSONSelectsV2 pins the default dialect: bare --json, and
// the Bool-era spellings --json=true and --json=1, all emit the v2 envelope.
func TestIssueListBareJSONSelectsV2(t *testing.T) {
	for _, arg := range []string{"--json", "--json=true", "--json=1"} {
		t.Run(arg, func(t *testing.T) {
			newVoteCreateStore(t)

			out, err := runIssueListThroughRoot(t, arg)
			testsupport.Must(t, err, "issue list %s: %v", arg, err)

			var env struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(out), &env); err != nil {
				t.Fatalf("unmarshal envelope: %v\n%s", err, out)
			}
			var items []json.RawMessage
			if err := json.Unmarshal(env.Data["items"], &items); err != nil || items == nil {
				t.Errorf(".data.items is not an array (err %v):\n%s", err, out)
			}
			if got := string(env.Data["total"]); got != "0" {
				t.Errorf(".data.total = %q, want 0:\n%s", got, out)
			}
		})
	}
}

// TestIssueListRejectsRetiredJSONDialect: the v1 dialect is gone, and the
// refusal points at the one dialect that remains. It runs the built binary
// because the refusal an operator sees, its rendering and its exit code, comes
// from the package Execute, which rootCmd.Execute alone does not run.
func TestIssueListRejectsRetiredJSONDialect(t *testing.T) {
	bin := buildDocketBinary(t)
	repo := t.TempDir()
	if _, stderr, code := runDocket(t, bin, repo, "init"); code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}

	stdout, stderr, code := runDocket(t, bin, repo, "issue", "list", "--json=v1")
	if code != 3 {
		t.Errorf("exit = %d, want 3 VALIDATION_ERROR\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing: a refused dialect lists nothing", stdout)
	}
	if !strings.Contains(stderr, "v2") {
		t.Errorf("refusal does not name v2: %q", stderr)
	}
	for _, dialect := range regexp.MustCompile(`v[0-9]+`).FindAllString(stderr, -1) {
		if dialect != "v2" {
			t.Errorf("refusal names dialect %q: %q", dialect, stderr)
		}
	}
}

// TestIssueListPrimaryKeyAlias is DKT-452's other half: every `issue list` row
// (`.data.items[]`) carries the id under `id` and `issue` alike. The RUN-37
// conductor that guessed wrong on `issue show` repeated the same guess across
// list rows, so a fix that reached only the detail view would have left the
// more-printed surface wrong.
func TestIssueListPrimaryKeyAlias(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssue(t, conn, "listed", model.StatusTodo, model.PriorityHigh)

	buf := &bytes.Buffer{}
	w := &output.Writer{
		JSONMode:    true,
		JSONVersion: output.JSONV2,
		Stdout:      buf,
		Stderr:      &bytes.Buffer{},
	}
	testsupport.Must(t, runIssueList(listCmdWithDB(conn), nil, w),
		"runIssueList: %v", nil)

	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, buf.String())
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(env.Data["items"], &rows); err != nil {
		t.Fatalf("unmarshal items: %v\n%s", err, buf.String())
	}
	if len(rows) != 1 {
		t.Fatalf("items = %d rows, want 1\n%s", len(rows), buf.String())
	}

	want := `"` + model.FormatID(issueID) + `"`
	for _, key := range []string{"id", "issue"} {
		got, ok := rows[0][key]
		if !ok {
			t.Fatalf("row has no %q key; keys = %v", key, keysOf(rows[0]))
		}
		if string(got) != want {
			t.Errorf("row.%s = %s, want %s", key, got, want)
		}
	}
}
