package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
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
