package verb_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

const policySeatTOML = `
[policy]
version = 2

[variants]
guarded = { model = "sonnet", effort = "medium", escalate_to = "tier-a" }
tier-a = { model = "opus", effort = "high" }
quick = { model = "haiku", effort = "low" }

[executors]
someone = { variant = "guarded" }
seat-a = { variant = "tier-a" }
seat-b = { variant = "guarded" }

[sizes]
small = "quick"

[security]
ceiling = "guarded"
labels = ["sensitive"]
`

// policyRun activates a one-step run in the default project with policyToml
// in the instance config root, so activation pins it; an empty policyToml
// leaves the run with no pinned policy.
func policyRun(t *testing.T, st store.Store, policyToml string) int {
	t.Helper()
	docketDir := filepath.Join(t.TempDir(), ".docket")
	configDir := filepath.Join(docketDir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("creating the config directory: %v", err)
	}
	t.Setenv("DOCKET_PATH", docketDir)
	if policyToml != "" {
		if err := os.WriteFile(filepath.Join(configDir, "policy.toml"), []byte(policyToml), 0o644); err != nil {
			t.Fatalf("writing policy.toml: %v", err)
		}
	}

	def, err := workflow.Load([]byte(stepWorkflow))
	if err != nil {
		t.Fatalf("workflow.Load: %v", err)
	}
	parsed, err := workflow.Canonical(def)
	if err != nil {
		t.Fatalf("workflow.Canonical: %v", err)
	}
	if _, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: store.DefaultProjectID,
		Name:      def.Pipeline.Name, Version: def.Pipeline.Version,
		SourceSHA256: workflow.SHA256([]byte(stepWorkflow)),
		Body:         stepWorkflow, Parsed: string(parsed),
	}, model.NowMS()); err != nil {
		t.Fatalf("InsertWorkflow: %v", err)
	}

	run, _, err := st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{}, "", createIssue(t, st))
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	if _, err := engine.ActivateStore(st, run.ID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}
	return run.ID
}

func TestPolicyResolveResolvesEachSeatInRequestOrder(t *testing.T) {
	st := openStore(t)
	run := model.FormatRunID(policyRun(t, st, policySeatTOML))

	tests := []struct {
		name string
		in   verb.PolicyResolveInput
		want []model.VoterAssignment
	}{
		{
			name: "each seat on its own variant",
			in:   verb.PolicyResolveInput{Run: run, Seats: []string{"seat-a", "seat-b"}},
			want: []model.VoterAssignment{
				{Voter: "seat-a", Model: "opus", Effort: "high", Variant: "tier-a"},
				{Voter: "seat-b", Model: "sonnet", Effort: "medium", Variant: "guarded"},
			},
		},
		{
			name: "a sensitive label clamps to the ceiling",
			in:   verb.PolicyResolveInput{Run: run, Seats: []string{"seat-a"}, Labels: []string{"sensitive"}},
			want: []model.VoterAssignment{
				{Voter: "seat-a", Model: "sonnet", Effort: "medium", Variant: "guarded"},
			},
		},
		{
			name: "a mapped size sets the starting variant",
			in:   verb.PolicyResolveInput{Run: run, Seats: []string{"seat-b"}, Size: "small"},
			want: []model.VoterAssignment{
				{Voter: "seat-b", Model: "haiku", Effort: "low", Variant: "quick"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := verb.PolicyResolve(context.Background(), projectEnv(st, "", false), tt.in)
			if err != nil {
				t.Fatalf("PolicyResolve: %v", err)
			}
			if !reflect.DeepEqual(got.Seats, tt.want) {
				t.Errorf("seats = %+v, want %+v", got.Seats, tt.want)
			}
		})
	}
}

// PolicyResolve is a read: an anchored identity no project holds still
// resolves the run's seats, and is not registered.
func TestPolicyResolveRegistersNoProject(t *testing.T) {
	st := openStore(t)
	run := model.FormatRunID(policyRun(t, st, policySeatTOML))

	_, err := verb.PolicyResolve(context.Background(), projectEnv(st, "/work/elsewhere", true),
		verb.PolicyResolveInput{Run: run, Seats: []string{"seat-a"}})
	if err != nil {
		t.Fatalf("PolicyResolve: %v", err)
	}
	requireUnregistered(t, st, "/work/elsewhere")
}

func TestPolicyResolveRefusals(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		run     func(runID int) string
		size    string
		seats   []string
		want    output.ErrorCode
		message string
	}{
		{"no run", policySeatTOML, func(int) string { return "" }, "", []string{"seat-a"}, output.ErrValidation, "--run is required"},
		{"a malformed run", policySeatTOML, func(int) string { return "nonsense" }, "", []string{"seat-a"}, output.ErrValidation, "invalid run ID"},
		{"a misspelled size", policySeatTOML, model.FormatRunID, "smal", []string{"seat-a"}, output.ErrValidation, `"smal"`},
		{"a run that does not exist", policySeatTOML, func(int) string { return model.FormatRunID(9999) }, "", []string{"seat-a"}, output.ErrNotFound, "not found"},
		{"a run with no pinned policy.toml", "", model.FormatRunID, "", []string{"seat-a"}, output.ErrNotFound, "no pinned policy.toml"},
		{"an unknown seat", policySeatTOML, model.FormatRunID, "", []string{"seat-a", "no-such-seat"}, output.ErrNotFound, "no-such-seat"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			runID := policyRun(t, st, tt.policy)

			got, err := verb.PolicyResolve(context.Background(), projectEnv(st, "", false),
				verb.PolicyResolveInput{Run: tt.run(runID), Seats: tt.seats, Size: tt.size})
			requireCode(t, err, tt.want)
			if !strings.Contains(err.Error(), tt.message) {
				t.Errorf("error %q does not say %q", err, tt.message)
			}
			if got.Seats != nil {
				t.Errorf("seats = %+v, want none on a refusal", got.Seats)
			}
		})
	}
}
