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
quick = { model = "haiku", effort = "low" }

[executors]
worker = { variant = "guarded" }
seat-a = { variant = "tier-a" }
seat-b = { variant = "guarded" }

[sizes]
small = "quick"

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

// policyResolveCmdWithDB builds the flag set runPolicyResolve reads. An empty
// size leaves --size at the real command's registered default, so a test
// that omits it exercises what an omitted --size resolves to.
func policyResolveCmdWithDB(conn *sql.DB, runRef, size string, labels []string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("run", runRef, "")
	cmd.Flags().String("size", policyResolveCmd.Flags().Lookup("size").DefValue, "")
	if size != "" {
		_ = cmd.Flags().Set("size", size)
	}
	cmd.Flags().StringSliceP("label", "l", labels, "")
	return cmd
}

type policySeatsJSON struct {
	Data struct {
		Items []model.VoterAssignment `json:"items"`
		Total int                     `json:"total"`
	} `json:"data"`
}

// TestPolicyResolveJSON is AC1: one call, one entry per seat, in request
// order, carrying the seat's model, effort and variant. With --size omitted
// each seat stands on its own variant even though the policy maps
// [sizes].small.
func TestPolicyResolveJSON(t *testing.T) {
	conn, runID := policySeatRun(t, policySeatTOML)

	cmd := policyResolveCmdWithDB(conn, model.FormatRunID(runID), "", nil)
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
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
	if len(got.Data.Items) != len(want) {
		t.Fatalf("seats = %+v, want %+v", got.Data.Items, want)
	}
	for i := range want {
		if got.Data.Items[i] != want[i] {
			t.Errorf("seat %d = %+v, want %+v", i, got.Data.Items[i], want[i])
		}
	}
}

// TestPolicyResolveLabelReachesTheSecurityCheck is AC2 at the CLI boundary:
// --label is what makes a seat sensitive for a gate that has no step row to
// snapshot labels from, and seat-a is then clamped to [security].ceiling.
func TestPolicyResolveLabelReachesTheSecurityCheck(t *testing.T) {
	conn, runID := policySeatRun(t, policySeatTOML)

	cmd := policyResolveCmdWithDB(conn, model.FormatRunID(runID), "", []string{"sensitive"})
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err := runPolicyResolve(cmd, []string{"seat-a"}, w)
	testsupport.Must(t, err, "runPolicyResolve: %v", err)

	var got policySeatsJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(got.Data.Items) != 1 {
		t.Fatalf("seats = %+v, want one", got.Data.Items)
	}
	if got.Data.Items[0].Variant != "guarded" || got.Data.Items[0].Model != "sonnet" {
		t.Errorf("seat-a = %+v, want the ceiling sonnet/medium/guarded", got.Data.Items[0])
	}
}

// TestPolicyResolveSizeReachesTheSizesTable: --size stands in for the frozen
// Size a vote step row snapshots, so --size small seats the [sizes].small
// variant.
func TestPolicyResolveSizeReachesTheSizesTable(t *testing.T) {
	conn, runID := policySeatRun(t, policySeatTOML)

	cmd := policyResolveCmdWithDB(conn, model.FormatRunID(runID), "small", nil)
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err := runPolicyResolve(cmd, []string{"seat-b"}, w)
	testsupport.Must(t, err, "runPolicyResolve: %v", err)

	var got policySeatsJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	want := model.VoterAssignment{Voter: "seat-b", Model: "haiku", Effort: "low", Variant: "quick"}
	if len(got.Data.Items) != 1 || got.Data.Items[0] != want {
		t.Errorf("seats = %+v, want [sizes].small's %+v", got.Data.Items, want)
	}
}

// TestPolicyResolveAcceptsEverySize: every Size a vote row can carry is a
// valid --size, including unknown and the sizes this policy leaves unmapped,
// which resolve to the seat's own variant as such a row does.
func TestPolicyResolveAcceptsEverySize(t *testing.T) {
	conn, runID := policySeatRun(t, policySeatTOML)

	for _, size := range []string{"trivial", "small", "bounded", "needs-design", "unknown"} {
		t.Run(size, func(t *testing.T) {
			cmd := policyResolveCmdWithDB(conn, model.FormatRunID(runID), size, nil)
			w, buf := bufWriter(true)
			w.JSONVersion = output.JSONV2
			err := runPolicyResolve(cmd, []string{"seat-b"}, w)
			testsupport.Must(t, err, "runPolicyResolve --size %s: %v", size, err)

			var got policySeatsJSON
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, buf.String())
			}
			want := model.VoterAssignment{Voter: "seat-b", Model: "sonnet", Effort: "medium", Variant: "guarded"}
			if size == "small" {
				want = model.VoterAssignment{Voter: "seat-b", Model: "haiku", Effort: "low", Variant: "quick"}
			}
			if len(got.Data.Items) != 1 || got.Data.Items[0] != want {
				t.Errorf("seats = %+v, want %+v", got.Data.Items, want)
			}
		})
	}
}

// TestPolicyResolveRegistersSize:--size is a flag on the real command, with
// no default, so an omitted --size resolves as an unsized row does.
func TestPolicyResolveRegistersSize(t *testing.T) {
	flag := policyResolveCmd.Flags().Lookup("size")
	if flag == nil {
		t.Fatal("policy resolve registers no --size flag")
	}
	if flag.DefValue != "" {
		t.Errorf("--size default = %q, want empty", flag.DefValue)
	}
}

// TestPolicyResolveRefusals: each way the lookup cannot be answered reports
// itself, with the taxonomy code a caller branches on.
func TestPolicyResolveRefusals(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		runRef  func(runID int) string
		size    string
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
		{
			// A --size no vote row can carry is refused, not silently
			// resolved as the unsized roster.
			name:    "a misspelled --size",
			policy:  policySeatTOML,
			runRef:  func(runID int) string { return model.FormatRunID(runID) },
			size:    "smal",
			seats:   []string{"seat-a"},
			want:    output.ErrValidation,
			message: `"smal"`,
		},
		{
			name:    "a --size outside the vocabulary's case",
			policy:  policySeatTOML,
			runRef:  func(runID int) string { return model.FormatRunID(runID) },
			size:    "Small",
			seats:   []string{"seat-a"},
			want:    output.ErrValidation,
			message: `"Small"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, runID := policySeatRun(t, tt.policy)
			cmd := policyResolveCmdWithDB(conn, tt.runRef(runID), tt.size, nil)
			w, buf := bufWriter(true)

			err := runPolicyResolve(cmd, tt.seats, w)
			if err == nil {
				t.Fatalf("want a refusal, got output %s", buf.String())
			}
			if buf.Len() != 0 {
				t.Errorf("a refusal wrote output %s, want no seats", buf.String())
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
