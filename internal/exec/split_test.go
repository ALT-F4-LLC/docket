package exec

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestFenceSplitterExpandsNothing is §5.2 K2's table.
//
// The splitter is the ONE piece of new parsing this stage adds to a security
// path, so its NON-GOALS are tested as hard as its goals: every metacharacter
// class must survive into the argv AS A LITERAL. A splitter that expanded any
// of these would reintroduce, at the harvest boundary, exactly the injection
// the no-interpreter rule closes at the spawn boundary.
func TestFenceSplitterExpandsNothing(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		// The ordinary case, so the table is not only adversarial.
		{"plain", "make test", []string{"make", "test"}},
		{"extra whitespace collapses", "  make   test  ", []string{"make", "test"}},
		{"tabs separate", "make\ttest", []string{"make", "test"}},

		// NO VARIABLE SUBSTITUTION: a dollar is a literal dollar sign.
		{"dollar variable", "echo $HOME", []string{"echo", "$HOME"}},
		{"braced variable", "echo ${HOME}", []string{"echo", "${HOME}"}},
		{"dollar in quotes", `echo "$HOME"`, []string{"echo", "$HOME"}},
		{"positional", "echo $1", []string{"echo", "$1"}},
		{"special var", "echo $?", []string{"echo", "$?"}},

		// NO COMMAND SUBSTITUTION: these are single arguments containing those
		// characters, not commands to run.
		{"command substitution", "echo $(whoami)", []string{"echo", "$(whoami)"}},
		{"backtick substitution", "echo `whoami`", []string{"echo", "`whoami`"}},
		{"arithmetic", "echo $((1+1))", []string{"echo", "$((1+1))"}},

		// NO GLOBBING: no filesystem is ever consulted.
		{"glob star", "ls *.go", []string{"ls", "*.go"}},
		{"glob question", "ls ?.go", []string{"ls", "?.go"}},
		{"glob bracket", "ls [abc].go", []string{"ls", "[abc].go"}},

		// NO TILDE EXPANSION.
		{"tilde alone", "ls ~", []string{"ls", "~"}},
		{"tilde path", "ls ~/secret", []string{"ls", "~/secret"}},

		// NO BRACE EXPANSION.
		{"brace", "echo {a,b}", []string{"echo", "{a,b}"}},

		// CONTROL OPERATORS ARE ORDINARY CHARACTERS. This is the row that
		// matters most: a splitter that treated `;` as a separator would turn
		// one fence line into two commands.
		{"semicolon", "make test; rm -rf /", []string{"make", "test;", "rm", "-rf", "/"}},
		{"double ampersand", "make test && rm -rf /", []string{"make", "test", "&&", "rm", "-rf", "/"}},
		{"pipe", "make test | tee out", []string{"make", "test", "|", "tee", "out"}},
		{"redirect", "make test > out", []string{"make", "test", ">", "out"}},
		{"background", "make test &", []string{"make", "test", "&"}},
		{"subshell", "(make test)", []string{"(make", "test)"}},

		// QUOTING: the three things the splitter DOES handle.
		{"single quotes keep whitespace", "echo 'a b'", []string{"echo", "a b"}},
		{"double quotes keep whitespace", `echo "a b"`, []string{"echo", "a b"}},
		{"single quotes are fully literal", `echo '$(whoami)'`, []string{"echo", "$(whoami)"}},
		{"single quotes keep backslashes", `echo 'a\b'`, []string{"echo", `a\b`}},
		{"escaped space joins", `echo a\ b`, []string{"echo", "a b"}},
		{"escaped quote", `echo \"`, []string{"echo", `"`}},
		{"escaped backslash in quotes", `echo "a\\b"`, []string{"echo", `a\b`}},
		{"escaped quote in quotes", `echo "a\"b"`, []string{"echo", `a"b`}},
		{"adjacent quoted parts join", `echo "a"'b'c`, []string{"echo", "abc"}},
		{"empty quoted token survives", `echo ""`, []string{"echo", ""}},
		{"quote inside a word", `echo it's`, nil}, // unterminated: refused below

		// A comment character is NOT a comment.
		{"hash is literal", "make test # comment", []string{"make", "test", "#", "comment"}},

		// An empty line yields no argv rather than a bogus one.
		{"empty line", "", nil},
		{"whitespace only", "   \t  ", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Split(tt.line)
			if tt.want == nil && tt.name == "quote inside a word" {
				// Covered by the unterminated-quote test below; an apostrophe
				// in an unquoted word IS an unbalanced quote, and refusing is
				// the fail-closed direction.
				if err == nil {
					t.Fatalf("expected a refusal for %q, got %q", tt.line, got)
				}
				return
			}
			testsupport.Must(t, err, "Split(%q): %v", tt.line, err)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Split(%q)\n got: %q\nwant: %q", tt.line, got, tt.want)
			}
		})
	}
}

