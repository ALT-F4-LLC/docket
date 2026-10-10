package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestWriteJSONSuccess(t *testing.T) {
	var buf bytes.Buffer
	writeJSONSuccess(&buf, map[string]string{"key": "val"}, "it worked")

	var env successEnvelope
	err := json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if !env.OK {
		t.Error("ok = false, want true")
	}
	if env.Message != "it worked" {
		t.Errorf("message = %q, want %q", env.Message, "it worked")
	}
	data, ok := env.Data.(map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want map", env.Data)
	}
	if data["key"] != "val" {
		t.Errorf("data.key = %v, want %q", data["key"], "val")
	}
}

func TestWriteJSONSuccessOmitsEmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	writeJSONSuccess(&buf, "data", "")

	var raw map[string]any
	err := json.Unmarshal(buf.Bytes(), &raw)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if _, exists := raw["message"]; exists {
		t.Error("expected message to be omitted when empty")
	}
}

func TestWriteJSONError(t *testing.T) {
	var buf bytes.Buffer
	writeJSONError(&buf, errors.New("something broke"), ErrNotFound)

	var env errorEnvelope
	err := json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if env.OK {
		t.Error("ok = true, want false")
	}
	if env.Error != "something broke" {
		t.Errorf("error = %q, want %q", env.Error, "something broke")
	}
	if env.Code != ErrNotFound {
		t.Errorf("code = %q, want %q", env.Code, ErrNotFound)
	}
}

func TestWriterErrorJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, Stdout: &stdout, Stderr: &stderr}

	code := w.Error(errors.New("fail"), ErrValidation)
	if code != ExitValidation {
		t.Errorf("exit code = %d, want %d", code, ExitValidation)
	}
	if stdout.Len() == 0 {
		t.Error("expected JSON error on stdout")
	}
	var env errorEnvelope
	err := json.Unmarshal(stdout.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if env.OK {
		t.Error("ok = true, want false")
	}
	if env.Code != ErrValidation {
		t.Errorf("code = %q, want %q", env.Code, ErrValidation)
	}
}

func TestWriterErrorHuman(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: false, Stdout: &stdout, Stderr: &stderr}

	code := w.Error(errors.New("fail"), ErrGeneral)
	if code != ExitGeneral {
		t.Errorf("exit code = %d, want %d", code, ExitGeneral)
	}
	if stderr.String() != "Error: fail\n" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "Error: fail\n")
	}
}

func TestWriterInfoSuppressedInJSONMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, Stdout: &stdout, Stderr: &stderr}

	w.Info("should not appear")
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output in JSON mode, got %q", stderr.String())
	}
}

func TestWriterInfoSuppressedInQuietMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := &Writer{QuietMode: true, Stdout: &stdout, Stderr: &stderr}

	w.Info("should not appear")
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output in quiet mode, got %q", stderr.String())
	}
}

func TestWriterInfoEmitsInDefaultMode(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	w := &Writer{Stdout: &stdout, Stderr: &stderr}

	w.Info("hello %s", "world")
	if stderr.String() != "hello world\n" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "hello world\n")
	}
}

func TestExitCodeForErrorMapping(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want int
	}{
		{ErrGeneral, ExitGeneral},
		{ErrNotFound, ExitNotFound},
		{ErrValidation, ExitValidation},
		{ErrConflict, ExitConflict},
		{ErrorCode("unknown"), ExitGeneral},
	}

	for _, tt := range tests {
		if got := ExitCodeForError(tt.code); got != tt.want {
			t.Errorf("ExitCodeForError(%q) = %d, want %d", tt.code, got, tt.want)
		}
	}
}

func TestWriteHumanSuccessPlainNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var buf bytes.Buffer
	writeHumanSuccess(&buf, "Created issue DKT-1")

	got := buf.String()
	want := "Created issue DKT-1\n"
	if got != want {
		t.Errorf("writeHumanSuccess(NO_COLOR) = %q, want %q", got, want)
	}
	// Should NOT contain the checkmark icon when colors are disabled
	if bytes.Contains(buf.Bytes(), []byte("\u2714")) {
		t.Error("expected no checkmark icon when NO_COLOR is set")
	}
}

func TestWriteHumanSuccessMultiLineNoCheckmark(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var buf bytes.Buffer
	table := "┌────┬───────┐\n│ ID │ Title │\n└────┴───────┘"
	writeHumanSuccess(&buf, table)

	got := buf.String()
	want := table + "\n"
	if got != want {
		t.Errorf("writeHumanSuccess(multi-line) = %q, want %q", got, want)
	}
	// Multi-line output must NOT contain the checkmark icon
	if bytes.Contains(buf.Bytes(), []byte("\u2714")) {
		t.Error("expected no checkmark icon for multi-line output")
	}
}

