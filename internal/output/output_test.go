package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/render"
)

func TestWriteJSONSuccess(t *testing.T) {
	var buf bytes.Buffer
	writeJSONSuccess(&buf, map[string]string{"key": "val"}, "it worked")

	var env successEnvelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
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
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, exists := raw["message"]; exists {
		t.Error("expected message to be omitted when empty")
	}
}

func TestWriteJSONError(t *testing.T) {
	var buf bytes.Buffer
	writeJSONError(&buf, errors.New("something broke"), ErrNotFound)

	var env errorEnvelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
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
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
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
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
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
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
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
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
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
