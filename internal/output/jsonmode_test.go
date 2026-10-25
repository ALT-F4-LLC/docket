package output

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestParseJSONMode covers the full accepted-value table. The boolean
// spellings matter: --json was a Bool flag before v2, so --json=true and
// --json=false parsed successfully then and must keep doing so. The v1
// dialect is retired and rejected.
func TestParseJSONMode(t *testing.T) {
	tests := []struct {
		raw     string
		want    JSONVersion
		wantErr bool
	}{
		{"", JSONNone, false},
		{"v1", JSONNone, true},
		{"true", JSONV2, false},
		{"1", JSONV2, false},
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

// TestZeroValueWriterEmitsV2 pins the Writer zero value: a Writer with
// JSONMode set but no JSONVersion renders the v2 envelope, the only JSON
// dialect.
func TestZeroValueWriterEmitsV2(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{JSONMode: true, Stdout: &buf, Stderr: &bytes.Buffer{}}
	w.Success(testCollection{items: []int{1, 2}, total: 9, truncated: true}, "")

	var envelope struct {
		Data struct {
			Items     []int `json:"items"`
			Total     int   `json:"total"`
			Truncated bool  `json:"truncated"`
		} `json:"data"`
	}
	err := json.Unmarshal(buf.Bytes(), &envelope)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if len(envelope.Data.Items) != 2 || envelope.Data.Total != 9 || !envelope.Data.Truncated {
		t.Errorf("zero-value Writer data = %+v, want {items:[1 2] total:9 truncated:true}\n%s",
			envelope.Data, buf.String())
	}
}

func TestNewWithVersion(t *testing.T) {
	tests := []struct {
		version      JSONVersion
		wantJSONMode bool
	}{
		{JSONNone, false},
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

// TestNewMatchesLegacyBoolConstructor pins New's Bool-era signature behavior:
// the bool alone selects JSON mode with the v2 envelope.
func TestNewMatchesLegacyBoolConstructor(t *testing.T) {
	if w := New(true, false); w.JSONVersion != JSONV2 || !w.JSONMode {
		t.Errorf("New(true,false) = {mode:%v version:%v}, want {true JSONV2}",
			w.JSONMode, w.JSONVersion)
	}
	if w := New(false, false); w.JSONVersion != JSONNone || w.JSONMode {
		t.Errorf("New(false,false) = {mode:%v version:%v}, want {false JSONNone}",
			w.JSONMode, w.JSONVersion)
	}
}
