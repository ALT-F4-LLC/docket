package output

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestParseJSONMode covers the full accepted-value table. The boolean
// spellings matter: --json was a Bool flag before v2, so --json=true and
// --json=false parsed successfully then and must keep doing so.
func TestParseJSONMode(t *testing.T) {
	tests := []struct {
		raw     string
		want    JSONVersion
		wantErr bool
	}{
		{"", JSONNone, false},
		{"v1", JSONV1, false},
		{"true", JSONV1, false},
		{"1", JSONV1, false},
		{"v2", JSONV2, false},
		{"false", JSONNone, false},
		{"0", JSONNone, false},
		{"v3", JSONNone, true},
		{"yes", JSONNone, true},
		{"V2", JSONNone, true},
		{" v2", JSONNone, true},
	}

	for _, tt := range tests {
		got, err := ParseJSONMode(tt.raw)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseJSONMode(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseJSONMode(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

// TestBareJSONIsByteIdenticalToV1 is the compatibility guard for the most
// compat-sensitive edit in the program: converting --json from Bool to String.
// A bare --json (NoOptDefVal "v1") must produce exactly what the Bool flag
// produced.
func TestBareJSONIsByteIdenticalToV1(t *testing.T) {
	data := map[string]any{"key": "val", "n": 3}

	// What the Bool-era flag produced: New(true, false) -> v1 envelope.
	var legacy bytes.Buffer
	legacyWriter := &Writer{JSONMode: true, Stdout: &legacy, Stderr: &bytes.Buffer{}}
	legacyWriter.Success(data, "done")

	// What a bare --json produces now.
	version, err := ParseJSONMode("v1")
	testsupport.Must(t, err, "ParseJSONMode(v1): %v", err)
	var current bytes.Buffer
	currentWriter := &Writer{
		JSONMode: true, JSONVersion: version,
		Stdout: &current, Stderr: &bytes.Buffer{},
	}
	currentWriter.Success(data, "done")

	if legacy.String() != current.String() {
		t.Errorf("bare --json output drifted from v1:\n legacy = %q\ncurrent = %q",
			legacy.String(), current.String())
	}
}

// TestZeroValueWriterEmitsV1 pins the Writer zero value: a Writer with
// JSONMode set but no JSONVersion (as older code and existing tests build)
// must emit v1, never v2.
func TestZeroValueWriterEmitsV1(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{JSONMode: true, Stdout: &buf, Stderr: &bytes.Buffer{}}
	w.Success(testCollection{items: []int{1, 2}, total: 9, truncated: true}, "")

	var raw map[string]any
	err := json.Unmarshal(buf.Bytes(), &raw)
	testsupport.Must(t, err, "unmarshal: %v", err)
	dataMap, ok := raw["data"].(map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want map", raw["data"])
	}
	if _, hasItems := dataMap["items"]; hasItems {
		t.Error("zero-value Writer emitted a v2 envelope; want v1")
	}
}

func TestNewWithVersion(t *testing.T) {
	tests := []struct {
		version      JSONVersion
		wantJSONMode bool
	}{
		{JSONNone, false},
		{JSONV1, true},
		{JSONV2, true},
	}
	for _, tt := range tests {
		w := NewWithVersion(tt.version, false)
		if w.JSONMode != tt.wantJSONMode {
			t.Errorf("NewWithVersion(%v).JSONMode = %v, want %v",
				tt.version, w.JSONMode, tt.wantJSONMode)
		}
		if w.JSONVersion != tt.version {
			t.Errorf("NewWithVersion(%v).JSONVersion = %v", tt.version, w.JSONVersion)
		}
	}
}

// TestNewMatchesLegacyBoolConstructor pins New's Bool-era signature behavior.
func TestNewMatchesLegacyBoolConstructor(t *testing.T) {
	if w := New(true, false); w.JSONVersion != JSONV1 || !w.JSONMode {
		t.Errorf("New(true,false) = {mode:%v version:%v}, want {true JSONV1}",
			w.JSONMode, w.JSONVersion)
	}
	if w := New(false, false); w.JSONVersion != JSONNone || w.JSONMode {
		t.Errorf("New(false,false) = {mode:%v version:%v}, want {false JSONNone}",
			w.JSONMode, w.JSONVersion)
	}
}
