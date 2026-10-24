package output

import (
	"bytes"
	"encoding/json"
	"reflect"
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

// siblingCollection is a Collection that also reports envelope siblings.
type siblingCollection struct {
	testCollection
	siblings map[string]any
}

func (c siblingCollection) CollectionSiblings() map[string]any { return c.siblings }

// TestV2CollectionEnvelopeShape: a Collection without CollectionSiblings
// yields exactly items, total and truncated.
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

	// Exactly the three envelope keys, nothing more.
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

// TestV2CollectionSiblingsBesideEnvelope: a Collection's siblings land beside
// items, total and truncated, and a sibling naming one of those three does not
// replace it.
func TestV2CollectionSiblingsBesideEnvelope(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{
		JSONMode: true, JSONVersion: JSONV2,
		Stdout: &buf, Stderr: &bytes.Buffer{},
	}
	w.Success(siblingCollection{
		testCollection: testCollection{items: []string{"a"}, total: 1},
		siblings:       map[string]any{"limits": map[string]int{"write": 2}, "total": 99},
	}, "")

	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	err := json.Unmarshal(buf.Bytes(), &envelope)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if len(envelope.Data) != 4 {
		t.Errorf("v2 data has %d keys (%v), want items, total, truncated and limits",
			len(envelope.Data), envelope.Data)
	}
	if got := string(envelope.Data["limits"]); got != `{"write":2}` {
		t.Errorf("limits = %s, want {\"write\":2}", got)
	}
	if got := string(envelope.Data["total"]); got != "1" {
		t.Errorf("total = %s, want the collection's 1, not the sibling's 99", got)
	}
}

// TestV2NonCollectionPassesThrough: scalar results have no items/total/
// truncated to report, so v2 renders the payload as data unchanged.
func TestV2NonCollectionPassesThrough(t *testing.T) {
	data := map[string]string{"id": "DKT-1", "title": "t"}

	var buf bytes.Buffer
	(&Writer{JSONMode: true, JSONVersion: JSONV2, Stdout: &buf, Stderr: &bytes.Buffer{}}).
		Success(data, "msg")

	var envelope struct {
		OK      bool              `json:"ok"`
		Data    map[string]string `json:"data"`
		Message string            `json:"message"`
	}
	err := json.Unmarshal(buf.Bytes(), &envelope)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if !envelope.OK || envelope.Message != "msg" {
		t.Errorf("envelope = {ok:%v message:%q}, want {true \"msg\"}", envelope.OK, envelope.Message)
	}
	if !reflect.DeepEqual(envelope.Data, data) {
		t.Errorf("data = %#v, want the payload unchanged %#v", envelope.Data, data)
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

		ErrUnavailable:     "UNAVAILABLE",
		ErrIncompatible:    "INCOMPATIBLE",
		ErrUnauthenticated: "UNAUTHENTICATED",
	}
	for code, str := range want {
		if string(code) != str {
			t.Errorf("code %v = %q, want %q", code, string(code), str)
		}
	}
}
