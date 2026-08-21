package output

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// testCollection is a minimal Collection implementation for envelope tests.
type testCollection struct {
	items     any
	total     int
	truncated bool
}

func (c testCollection) CollectionItems() any      { return c.items }
func (c testCollection) CollectionTotal() int      { return c.total }
func (c testCollection) CollectionTruncated() bool { return c.truncated }

func TestV2CollectionEnvelopeShape(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{
		JSONMode: true, JSONVersion: JSONV2,
		Stdout: &buf, Stderr: &bytes.Buffer{},
	}
	w.Success(testCollection{items: []string{"a", "b"}, total: 7, truncated: true}, "listed")

	var raw map[string]any
	err := json.Unmarshal(buf.Bytes(), &raw)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if raw["ok"] != true {
		t.Errorf("ok = %v, want true", raw["ok"])
	}
	data, ok := raw["data"].(map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want map", raw["data"])
	}

	// Exactly the three §5 keys, nothing more.
	for _, key := range []string{"items", "total", "truncated"} {
		if _, present := data[key]; !present {
			t.Errorf("v2 data missing %q key", key)
		}
	}
	if len(data) != 3 {
		t.Errorf("v2 data has %d keys (%v), want exactly 3", len(data), data)
	}

	if got := data["total"].(float64); got != 7 {
		t.Errorf("total = %v, want 7", got)
	}
	if data["truncated"] != true {
		t.Errorf("truncated = %v, want true", data["truncated"])
	}
	if items := data["items"].([]any); len(items) != 2 {
		t.Errorf("items length = %d, want 2", len(items))
	}
}

// TestV1IgnoresCollection is the dormancy guarantee at the type level: adding
// a Collection implementation must not change v1 output at all.
func TestV1IgnoresCollection(t *testing.T) {
	coll := testCollection{items: []string{"a"}, total: 42, truncated: true}

	var buf bytes.Buffer
	w := &Writer{
		JSONMode: true, JSONVersion: JSONV1,
		Stdout: &buf, Stderr: &bytes.Buffer{},
	}
	w.Success(coll, "")

	var raw map[string]any
	err := json.Unmarshal(buf.Bytes(), &raw)
	testsupport.Must(t, err, "unmarshal: %v", err)
	// testCollection has no exported fields, so v1 marshals it as {} — the
	// point being that v1 never consults the Collection methods.
	data, ok := raw["data"].(map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want map", raw["data"])
	}
	for _, key := range []string{"items", "total", "truncated"} {
		if _, present := data[key]; present {
			t.Errorf("v1 envelope leaked v2 key %q", key)
		}
	}
}

// TestV2NonCollectionPassesThrough: scalar results have no items/total/
// truncated to report, so v2 renders them exactly as v1 does.
func TestV2NonCollectionPassesThrough(t *testing.T) {
	data := map[string]string{"id": "DKT-1", "title": "t"}

	var v1buf, v2buf bytes.Buffer
	(&Writer{JSONMode: true, JSONVersion: JSONV1, Stdout: &v1buf, Stderr: &bytes.Buffer{}}).
		Success(data, "msg")
	(&Writer{JSONMode: true, JSONVersion: JSONV2, Stdout: &v2buf, Stderr: &bytes.Buffer{}}).
		Success(data, "msg")

	if v1buf.String() != v2buf.String() {
		t.Errorf("scalar payload differed between versions:\nv1 = %q\nv2 = %q",
			v1buf.String(), v2buf.String())
	}
}

func TestIsTruncated(t *testing.T) {
	tests := []struct {
		name                   string
		limit, total, returned int
		want                   bool
	}{
		{"no limit, all returned", 0, 100, 100, false},
		{"no limit set, fewer returned", 0, 100, 10, false},
		{"limit not reached", 50, 10, 10, false},
		{"limit exactly matches total", 10, 10, 10, false},
		{"limit truncates", 2, 10, 2, true},
		{"empty result", 10, 0, 0, false},
	}
	for _, tt := range tests {
		if got := IsTruncated(tt.limit, tt.total, tt.returned); got != tt.want {
			t.Errorf("%s: IsTruncated(%d,%d,%d) = %v, want %v",
				tt.name, tt.limit, tt.total, tt.returned, got, tt.want)
		}
	}
}

// TestExtendedErrorTaxonomy pins every code→exit mapping. The first four are
// the pre-existing contract and must never be renumbered.
func TestExtendedErrorTaxonomy(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want int
	}{
		{ErrGeneral, 1},
		{ErrNotFound, 2},
		{ErrValidation, 3},
		{ErrConflict, 4},
		{ErrAuth, 5},
		{ErrStaleLease, 6},
		{ErrTimeout, 7},
		{ErrUntrusted, 8},
		{ErrorCode("NEVER_DEFINED"), 1},
	}
	for _, tt := range tests {
		if got := ExitCodeForError(tt.code); got != tt.want {
			t.Errorf("ExitCodeForError(%q) = %d, want %d", tt.code, got, tt.want)
		}
	}
}

// TestErrorCodeStringsAreStable pins the wire strings — they are a public
// contract consumed by scripts.
func TestErrorCodeStringsAreStable(t *testing.T) {
	want := map[ErrorCode]string{
		ErrGeneral:    "GENERAL_ERROR",
		ErrNotFound:   "NOT_FOUND",
		ErrValidation: "VALIDATION_ERROR",
		ErrConflict:   "CONFLICT",
		ErrAuth:       "AUTH_ERROR",
		ErrStaleLease: "STALE_LEASE",
		ErrTimeout:    "TIMEOUT",
		ErrUntrusted:  "UNTRUSTED",
	}
	for code, str := range want {
		if string(code) != str {
			t.Errorf("code %v = %q, want %q", code, string(code), str)
		}
	}
}
