package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-414 at the CLI boundary. internal/engine tests WHICH rows a held
// resolution flags (held_stale_test.go, against real git history); what this
// file asserts is the RENDERING — that the advisory reaches the operator on
// the same two channels every stale_targets/pin_drift advisory uses: w.Warn
// lines on stderr in human mode, a `stale_targets` field beside the row in
// the JSON envelope — and that a resolution with nothing to warn about emits
// exactly the bytes it always has.

// divergenceAdvisory is a stale-target row as the engine would compute it —
// the fixture for the rendering assertions.
var divergenceAdvisory = []engine.StaleTarget{{
	Instance:   "verify@0",
	Issue:      "DKT-7",
	TargetSHA:  "3e8d3984d680aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	SharedHead: "6399743bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	Reason:     "the recorded target sha 3e8d3984d680 is not an ancestor of the shared checkout's HEAD 6399743bbbbb",
}}

// TestResolutionAdvisoryWarnsInHumanMode: the stderr line names the step, the
// recorded target, and the ancestry fact — the same sentence `dispatch open`
// prints for the same divergence.
func TestResolutionAdvisoryWarnsInHumanMode(t *testing.T) {
	conn := newTestDB(t)
	activatedRunForNext(t, conn)

	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	w := &output.Writer{Stdout: outBuf, Stderr: errBuf}

	err := emitStepStateAdvised(w, conn, 1, "Resolved", divergenceAdvisory)
	testsupport.Must(t, err, "emitStepStateAdvised: %v", err)

	if !strings.Contains(errBuf.String(), "verify@0") ||
		!strings.Contains(errBuf.String(), "not an ancestor of the shared") {
		t.Errorf("stderr does not carry the divergence warning:\n%s", errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "Resolved STEP-1") {
		t.Errorf("stdout lost the resolution message:\n%s", outBuf.String())
	}
}

// TestResolutionAdvisoryRidesTheJSONEnvelope: JSON mode suppresses w.Warn by
// design, so the envelope must carry the same rows — flat beside the step row
// it has always emitted, exactly as the manifest carries them.
func TestResolutionAdvisoryRidesTheJSONEnvelope(t *testing.T) {
	conn := newTestDB(t)
	activatedRunForNext(t, conn)

	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	w := &output.Writer{JSONMode: true, Stdout: outBuf, Stderr: errBuf}

	err := emitStepStateAdvised(w, conn, 1, "Resolved", divergenceAdvisory)
	testsupport.Must(t, err, "emitStepStateAdvised: %v", err)

	var envelope struct {
		Data struct {
			Step         string               `json:"step"`
			StaleTargets []engine.StaleTarget `json:"stale_targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(outBuf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, outBuf.String())
	}
	if envelope.Data.Step != "STEP-1" {
		t.Errorf("data.step = %q; the embedded row must still marshal flat",
			envelope.Data.Step)
	}
	if !reflectEqualStale(envelope.Data.StaleTargets, divergenceAdvisory) {
		t.Errorf("data.stale_targets = %+v, want the advisory verbatim",
			envelope.Data.StaleTargets)
	}
	if errBuf.Len() != 0 {
		t.Errorf("JSON mode wrote to stderr: %s", errBuf.String())
	}
}

func reflectEqualStale(got, want []engine.StaleTarget) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestApproveWithoutAdvisoryKeepsPlainPayload runs the FULL approve path on a
// declared human gate: the engine's advisory probe returns nothing for a
// non-materialized step, and the emitted envelope must carry no
// `stale_targets` key at all — absent, not empty, the same omission rule
// every advisory field here follows.
func TestApproveWithoutAdvisoryKeepsPlainPayload(t *testing.T) {
	conn := newTestDB(t)
	activatedRunForNext(t, conn)

	cmd := cmdWithDB(conn)
	outBuf := &bytes.Buffer{}
	w := &output.Writer{JSONMode: true, Stdout: outBuf, Stderr: &bytes.Buffer{}}

	// STEP-2 is the fixture's `type="human"` gate.
	err := runDecide(cmd, []string{"STEP-2"}, true, w)
	testsupport.Must(t, err, "runDecide: %v", err)

	if strings.Contains(outBuf.String(), "stale_targets") {
		t.Errorf("an ordinary approval grew a stale_targets key:\n%s", outBuf.String())
	}
	var envelope struct {
		Data struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(outBuf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, outBuf.String())
	}
	if envelope.Data.Step != "STEP-2" || envelope.Data.Status != "done" {
		t.Errorf("approval payload = %+v, want STEP-2 done", envelope.Data)
	}
}
