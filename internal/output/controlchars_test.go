package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// DKT-62 reported that `docket issue create` printed
//
//	jq: parse error: Invalid string: control characters from U+0000 through
//	U+001F must be escaped
//
// and created the issue anyway, so a successful create read as a failure and
// the caller retried, orphaning two duplicate issues.
//
// THE ENGINE DOES NOT PRODUCE THAT OUTPUT. Both envelopes encode through
// encoding/json, which escapes every byte below U+0020 by contract, and the
// verb was measured emitting `\u0001` and `\u001f` correctly in v1 and v2 for
// a control character in the description, the title, and the rendered message
// alike. The parse error came from a jq invocation in the CALLER's own
// pipeline, not from what docket wrote.
//
// These tests exist because "we checked and it was fine" is not a durable
// answer — a future writer that reached for fmt.Sprintf to build an envelope
// would reintroduce exactly the reported symptom, and nothing would have
// caught it. They pin the property at the layer that owns it, so it holds for
// every verb rather than for the one that was reported.

// controlCharPayload carries the bytes jq refuses when they appear raw.
type controlCharPayload struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

func controlCharFixture() controlCharPayload {
	return controlCharPayload{
		// U+0001 and U+001F bracket the range jq names; the newline and tab in
		// between are the ones a real description actually contains, and are
		// control characters by the same rule.
		Title:       "ctrl\x01title",
		Description: "first line\nsecond\ttab\x1fend",
	}
}

// assertParsesAndRoundTrips is the whole obligation: the bytes are valid JSON,
// and the values survive intact. Escaping that mangled the content would pass a
// parse check and still lose the artifact.
func assertParsesAndRoundTrips(t *testing.T, envelope string, raw []byte) {
	t.Helper()

	if !json.Valid(raw) {
		t.Fatalf("%s envelope is not valid JSON: %s", envelope, raw)
	}
	// The literal check, because json.Valid alone would also accept an envelope
	// that dropped the field entirely.
	if bytes.ContainsAny(raw, "\x00\x01\x1f") {
		t.Errorf("%s envelope contains a RAW control byte; this is the exact "+
			"shape jq refuses: %q", envelope, raw)
	}
	if !strings.Contains(string(raw), `\u0001`) {
		t.Errorf("%s envelope does not carry the escaped control character; "+
			"the field may have been dropped rather than encoded: %s",
			envelope, raw)
	}

	var decoded struct {
		Data controlCharPayload `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s envelope does not decode: %v\n%s", envelope, err, raw)
	}
	want := controlCharFixture()
	if decoded.Data.Title != want.Title || decoded.Data.Description != want.Description {
		t.Errorf("%s envelope did not round-trip the control characters:\n got %q / %q\nwant %q / %q",
			envelope, decoded.Data.Title, decoded.Data.Description,
			want.Title, want.Description)
	}
}

func TestJSONV1EscapesControlCharacters(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, JSONVersion: JSONV1, Stdout: &stdout, Stderr: &stderr}

	// The MESSAGE carries them too: it is the rendered human line, built from
	// the same untrusted title, and it rides in the same envelope.
	w.Success(controlCharFixture(), "Created DKT-1: ctrl\x01title")

	assertParsesAndRoundTrips(t, "v1", stdout.Bytes())
}

func TestJSONV2EscapesControlCharacters(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, JSONVersion: JSONV2, Stdout: &stdout, Stderr: &stderr}

	w.Success(controlCharFixture(), "Created DKT-1: ctrl\x01title")

	assertParsesAndRoundTrips(t, "v2", stdout.Bytes())
}

// TestJSONErrorEnvelopeEscapesControlCharacters covers the other half of the
// reported confusion: the caller read an error and retried. An error envelope
// carrying an unescaped byte would be unparseable at exactly the moment a
// caller is trying to decide whether the command failed.
func TestJSONErrorEnvelopeEscapesControlCharacters(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := &Writer{JSONMode: true, JSONVersion: JSONV2, Stdout: &stdout, Stderr: &stderr}

	w.Error(&controlCharError{}, ErrValidation)

	raw := stdout.Bytes()
	if len(raw) == 0 {
		raw = stderr.Bytes()
	}
	if !json.Valid(raw) {
		t.Fatalf("the error envelope is not valid JSON: %s", raw)
	}
	if bytes.ContainsAny(raw, "\x00\x01\x1f") {
		t.Errorf("the error envelope contains a RAW control byte: %q", raw)
	}
}

type controlCharError struct{}

func (*controlCharError) Error() string { return "bad title: ctrl\x01title" }