// TestFenceSplitterRefusesUnterminatedQuotes is the fail-closed half.
//
// A line the splitter cannot tokenize is NOT EXECUTED — it records unmatched
// with the parse failure as its reason. Guessing at what an operator meant by
// an unbalanced quote is guessing at what command to run, which is the one
// thing this package must never do.
func TestFenceSplitterRefusesUnterminatedQuotes(t *testing.T) {
	for _, line := range []string{
		`echo "unterminated`,
		`echo 'unterminated`,
		`echo it's`,
		`echo "a" "b`,
		`echo trailing\`,
	} {
		t.Run(line, func(t *testing.T) {
			got, err := Split(line)
			if err == nil {
				t.Fatalf("Split(%q) must refuse, got %q", line, got)
			}
			if !errors.Is(err, ErrUnterminatedQuote) {
				t.Errorf("expected ErrUnterminatedQuote, got %v", err)
			}
			if got != nil {
				t.Errorf("a refused line must yield no argv, got %q", got)
			}
			// The reason quotes the offending line THROUGH THE RENDERER, since
			// fence content is attacker-influenced and this reason reaches a
			// terminal.
			if !strings.Contains(err.Error(), `"`) {
				t.Errorf("the refusal should render the offending line; got: %v", err)
			}
		})
	}
}

// TestFenceSplitterRefusalRendersControlBytes ties the splitter's reason to
// §5.7: a hostile fence line's bytes must not reach a terminal raw.
func TestFenceSplitterRefusalRendersControlBytes(t *testing.T) {
	_, err := Split("make test\x1b[2K\r \"unterminated")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.ContainsRune(err.Error(), '\x1b') || strings.ContainsRune(err.Error(), '\r') {
		t.Error("the refusal must not carry raw control bytes to a terminal")
	}
	if !strings.Contains(err.Error(), `\x1b`) {
		t.Errorf("the escape must be visible in the refusal; got: %v", err)
	}
}

// TestFenceSplitterOutputIsSafeToExecute closes the loop: whatever the splitter
// produces goes to the runner as ARGV ELEMENTS, and the runner never joins them
// back into a string. A metacharacter that survived tokenization as a literal
// must also survive the spawn as a literal.
func TestFenceSplitterOutputIsSafeToExecute(t *testing.T) {
	argv, err := Split(`echo "$(whoami); rm -rf /"`)
	testsupport.Must(t, err, "Split: %v", err)
	want := []string{"echo", "$(whoami); rm -rf /"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("Split produced %q, want %q", argv, want)
	}

	// And that element reaches the child intact.
	spec := witnessSpec(t, "", argv[1])
	res, err := Run(spec)
	testsupport.Must(t, err, "Run: %v", err)
	got := parseArgs(res.Output)
	if len(got) != 1 || got[0] != want[1] {
		t.Errorf("the tokenized element must reach the child verbatim; got %q", got)
	}
}