func TestWriteHumanSuccessMultiLineWithColorsNoCheckmark(t *testing.T) {
	// Even when colors are enabled, multi-line content should not get a checkmark
	t.Setenv("TERM", "xterm-256color")
	// Unset NO_COLOR to ensure colors would be enabled
	t.Setenv("NO_COLOR", "")
	// Note: render.ColorsEnabled() may still return false in test environments,
	// but the newline check comes first regardless — this test verifies the
	// multi-line branch is taken before the color check.

	var buf bytes.Buffer
	table := "line1\nline2\nline3"
	writeHumanSuccess(&buf, table)

	got := buf.String()
	want := table + "\n"
	if got != want {
		t.Errorf("writeHumanSuccess(multi-line, colors) = %q, want %q", got, want)
	}
	if bytes.Contains(buf.Bytes(), []byte("\u2714")) {
		t.Error("expected no checkmark icon for multi-line output even with colors")
	}
}

func TestWriteHumanSuccessEmptyMessage(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var buf bytes.Buffer
	writeHumanSuccess(&buf, "")

	if buf.Len() != 0 {
		t.Errorf("writeHumanSuccess with empty message should produce no output, got %q", buf.String())
	}
}

func TestWriteHumanErrorPlainNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var buf bytes.Buffer
	writeHumanError(&buf, errors.New("something failed"))

	got := buf.String()
	want := "Error: something failed\n"
	if got != want {
		t.Errorf("writeHumanError(NO_COLOR) = %q, want %q", got, want)
	}
	// Should NOT contain the cross icon when colors are disabled
	if bytes.Contains(buf.Bytes(), []byte("\u2718")) {
		t.Error("expected no cross icon when NO_COLOR is set")
	}
}

func TestWriterSuccessHumanMode(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: false, Stdout: &stdout, Stderr: &stderr}

	w.Success(map[string]string{"key": "val"}, "Operation succeeded")

	// In human mode, only the message is printed to stdout
	got := stdout.String()
	if got != "Operation succeeded\n" {
		t.Errorf("Writer.Success human mode stdout = %q, want %q", got, "Operation succeeded\n")
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output, got %q", stderr.String())
	}
}

func TestWriterSuccessHumanModeMultiLine(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: false, Stdout: &stdout, Stderr: &stderr}

	table := "┌────┐\n│ OK │\n└────┘"
	w.Success(nil, table)

	got := stdout.String()
	want := table + "\n"
	if got != want {
		t.Errorf("Writer.Success human mode multi-line stdout = %q, want %q", got, want)
	}
	if bytes.Contains(stdout.Bytes(), []byte("\u2714")) {
		t.Error("expected no checkmark icon for multi-line output via Writer.Success")
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output, got %q", stderr.String())
	}
}

func TestWriterSuccessJSONMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, Stdout: &stdout, Stderr: &stderr}

	w.Success(map[string]string{"key": "val"}, "it worked")

	var env successEnvelope
	err := json.Unmarshal(stdout.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if !env.OK {
		t.Error("ok = false, want true")
	}
	if env.Message != "it worked" {
		t.Errorf("message = %q, want %q", env.Message, "it worked")
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr in JSON mode, got %q", stderr.String())
	}
}

func TestWriterSuccessJSONModeUnchangedByNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, Stdout: &stdout, Stderr: &stderr}

	w.Success(map[string]string{"key": "val"}, "it worked")

	var env successEnvelope
	err := json.Unmarshal(stdout.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if !env.OK {
		t.Error("ok = false, want true")
	}
	if env.Message != "it worked" {
		t.Errorf("message = %q, want %q", env.Message, "it worked")
	}
}

func TestWriterErrorJSONModeUnchangedByNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, Stdout: &stdout, Stderr: &stderr}

	code := w.Error(errors.New("fail"), ErrNotFound)
	if code != ExitNotFound {
		t.Errorf("exit code = %d, want %d", code, ExitNotFound)
	}

	var env errorEnvelope
	err := json.Unmarshal(stdout.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if env.OK {
		t.Error("ok = true, want false")
	}
	if env.Error != "fail" {
		t.Errorf("error = %q, want %q", env.Error, "fail")
	}
	if env.Code != ErrNotFound {
		t.Errorf("code = %q, want %q", env.Code, ErrNotFound)
	}
}

func TestWriterWarnSuppressedInJSONMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, Stdout: &stdout, Stderr: &stderr}

	w.Warn("should not appear")
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output in JSON mode, got %q", stderr.String())
	}
}

func TestWriterWarnEmitsInHumanMode(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	w := &Writer{Stdout: &stdout, Stderr: &stderr}

	w.Warn("something is off")
	got := stderr.String()
	if got != "Warning: something is off\n" {
		t.Errorf("Writer.Warn = %q, want %q", got, "Warning: something is off\n")
	}
}

// enableColors makes render.ColorsEnabled() true for the duration of one test.
//
// t.Setenv cannot express it on its own: ColorsEnabled uses LookupEnv, so
// NO_COLOR="" still disables colors. The variable has to be genuinely absent.
func enableColors(t *testing.T) {
	t.Helper()
	if prev, ok := os.LookupEnv("NO_COLOR"); ok {
		testsupport.Must(t, os.Unsetenv("NO_COLOR"), "unsetting NO_COLOR: %v", nil)
		t.Cleanup(func() { _ = os.Setenv("NO_COLOR", prev) })
	}
	t.Setenv("TERM", "xterm-256color")
	if !render.ColorsEnabled() {
		t.Fatal("premise: colors are still disabled, so the glyph under test " +
			"would not be emitted either way")
	}
}

