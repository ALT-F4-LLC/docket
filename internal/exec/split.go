package exec

import (
	"errors"
	"fmt"
)

// ErrUnterminatedQuote is returned by Split for a line it cannot tokenize.
//
// A line that cannot be tokenized is NOT EXECUTED — the caller records
// verdict = "unmatched" with the parse failure as its reason. That is the same
// fail-closed direction as every other unknown in this stage: guessing at what
// an operator meant by an unbalanced quote is guessing at what command to run.
var ErrUnterminatedQuote = errors.New("unterminated quote")

// Split tokenizes ONE harvested line into an argv (§5.2 K2).
//
// This is the one piece of new parsing this stage adds to a security path, so
// its NON-GOALS are pinned as hard as its goals. The splitter handles exactly
// three things — single quotes, double quotes, and backslash escapes — and
// PERFORMS NO EXPANSION WHATSOEVER:
//
//   - no variable substitution: `$HOME` is a five-character literal
//   - no command substitution: `$(whoami)` is a single argument containing
//     those characters, and a backtick is a literal backtick
//   - no globbing: `*` and `?` are literal, and no filesystem is consulted
//   - no tilde expansion: `~` is a literal tilde
//   - no brace expansion
//   - no word splitting on anything but UNQUOTED WHITESPACE
//
// TestFenceSplitterExpandsNothing is a table over every metacharacter class,
// each asserting the character survives into the argv as a literal.
//
// The reason the splitter exists at all is that a harvested line is a STRING
// while everything downstream is a LIST. Tokenization happens ONCE, here, at
// the boundary — never again, and never by an interpreter.
func Split(line string) ([]string, error) {
	var (
		argv    []string
		cur     []rune
		started bool // distinguishes an empty quoted token from no token
	)

	flush := func() {
		if started {
			argv = append(argv, string(cur))
			cur = cur[:0]
			started = false
		}
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]

		switch c {
		case ' ', '\t', '\n', '\r':
			// Unquoted whitespace is the ONLY thing that separates tokens.
			flush()

		case '\'':
			// Single quotes: everything until the next single quote is
			// literal, including backslashes. This is the POSIX rule and it is
			// the one that makes `'$(whoami)'` obviously safe.
			started = true
			j := i + 1
			for j < len(runes) && runes[j] != '\'' {
				cur = append(cur, runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("%w: unbalanced single quote in %s", ErrUnterminatedQuote, Render(line))
			}
			i = j

		case '"':
			// Double quotes: whitespace is literal, and a backslash escapes
			// only the characters it must. NOTE what does NOT happen here — a
			// `$` inside double quotes stays a literal `$`, because this
			// splitter has no variable expansion to perform. A POSIX shell
			// would expand it; that difference is deliberate and is the whole
			// point of K2.
			started = true
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				if runes[j] == '\\' && j+1 < len(runes) && isDoubleQuoteEscapable(runes[j+1]) {
					cur = append(cur, runes[j+1])
					j += 2
					continue
				}
				cur = append(cur, runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("%w: unbalanced double quote in %s", ErrUnterminatedQuote, Render(line))
			}
			i = j

		case '\\':
			// An unquoted backslash escapes the next character literally. A
			// trailing backslash has nothing to escape and is a parse failure
			// rather than a silently dropped character.
			if i+1 >= len(runes) {
				return nil, fmt.Errorf("%w: trailing backslash in %s", ErrUnterminatedQuote, Render(line))
			}
			started = true
			cur = append(cur, runes[i+1])
			i++

		default:
			// EVERY other character — `$`, backtick, `*`, `?`, `~`, `;`, `&`,
			// `|`, `>`, `<`, `(`, `)`, `{`, `}` — is an ORDINARY CHARACTER
			// that accumulates into the current token. There is no case
			// statement for any of them because there is nothing special to
			// do: the absence of that code is the security property.
			started = true
			cur = append(cur, c)
		}
	}
	flush()

	return argv, nil
}

// isDoubleQuoteEscapable reports which characters a backslash may escape inside
// double quotes.
//
// The set is deliberately SMALL — only the quote, the backslash itself, and a
// newline. A backslash before anything else stays a literal backslash, which
// matches POSIX and, more importantly, means this function can never turn two
// harmless characters into one meaningful one.
func isDoubleQuoteEscapable(c rune) bool {
	return c == '"' || c == '\\' || c == '\n'
}
