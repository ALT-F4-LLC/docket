package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// `docket policy resolve` — the seat lookup a conversational gate makes,
// since it has no step row for routing to ride on.

const policySeatWorkflow = `
[pipeline]
name = "seat-fixture"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "worker"
emits = "out"
`

const policySeatTOML = `
[policy]
version = 2

[variants]
guarded = { model = "sonnet", effort = "medium", escalate_to = "tier-a" }
tier-a = { model = "opus", effort = "high" }

[executors]
worker = { variant = "guarded" }
seat-a = { variant = "tier-a" }
seat-b = { variant = "guarded" }

[security]
ceiling = "guarded"
labels = ["sensitive"]
`

// policySeatRun activates a run against a repo-local config root, pinning
// policyToml when non-empty (the dormancy case passes "").
//
// THE TEMP DIR COMES FIRST, BEFORE t.Setenv, for the reason engine's
// configRepo records: t.TempDir reads TMPDIR.
func policySeatRun(t *testing.T, policyToml string) (*sql.DB, int) {
	t.Helper()

	root := t.TempDir()
	docketDir := filepath.Join(root, ".docket")
	configDir := filepath.Join(docketDir, "config")
	err := os.MkdirAll(configDir, 0o755)
	testsupport.Must(t, err, "creating the config directory: %v", err)
	t.Setenv("DOCKET_PATH", docketDir)

	conn, err := db.Open(filepath.Join(docketDir, "issues.db"))
	testsupport.Must(t, err, "opening: %v", err)
	t.Cleanup(func() { conn.Close() })
	testsupport.Must(t, db.Initialize(conn), "Initialize: %v", err)
	testsupport.Must(t, db.Migrate(conn), "Migrate: %v", err)

	writePolicyFile(t, configDir, "workflows/seat-fixture.toml", policySeatWorkflow)
	if policyToml != "" {
		writePolicyFile(t, configDir, "policy.toml", policyToml)
	}

	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "seat fixture", Status: model.StatusBacklog,
		Priority: model.PriorityNone, Kind: model.IssueKind("task"),
	}, nil, nil)
	testsupport.Must(t, err, "creating the issue: %v", err)

	now := model.NowMS()
	run, err := db.InsertRun(conn, db.DefaultProjectID, "seat run", 0, now)
	testsupport.Must(t, err, "starting the run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "binding the issue: %v", err)

	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: now})
	testsupport.Must(t, err, "activate: %v", err)
	return conn, run.ID
}

func writePolicyFile(t *testing.T, configDir, rel, body string) {
	t.Helper()
	path := filepath.Join(configDir, rel)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	testsupport.Must(t, err, "creating %s: %v", filepath.Dir(path), err)
	err = os.WriteFile(path, []byte(body), 0o644)
	testsupport.Must(t, err, "writing %s: %v", path, err)
}

func policyResolveCmdWithDB(conn *sql.DB, runRef string, labels []string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("run", runRef, "")
	cmd.Flags().StringSliceP("label", "l", labels, "")
	return cmd
}

type policySeatsJSON struct {
	Data struct {
		Seats []model.VoterAssignment `json:"seats"`
		Total int                     `json:"total"`
	} `json:"data"`
}

// TestPolicyResolveJSON is AC1: one call, one entry per seat, in request
// order, carrying the seat's model, effort and variant.
func TestPolicyResolveJSON(t *testing.T) {
	conn, runID := policySeatRun(t, policySeatTOML)

	cmd := policyResolveCmdWithDB(conn, model.FormatRunID(runID), nil)
	w, buf := bufWriter(true)
	err := runPolicyResolve(cmd, []string{"seat-a", "seat-b"}, w)
	testsupport.Must(t, err, "runPolicyResolve: %v", err)

	var got policySeatsJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	want := []model.VoterAssignment{
		{Voter: "seat-a", Model: "opus", Effort: "high", Variant: "tier-a"},
		{Voter: "seat-b", Model: "sonnet", Effort: "medium", Variant: "guarded"},
	}
	if got.Data.Total != len(want) {
		t.Errorf("total = %d, want %d", got.Data.Total, len(want))
	}
	if len(got.Data.Seats) != len(want) {
		t.Fatalf("seats = %+v, want %+v", got.Data.Seats, want)
	}
	for i := range want {
		if got.Data.Seats[i] != want[i] {
			t.Errorf("seat %d = %+v, want %+v", i, got.Data.Seats[i], want[i])
		}
	}
}