// TestWriterOutcomeSwapsTheCheckmarkForTheFailureGlyph is DKT-982's second
// acceptance criterion at the writer: a parked outcome must not be presented
// behind a "✔".
//
// Success is exercised in the same test over the same message, because the
// claim is a DIFFERENCE between the two paths — asserting only that Outcome
// prints ✘ would still pass if Success had quietly stopped printing ✔.
func TestWriterOutcomeSwapsTheCheckmarkForTheFailureGlyph(t *testing.T) {
	enableColors(t)

	const message = "gate self-hygiene failed (exit 2); STEP-3107 parked waiting-human"

	var adverse, stderr bytes.Buffer
	(&Writer{Stdout: &adverse, Stderr: &stderr}).Outcome(nil, message)

	if bytes.Contains(adverse.Bytes(), []byte("✔")) {
		t.Errorf("Outcome = %q, and it carries the success checkmark — a park "+
			"behind a ✔ is the whole of DKT-982", adverse.String())
	}
	if !bytes.Contains(adverse.Bytes(), []byte("✘")) {
		t.Errorf("Outcome = %q, want the failure glyph", adverse.String())
	}
	if !bytes.Contains(adverse.Bytes(), []byte(message)) {
		t.Errorf("Outcome = %q, want it to carry the message", adverse.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q; the command succeeded, so nothing goes there",
			stderr.String())
	}

	var ok bytes.Buffer
	(&Writer{Stdout: &ok, Stderr: &bytes.Buffer{}}).Success(nil, message)
	if !bytes.Contains(ok.Bytes(), []byte("✔")) {
		t.Errorf("Success = %q, want the checkmark it has always printed", ok.String())
	}
}

// TestWriterOutcomeJSONIsASuccessEnvelope pins the half that must NOT change:
// the recording succeeded, so a caller branching on `.ok` or on the exit code
// is told exactly what Success would tell it. Only the human line differs.
func TestWriterOutcomeJSONIsASuccessEnvelope(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, Stdout: &stdout, Stderr: &stderr}

	w.Outcome(map[string]string{"status": "waiting-human"}, "gate build failed (exit 2)")

	var env successEnvelope
	err := json.Unmarshal(stdout.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if !env.OK {
		t.Error("ok = false; the recording succeeded — its subject did not")
	}
	if env.Message != "gate build failed (exit 2)" {
		t.Errorf("message = %q, want the outcome line", env.Message)
	}
}

func TestWriterPartialFailureJSONKeepsDataInFailureEnvelope(t *testing.T) {
	for _, version := range []JSONVersion{JSONV1, JSONV2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			w := &Writer{JSONMode: true, JSONVersion: version, Stdout: &stdout, Stderr: &stderr}
			payload := map[string]any{"succeeded": float64(11), "failed": float64(2)}

			exit := w.PartialFailure(payload, "", errors.New("2 of 13 projects failed"), ErrConflict)

			if exit != ExitConflict {
				t.Errorf("exit code = %d, want %d", exit, ExitConflict)
			}
			var env map[string]any
			err := json.Unmarshal(stdout.Bytes(), &env)
			testsupport.Must(t, err, "stdout is not one JSON document: %v\n%s", err, stdout.String())
			if env["ok"] != false {
				t.Errorf("ok = %v, want false", env["ok"])
			}
			if !reflect.DeepEqual(env["data"], payload) {
				t.Errorf("data = %#v, want %#v", env["data"], payload)
			}
			if env["error"] != "2 of 13 projects failed" {
				t.Errorf("error = %v, want %q", env["error"], "2 of 13 projects failed")
			}
			if env["code"] != string(ErrConflict) {
				t.Errorf("code = %v, want %q", env["code"], ErrConflict)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing in JSON mode", stderr.String())
			}
		})
	}
}

func TestWriterPartialFailureHumanPrintsReportAndError(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	w := &Writer{Stdout: &stdout, Stderr: &stderr}

	exit := w.PartialFailure(nil, "project a: ok\nproject b: conflict", errors.New("1 of 2 projects failed"), ErrConflict)

	if exit != ExitConflict {
		t.Errorf("exit code = %d, want %d", exit, ExitConflict)
	}
	if stdout.String() != "project a: ok\nproject b: conflict\n" {
		t.Errorf("stdout = %q, want the report", stdout.String())
	}
	if stderr.String() != "Error: 1 of 2 projects failed\n" {
		t.Errorf("stderr = %q, want the error line", stderr.String())
	}
}

func TestColorsEnabledRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	if render.ColorsEnabled() {
		t.Error("ColorsEnabled() = true, want false when NO_COLOR is set")
	}
}

func TestColorsEnabledRespectsDumbTerm(t *testing.T) {
	t.Setenv("TERM", "dumb")

	if render.ColorsEnabled() {
		t.Error("ColorsEnabled() = true, want false when TERM=dumb")
	}
}
