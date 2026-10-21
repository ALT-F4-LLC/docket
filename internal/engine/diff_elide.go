package engine

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// GENERATED-FILE ELISION IN THE RENDERED PACKET (DKT-4252).
//
// A review step's packet inlines its `issue.diff` input whole. A change that
// regenerated protobuf output put 845 KB of generated hunks into one packet, and
// the claim script refused it against the Workflow script cap, so the review
// chain never ran. The recorded artifact stays whole; the RENDER replaces each
// generated file's hunks with one stat line carrying the blob hash, so the
// reviewer sees that the file changed, by how much, and which bytes, and reads
// the hand-written hunks in full.
//
// A file is generated when its section of the diff carries a
// `Code generated ... DO NOT EDIT.` header line in its first lines, or when the
// checkout's .gitattributes marks its path `linguist-generated`.

// generatedHeader is Go's convention for generated source, matched anywhere on
// a diff line so an added, removed, or context line all count.
var generatedHeader = regexp.MustCompile(`Code generated .*DO NOT EDIT`)

// generatedHeaderLines is how far into a file's diff section the header is
// looked for: it is the file's first line, so only hunk context within a few
// lines of the top can carry it.
const generatedHeaderLines = 40

// ElidedFile is one generated file the render replaced with a stat line.
type ElidedFile struct {
	Path string
	Blob string
	Adds int
	Dels int
}

// elideGeneratedDiff returns the diff with every generated file's section
// replaced by a stat line, and the files it elided. A diff with no generated
// file comes back byte-identical.
func elideGeneratedDiff(body string, attrGenerated func(path string) bool) (string, []ElidedFile) {
	if !strings.Contains(body, "diff --git ") {
		return body, nil
	}
	sections := splitDiffSections(body)
	var out strings.Builder
	var elided []ElidedFile
	for _, s := range sections {
		if s.path == "" || !(hasGeneratedHeader(s.text) || (attrGenerated != nil && attrGenerated(s.path))) {
			out.WriteString(s.text)
			continue
		}
		f := ElidedFile{Path: s.path}
		for i, line := range strings.Split(s.text, "\n") {
			switch {
			case strings.HasPrefix(line, "index "):
				if _, after, ok := strings.Cut(line, ".."); ok {
					f.Blob, _, _ = strings.Cut(after, " ")
				}
			case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), i == 0:
			case strings.HasPrefix(line, "+"):
				f.Adds++
			case strings.HasPrefix(line, "-"):
				f.Dels++
			}
		}
		elided = append(elided, f)
		fmt.Fprintf(&out, "diff --git a/%s b/%s\n", s.path, s.path)
		fmt.Fprintf(&out, "# generated file, hunks elided from this packet: %s (+%d -%d)", f.Path, f.Adds, f.Dels)
		if f.Blob != "" {
			fmt.Fprintf(&out, ", blob %s", f.Blob)
		}
		out.WriteString("\n# the full diff is the recorded issue.diff artifact\n")
	}
	return out.String(), elided
}

type diffSection struct {
	path string // "" for the preamble before the first file
	text string
}

// splitDiffSections cuts a unified diff at each `diff --git` line. Every
// section keeps its own trailing newline, so concatenating them is the input.
func splitDiffSections(body string) []diffSection {
	var sections []diffSection
	start := 0
	for i := 0; i <= len(body); {
		next := strings.Index(body[i:], "\ndiff --git ")
		if next < 0 {
			break
		}
		cut := i + next + 1
		sections = append(sections, newDiffSection(body[start:cut]))
		start, i = cut, cut+1
	}
	return append(sections, newDiffSection(body[start:]))
}

func newDiffSection(text string) diffSection {
	line, _, _ := strings.Cut(text, "\n")
	rest, ok := strings.CutPrefix(line, "diff --git ")
	if !ok {
		return diffSection{text: text}
	}
	_, b, ok := strings.Cut(rest, " b/")
	if !ok {
		return diffSection{text: text}
	}
	return diffSection{path: b, text: text}
}

func hasGeneratedHeader(section string) bool {
	lines := strings.SplitN(section, "\n", generatedHeaderLines+1)
	if len(lines) > generatedHeaderLines {
		lines = lines[:generatedHeaderLines]
	}
	for _, l := range lines {
		if generatedHeader.MatchString(l) {
			return true
		}
	}
	return false
}

// gitattributesGenerated reads root/.gitattributes and reports whether a path
// is marked `linguist-generated`. A pattern with no slash matches a basename; a
// pattern with one matches the full path. Later lines win, as in git. A
// missing or unreadable file marks nothing.
func gitattributesGenerated(root string) func(string) bool {
	if root == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(root, ".gitattributes"))
	if err != nil {
		return nil
	}
	type rule struct {
		pattern   string
		generated bool
	}
	var rules []rule
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		verdict, set := false, false
		for _, attr := range fields[1:] {
			switch attr {
			case "linguist-generated", "linguist-generated=true":
				verdict, set = true, true
			case "-linguist-generated", "linguist-generated=false":
				verdict, set = false, true
			}
		}
		if set {
			rules = append(rules, rule{pattern: fields[0], generated: verdict})
		}
	}
	if len(rules) == 0 {
		return nil
	}
	return func(p string) bool {
		generated := false
		for _, r := range rules {
			pattern := strings.TrimPrefix(r.pattern, "/")
			target := p
			if !strings.Contains(pattern, "/") {
				target = path.Base(p)
			}
			if ok, _ := path.Match(pattern, target); ok {
				generated = r.generated
			}
		}
		return generated
	}
}

// abridgeGeneratedDiffs returns the bundle with each `issue.diff` input's
// generated-file hunks elided (DKT-4252). The bundle itself is untouched: it
// also feeds `step context`, which keeps the full diff. A bundle with nothing
// to elide is returned as is.
func abridgeGeneratedDiffs(bundle *Context, attrGenerated func(string) bool) *Context {
	var inputs []ContextInput
	for i, in := range bundle.Inputs {
		if in.Kind != ArtifactKindIssueDiff {
			continue
		}
		body, elided := elideGeneratedDiff(in.Body, attrGenerated)
		if len(elided) == 0 {
			continue
		}
		if inputs == nil {
			inputs = append([]ContextInput(nil), bundle.Inputs...)
		}
		inputs[i].Body = body
	}
	if inputs == nil {
		return bundle
	}
	abridged := *bundle
	abridged.Inputs = inputs
	return &abridged
}