// TestPolicyResolveLabelReachesTheSecurityCheck is AC2 at the CLI boundary:
// --label is what makes a seat sensitive for a gate that has no step row to
// snapshot labels from, and seat-a is then clamped to [security].ceiling.
func TestPolicyResolveLabelReachesTheSecurityCheck(t *testing.T) {
	conn, runID := policySeatRun(t, policySeatTOML)

	cmd := policyResolveCmdWithDB(conn, model.FormatRunID(runID), []string{"sensitive"})
	w, buf := bufWriter(true)
	err := runPolicyResolve(cmd, []string{"seat-a"}, w)
	testsupport.Must(t, err, "runPolicyResolve: %v", err)

	var got policySeatsJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(got.Data.Seats) != 1 {
		t.Fatalf("seats = %+v, want one", got.Data.Seats)
	}
	if got.Data.Seats[0].Variant != "guarded" || got.Data.Seats[0].Model != "sonnet" {
		t.Errorf("seat-a = %+v, want the ceiling sonnet/medium/guarded", got.Data.Seats[0])
	}
}

// TestPolicyResolveRefusals: each way the lookup cannot be answered reports
// itself, with the taxonomy code a caller branches on.
func TestPolicyResolveRefusals(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		runRef  func(runID int) string
		seats   []string
		want    output.ErrorCode
		message string
	}{
		{
			name:    "no --run",
			policy:  policySeatTOML,
			runRef:  func(int) string { return "" },
			seats:   []string{"seat-a"},
			want:    output.ErrValidation,
			message: "--run is required",
		},
		{
			name:    "a malformed --run",
			policy:  policySeatTOML,
			runRef:  func(int) string { return "nonsense" },
			seats:   []string{"seat-a"},
			want:    output.ErrValidation,
			message: "invalid run ID",
		},
		{
			name:    "a run that does not exist",
			policy:  policySeatTOML,
			runRef:  func(int) string { return model.FormatRunID(9999) },
			seats:   []string{"seat-a"},
			want:    output.ErrNotFound,
			message: "not found",
		},
		{
			name:    "a run with no pinned policy.toml",
			policy:  "",
			runRef:  func(runID int) string { return model.FormatRunID(runID) },
			seats:   []string{"seat-a"},
			want:    output.ErrNotFound,
			message: "no pinned policy.toml",
		},
		{
			name:    "an unknown seat",
			policy:  policySeatTOML,
			runRef:  func(runID int) string { return model.FormatRunID(runID) },
			seats:   []string{"seat-a", "no-such-seat"},
			want:    output.ErrNotFound,
			message: "no-such-seat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, runID := policySeatRun(t, tt.policy)
			cmd := policyResolveCmdWithDB(conn, tt.runRef(runID), nil)
			w, buf := bufWriter(true)

			err := runPolicyResolve(cmd, tt.seats, w)
			if err == nil {
				t.Fatalf("want a refusal, got output %s", buf.String())
			}
			var ce *CmdError
			if !errors.As(err, &ce) {
				t.Fatalf("error = %T (%v), want *CmdError", err, err)
			}
			if ce.Code != tt.want {
				t.Errorf("code = %q, want %q (%v)", ce.Code, tt.want, err)
			}
			if !strings.Contains(err.Error(), tt.message) {
				t.Errorf("error %q does not say %q", err, tt.message)
			}
		})
	}
}

// TestPolicyResolveRequiresASeat: the seats are the verb's subject, so an
// empty list is refused by cobra rather than answered with nothing.
func TestPolicyResolveRequiresASeat(t *testing.T) {
	if err := policyResolveCmd.Args(policyResolveCmd, nil); err == nil {
		t.Error("want a refusal when no seat is named")
	}
}
