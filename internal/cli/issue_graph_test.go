package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestIssueGraphOwnsItsProject: issue graph resolves its project through
// internal/verb, so the root hook must skip its eager resolution.
func TestIssueGraphOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(graphCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", graphCmd.CommandPath())
	}
}

// graphChain seeds blocker -> focal -> blocked and returns the three ids.
func graphChain(t *testing.T) (conn *sql.DB, blocker, focal, blocked int) {
	t.Helper()
	conn = newTestDB(t)
	blocker = createIssue(t, conn, "blocker", model.StatusTodo, model.PriorityNone)
	focal = createIssue(t, conn, "focal", model.StatusTodo, model.PriorityNone)
	blocked = createIssue(t, conn, "blocked", model.StatusBacklog, model.PriorityNone)
	for _, rel := range [][2]int{{blocker, focal}, {focal, blocked}} {
		_, err := db.CreateRelation(conn, &model.Relation{
			SourceIssueID: rel[0], TargetIssueID: rel[1], RelationType: model.RelationBlocks,
		})
		testsupport.Must(t, err, "CreateRelation(%v): %v", rel, err)
	}
	return conn, blocker, focal, blocked
}

// runIssueGraphHuman runs the graph command in human mode with the given
// flags and returns what it printed.
func runIssueGraphHuman(t *testing.T, conn *sql.DB, flags map[string]string, args ...string) string {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	for name, value := range flags {
		testsupport.Must(t, graphCmd.Flags().Set(name, value), "set --%s", name)
	}
	defer func() {
		for name := range flags {
			f := graphCmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}()
	graphCmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	var out bytes.Buffer
	err := runIssueGraph(graphCmd, args, &output.Writer{Stdout: &out, Stderr: &out})
	testsupport.Must(t, err, "issue graph %v: %v", args, err)
	return out.String()
}

// TestIssueGraphJSONCarriesTheNodesAndEdges: the JSON answer names the focal
// issue, every issue the walk reached, and each edge blocker-to-blocked, down
// before up.
func TestIssueGraphJSONCarriesTheNodesAndEdges(t *testing.T) {
	conn, blocker, focal, blocked := graphChain(t)

	out, err := runLeaseCommandOutput(t, conn, nil, graphCmd, model.FormatID(focal))
	testsupport.Must(t, err, "issue graph: %v", err)
	var env struct {
		Data graphResult `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &env), "decoding %q", out)

	if env.Data.IssueID != focal {
		t.Errorf("issue_id = %d, want %d", env.Data.IssueID, focal)
	}
	var ids []string
	for _, n := range env.Data.Nodes {
		ids = append(ids, n.ID)
	}
	slices.Sort(ids)
	want := []string{model.FormatID(blocker), model.FormatID(focal), model.FormatID(blocked)}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("nodes = %v, want %v", ids, want)
	}
	wantEdges := []graphEdge{
		{From: focal, To: blocked, Type: "blocks"},
		{From: blocker, To: focal, Type: "blocks"},
	}
	if !slices.Equal(env.Data.Edges, wantEdges) {
		t.Errorf("edges = %+v, want %+v", env.Data.Edges, wantEdges)
	}
}

// TestIssueGraphRendersThePlainTreeAndMermaid pins the human renderings.
func TestIssueGraphRendersThePlainTreeAndMermaid(t *testing.T) {
	conn, blocker, focal, blocked := graphChain(t)
	b, f, d := model.FormatID(blocker), model.FormatID(focal), model.FormatID(blocked)

	tree := runIssueGraphHuman(t, conn, nil, f)
	wantTree := "* " + f + " [todo] focal\n" +
		"  Blocked by\n" +
		"    " + b + " [todo] blocker\n" +
		"  Blocks\n" +
		"    " + d + " [backlog] blocked\n\n"
	if tree != wantTree {
		t.Errorf("tree =\n%q\nwant\n%q", tree, wantTree)
	}

	down := runIssueGraphHuman(t, conn, map[string]string{"direction": "down"}, f)
	wantDown := "* " + f + " [todo] focal\n" +
		"  Blocks\n" +
		"    " + d + " [backlog] blocked\n\n"
	if down != wantDown {
		t.Errorf("down tree =\n%q\nwant\n%q", down, wantDown)
	}

	mermaid := runIssueGraphHuman(t, conn, map[string]string{"mermaid": "true"}, f)
	wantMermaid := "graph TD\n" +
		"    " + f + `["` + f + `: focal"] --> ` + d + `["` + d + `: blocked"]` + "\n" +
		"    " + b + `["` + b + `: blocker"] --> ` + f + `["` + f + `: focal"]` + "\n\n"
	if mermaid != wantMermaid {
		t.Errorf("mermaid =\n%q\nwant\n%q", mermaid, wantMermaid)
	}
}

// TestIssueGraphRefusals keeps each refusal's code and text.
func TestIssueGraphRefusals(t *testing.T) {
	conn, _, focal, _ := graphChain(t)

	for _, tc := range []struct {
		flag, value, issue string
		code               output.ErrorCode
		msg                string
	}{
		{"", "", "DKT-9999", output.ErrNotFound, "issue DKT-9999 not found"},
		{"direction", "sideways", model.FormatID(focal), output.ErrValidation,
			`invalid direction "sideways": must be one of [up, down, both]`},
		{"depth", "-1", model.FormatID(focal), output.ErrValidation, "depth must be non-negative"},
	} {
		if tc.flag != "" {
			testsupport.Must(t, graphCmd.Flags().Set(tc.flag, tc.value), "set --%s", tc.flag)
		}
		_, err := runLeaseCommandOutput(t, conn, nil, graphCmd, tc.issue)
		var cmdError *CmdError
		if !errors.As(err, &cmdError) || cmdError.Code != tc.code || cmdError.Error() != tc.msg {
			t.Errorf("issue graph %s --%s=%s returned %v, want %s %q", tc.issue, tc.flag, tc.value, err, tc.code, tc.msg)
		}
		if tc.flag != "" {
			f := graphCmd.Flags().Lookup(tc.flag)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}
}
