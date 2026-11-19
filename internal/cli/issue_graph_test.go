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

// setGraphFlag sets one of graphCmd's flags for the rest of t.
func setGraphFlag(t *testing.T, name, value string) {
	t.Helper()
	testsupport.Must(t, graphCmd.Flags().Set(name, value), "set --%s", name)
	t.Cleanup(func() {
		f := graphCmd.Flags().Lookup(name)
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
}

// runIssueGraphHuman runs the graph command in human mode with the given
// flags, which stay set for the rest of t, and returns what it printed.
func runIssueGraphHuman(t *testing.T, conn *sql.DB, flags map[string]string, args ...string) string {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	for name, value := range flags {
		setGraphFlag(t, name, value)
	}
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

	for _, tc := range []struct {
		name  string
		flags map[string]string
		want  string
	}{
		{"tree", nil, "* " + f + " [todo] focal\n" +
			"  Blocked by\n" +
			"    " + b + " [todo] blocker\n" +
			"  Blocks\n" +
			"    " + d + " [backlog] blocked\n\n"},
		{"down tree", map[string]string{"direction": "down"}, "* " + f + " [todo] focal\n" +
			"  Blocks\n" +
			"    " + d + " [backlog] blocked\n\n"},
		{"mermaid", map[string]string{"mermaid": "true"}, "graph TD\n" +
			"    " + f + `["` + f + `: focal"] --> ` + d + `["` + d + `: blocked"]` + "\n" +
			"    " + b + `["` + b + `: blocker"] --> ` + f + `["` + f + `: focal"]` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runIssueGraphHuman(t, conn, tc.flags, f); got != tc.want {
				t.Errorf("%s =\n%q\nwant\n%q", tc.name, got, tc.want)
			}
		})
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
		t.Run(tc.msg, func(t *testing.T) {
			if tc.flag != "" {
				setGraphFlag(t, tc.flag, tc.value)
			}
			_, err := runLeaseCommandOutput(t, conn, nil, graphCmd, tc.issue)
			var cmdError *CmdError
			if !errors.As(err, &cmdError) || cmdError.Code != tc.code || cmdError.Error() != tc.msg {
				t.Errorf("issue graph %s --%s=%s returned %v, want %s %q", tc.issue, tc.flag, tc.value, err, tc.code, tc.msg)
			}
		})
	}
}
