package verb_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

const pinWorkflow = `
[pipeline]
name = "pin-show"
version = 1

[match]
kind = ["task"]

[[step]]
name = "judge"
executor = "judge"
emits = "change-summary"
after = []
packet = ["contracts/judge.md"]
`

const pinPolicy = "opaque = \"instance policy\"\n"

// activatedPinRun activates a run over a config tree holding policy.toml and
// returns the run's ref.
func activatedPinRun(t *testing.T, st store.Store) string {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, ".docket", "config")
	for path, body := range map[string]string{
		"workflows/pin-show.toml": pinWorkflow,
		"contracts/judge.md":      "the judge contract\n",
		"policy.toml":             pinPolicy,
	} {
		full := filepath.Join(configDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}
	t.Setenv("DOCKET_PATH", filepath.Join(root, ".docket"))

	issue := createIssue(t, st)
	run, _, err := st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{}, "", issue)
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	if _, err := engine.ActivateStore(st, run.ID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}
	return run.Ref()
}

func TestPinShowReturnsThePinnedBytes(t *testing.T) {
	st := openStore(t)
	runRef := activatedPinRun(t, st)

	res, err := verb.PinShow(context.Background(), verb.Env{Store: st},
		verb.PinShowInput{Run: runRef, Path: "policy.toml"})
	if err != nil {
		t.Fatalf("PinShow: %v", err)
	}
	want := verb.PinShowResult{Run: runRef, Path: "policy.toml", Body: pinPolicy}
	if res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
}

func TestPinShowRefusals(t *testing.T) {
	st := openStore(t)
	runRef := activatedPinRun(t, st)

	cases := []struct {
		name string
		in   verb.PinShowInput
		code output.ErrorCode
	}{
		{"a malformed run ref", verb.PinShowInput{Run: "not-a-run", Path: "policy.toml"}, output.ErrValidation},
		{"a run that does not exist", verb.PinShowInput{Run: model.FormatRunID(9999), Path: "policy.toml"}, output.ErrNotFound},
		{"a path the run never pinned", verb.PinShowInput{Run: runRef, Path: "unpinned.toml"}, output.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verb.PinShow(context.Background(), verb.Env{Store: st}, tc.in)
			requireCode(t, err, tc.code)
		})
	}
}

// PinShow is a read: an anchored identity no project holds is not registered.
func TestPinShowNeverRegistersAProject(t *testing.T) {
	st := openStore(t)
	runRef := activatedPinRun(t, st)
	const identity = "/work/unregistered-repo"

	if _, err := verb.PinShow(context.Background(), projectEnv(st, identity, true),
		verb.PinShowInput{Run: runRef, Path: "policy.toml"}); err != nil {
		t.Fatalf("PinShow: %v", err)
	}
	requireUnregistered(t, st, identity)
}
