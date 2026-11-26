package exec

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// hostileValues are the attack strings, each a real way to make a terminal
// show something other than what the bytes say.
var hostileValues = []struct {
	name  string
	value string
}{
	{"clear line and return", "make test\x1b[2K\r  make lint"},
	{"cursor up", "make test\x1b[1A  make lint"},
	{"bare carriage return", "make test\r  make lint"},
	{"bare newline", "make test\n  make lint"},
	{"bell", "make test\x07"},
	{"window title", "make test\x1b]0;t\x07"},
	{"backspace run", "make test\x08\x08\x08\x08lint"},
	{"C1 control", "make test\u0085make lint"},
	{"null", "make test\x00make lint"},
	{"vertical tab", "make test\vmake lint"},
}

// TestOperatorFacingRenderingEscapesControlBytes proves no raw control byte
// reaches an operator's terminal.
//
// THE ASSERTION IS ON THE WRITTEN BYTES, not on the string's appearance — a
// test that compared strings could pass while raw escapes still reached the
// terminal. All three value classes are driven through: an argv, a fence
// command, and a reason.
func TestOperatorFacingRenderingEscapesControlBytes(t *testing.T) {
	for _, hv := range hostileValues {
		t.Run(hv.name, func(t *testing.T) {
			// Class 1: a bare value (a fence command or a reason).
			assertRenderedSafely(t, Render(hv.value), hv.value)

			// Class 2: an argv, whose elements are individually quoted.
			assertRenderedSafely(t, RenderArgv([]string{"make", hv.value}), hv.value)
		})
	}
}

// assertRenderedSafely writes the rendered text to a real writer and inspects
// THE BYTES THAT ARRIVED.
func assertRenderedSafely(t *testing.T, rendered, original string) {
	t.Helper()

	var buf bytes.Buffer
	if _, err := fmt.Fprint(&buf, rendered); err != nil {
		t.Fatalf("writing the rendered value: %v", err)
	}
	written := buf.Bytes()

	// NO RAW C0/C1 CONTROL BYTE MAY REACH THE WRITER.
	for i, b := range written {
		if b < 0x20 || b == 0x7f {
			t.Errorf("raw control byte 0x%02x at offset %d reached the writer; rendered: %q", b, i, rendered)
		}
	}
	// The C1 range (0x80–0x9f) arrives as multi-byte UTF-8, so it is checked
	// as runes rather than bytes.
	for _, r := range string(written) {
		if r >= 0x80 && r <= 0x9f {
			t.Errorf("raw C1 control U+%04X reached the writer; rendered: %q", r, rendered)
		}
	}

	// THE ESCAPES APPEAR AS VISIBLE TEXT. %q is lossless and reversible, so
	// the operator sees that something odd is present rather than seeing
	// sanitized-but-plausible text with the hostile bytes silently removed.
	// Stripping would render the attack line as "make test  make lint", which
	// is STILL misleading; escaping renders it as what it is.
	if strings.ContainsAny(original, "\x1b") && !strings.Contains(rendered, `\x1b`) {
		t.Errorf("an escape byte must render as visible text; got %q", rendered)
	}
	if strings.Contains(original, "\r") && !strings.Contains(rendered, `\r`) {
		t.Errorf("a carriage return must render as visible text; got %q", rendered)
	}
	if strings.Contains(original, "\n") && !strings.Contains(rendered, `\n`) {
		t.Errorf("a newline must render as visible text; got %q", rendered)
	}
}

// TestRenderIsLosslessAndReversible proves the mechanism is Go %q, so the
// ORIGINAL bytes can be recovered from what was displayed. That is what
// distinguishes escaping from stripping.
func TestRenderIsLosslessAndReversible(t *testing.T) {
	for _, hv := range hostileValues {
		// strconv.Unquote is the exact inverse of the renderer's
		// strconv.Quote, which is what "lossless and reversible" means: the
		// operator's terminal showed a form from which the real bytes can be
		// recovered, rather than a cleaned-up form from which they cannot.
		back, err := strconv.Unquote(Render(hv.value))
		testsupport.Must(t, err, "%s: the rendered form must be unquotable: %v", hv.name, err)
		if back != hv.value {
			t.Errorf("%s: rendering must be reversible.\n got: %q\nwant: %q", hv.name, back, hv.value)
		}
	}
}

// TestRenderArgvKeepsElementBoundariesVisible proves the per-element rule.
//
// Per-element quoting rather than quoting a joined string is what keeps the
// boundaries visible: an argument containing a space renders as ONE quoted
// token, so the operator can see that ["make","a b"] is two arguments and not
// three.
func TestRenderArgvKeepsElementBoundariesVisible(t *testing.T) {
	got := RenderArgv([]string{"make", "a b", "$(whoami)"})
	want := `"make" "a b" "$(whoami)"`
	if got != want {
		t.Errorf("RenderArgv = %s, want %s", got, want)
	}

	if RenderArgv(nil) != "" {
		t.Error("an empty argv renders as the empty string")
	}
}

// TestNoHumanModePrintBypassesTheRenderer is the renderer's SURFACE GUARD.
//
// The rule is one line to state and one careless print call to violate, which
// is exactly the shape that needs a mechanical check. Every place this package
// builds an operator-facing message about an argv, a command, or a reason must
// route the value through Render or RenderArgv.
func TestNoHumanModePrintBypassesTheRenderer(t *testing.T) {
	entries, err := os.ReadDir(".")
	testsupport.Must(t, err, "reading the package directory: %v", err)

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		code := codeWithoutComments(t, name)

		// This package must not print directly at all: it RETURNS values and
		// errors, and the CLI decides how to display them. A direct print here
		// would be a print no caller can route through the renderer.
		for _, printer := range []string{"fmt.Print", "fmt.Println", "fmt.Printf", "fmt.Fprint", "os.Stdout", "os.Stderr"} {
			if strings.Contains(code, printer) {
				t.Errorf("%s calls %s; this package returns values rather than printing, so every display goes through the caller's renderer", name, printer)
			}
		}

		// Where a file BUILDS a refusal message — as opposed to merely
		// declaring the sentinel error — it must reference the renderer, since
		// those messages embed argvs, resolved paths, and fence content, all
		// of which are attacker-influenced and all of which reach a terminal.
		//
		// The distinction matters: exec.go DECLARES ErrRefused and formats
		// nothing, so requiring a Render call there would be requiring an
		// unused import. What is checked is the construction site.
		buildsRefusal := strings.Contains(code, "fmt.Errorf") &&
			strings.Contains(code, "ErrRefused")
		if buildsRefusal && !strings.Contains(code, "Render(") {
			t.Errorf("%s formats refusal messages but never calls Render; attacker-influenced values in a reason must be escaped", name)
		}
	}
}

// TestRendererIsTheOnlyOne asserts the "one renderer" rule structurally: a
// second escaping implementation is how the two drift apart and one of them
// stops being applied.
func TestRendererIsTheOnlyOne(t *testing.T) {
	code := codeWithoutComments(t, "render.go")
	if !strings.Contains(code, "strconv.Quote") {
		t.Error("the renderer must use strconv.Quote semantics, not a hand-rolled character table")
	}

	entries, err := os.ReadDir(".")
	testsupport.Must(t, err, "reading the package directory: %v", err)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "render.go" {
			continue
		}
		if strings.Contains(codeWithoutComments(t, name), "strconv.Quote") {
			t.Errorf("%s calls strconv.Quote directly; there must be exactly ONE renderer and it lives in render.go", name)
		}
	}
}
