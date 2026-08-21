package trust

import (
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// goCodeWithoutComments returns a Go file's source with every comment removed.
//
// The source guards in this package and in internal/exec are grep-level checks
// over CODE, and they must not fire on the prose that explains them. This is
// the same decision the genericity gate makes for the same reason: a rule whose
// own rationale cannot be written down is a rule that will be violated by
// someone who never learned why it exists.
//
// go/parser rather than a regex, because a regex that strips comments also
// strips a `//` inside a string literal — and the string literals are exactly
// what these guards read.
func goCodeWithoutComments(t *testing.T, filename string) string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, parser.SkipObjectResolution)
	testsupport.Must(t, err, "parsing %s: %v", filename, err)
	// ParseFile without ParseComments already drops comments from the AST;
	// clearing Comments makes that explicit so a future edit that adds
	// ParseComments does not silently reintroduce them.
	file.Comments = nil

	var b strings.Builder
	err = printer.Fprint(&b, fset, file)
	testsupport.Must(t, err, "printing %s: %v", filename, err)
	return b.String()
}
