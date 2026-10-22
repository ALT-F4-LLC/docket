package engine

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// GENERATED-FILE ELISION IN THE RENDERED PACKET (DKT-4252).
//
// A review step's packet inlines its `issue.diff` input whole. A change that
// regenerated protobuf output put 845 KB of generated hunks into one packet, and
// the claim script refused it against the Workflow script cap, so the review
// chain never ran. The recorded artifact stays whole (a binary section past
// perFileDiffCap aside); the RENDER replaces each generated file's hunks with
// one stat line carrying the blob hash, so the
// reviewer sees that the file changed, by how much, and which bytes, and reads
// the hand-written hunks in full.
//
// A file is generated when the .gitattributes of the commit the run pinned
// marks its path `linguist-generated`, or when the file carries a
// `Code generated ... DO NOT EDIT.` header line in its first lines both BEFORE
// and AFTER the change. Neither test reads anything a writer controls: the
// attributes are read from the pinned commit, not the live checkout, and a
// header counts only on a file that already carried it, so a writer can
// neither add the line to hide a new or hand-written file nor keep a file
// elided while removing the line and rewriting it by hand. A file the change
// creates has no earlier state and is elided only by attribute.

// generatedHeader is Go's convention for generated source, the whole line:
// `// Code generated ... DO NOT EDIT.`. Unanchored, it matched any diff line
// that merely mentioned the phrase, so a hand-written file could hide behind
// one added or quoted line.
var generatedHeader = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// generatedHeaderLines is how far into a file the header is looked for: it
// precedes the package clause, so only a file's first few lines can carry it.
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
// file comes back byte-identical. blobHeader reports whether a blob of the
// run's repository starts with the generated header; nil reads no blob.
func elideGeneratedDiff(
	body string, attrGenerated func(path string) bool, blobHeader func(blob string) bool,
) (string, []ElidedFile) {
	if !strings.Contains(body, "diff --git ") {
		return body, nil
	}
	sections := splitDiffSections(body)
	var out strings.Builder
	var elided []ElidedFile
	for _, s := range sections {
		if s.path == "" || !((attrGenerated != nil && attrGenerated(s.path)) ||
			generatedBothSides(s.text, blobHeader)) {
			out.WriteString(s.text)
			continue
		}
		f := ElidedFile{Path: s.path}
		_, f.Blob = indexBlobs(s.text)
		// Only lines after the first hunk header are content: a removed line
		// reading `-- x` renders as `--- x`, and the file header's own `---`
		// and `+++` lines come before it (DKT-2550).
		inHeader := true
		for i, line := range strings.Split(s.text, "\n") {
			switch {
			case i == 0:
			case strings.HasPrefix(line, "@@ "):
				inHeader = false
			case inHeader:
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
		// A line starting `#` is never diff content (hunk lines start with a
		// space, `+`, `-` or `\`), so it is a trailer the diff builder appended
		// after the last in-scope file: the out-of-scope disclosure and round
		// delta headings. Dropping them with the hunks would let the hunks that
		// follow read as in-scope.
		for _, line := range strings.Split(s.text, "\n") {
			if strings.HasPrefix(line, "#") {
				out.WriteString(line + "\n")
			}
		}
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

// generatedBothSides reports whether a file's section shows the generated
// header in the file's first lines both before and after the change. Each side
// is read from the first hunk when that hunk starts at the side's line 1, and
// otherwise from the side's blob (a regenerated file's hunks usually start
// deep in it). A created file has no before and is never header-generated; a
// deleted file has no after, so its before decides.
func generatedBothSides(section string, blobHeader func(string) bool) bool {
	header, _, _ := strings.Cut(section, "\n@@ ")
	if strings.Contains(header, "\nnew file mode ") {
		return false
	}
	before, after := indexBlobs(header)
	if !sideHasGeneratedHeader(section, '-', before, blobHeader) {
		return false
	}
	if strings.Contains(header, "\ndeleted file mode ") {
		return true
	}
	return sideHasGeneratedHeader(section, '+', after, blobHeader)
}

func sideHasGeneratedHeader(section string, sign byte, blob string, blobHeader func(string) bool) bool {
	if headerInFirstHunk(section, sign) {
		return true
	}
	return blobHeader != nil && blob != "" && blobHeader(blob)
}

// headerInFirstHunk reports whether the first hunk starts at line 1 of the
// side sign names (`-` before, `+` after) and shows the header among that
// side's lines within generatedHeaderLines of the top.
func headerInFirstHunk(section string, sign byte) bool {
	lines := strings.Split(section, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "@@ ") {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) < 3 {
			return false
		}
		rng := fields[1]
		if sign == '+' {
			rng = fields[2]
		}
		if !startsAtLineOne(rng, string(sign)) {
			return false
		}
		body := lines[i+1:]
		if len(body) > generatedHeaderLines {
			body = body[:generatedHeaderLines]
		}
		for _, b := range body {
			if b == "" || strings.HasPrefix(b, "@@ ") {
				break
			}
			if (b[0] == ' ' || b[0] == sign) && generatedHeader.MatchString(b[1:]) {
				return true
			}
		}
		return false
	}
	return false
}

// gitattributesGenerated reads the root .gitattributes of the commit the run
// pinned and reports whether a path is marked `linguist-generated`. It is read
// from that commit, not the working tree, so every render of the run's
// packets judges the same files generated whatever later lands in the shared
// checkout. No pinned commit, or no file in it, marks nothing.
func gitattributesGenerated(root, commit string) func(string) bool {
	if root == "" || !fullCommitSHA.MatchString(commit) {
		return nil
	}
	raw, err := gitCommand("-c", "core.fsmonitor=false", "-C", root,
		"cat-file", "blob", commit+":.gitattributes").Output()
	if err != nil {
		return nil
	}
	return parseGeneratedAttributes(string(raw))
}

// parseGeneratedAttributes reads .gitattributes text for `linguist-generated`.
// Later lines win, as in git.
func parseGeneratedAttributes(raw string) func(string) bool {
	type rule struct {
		pattern   string
		generated bool
	}
	var rules []rule
	for _, line := range strings.Split(raw, "\n") {
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
			if attrPatternMatch(r.pattern, p) {
				generated = r.generated
			}
		}
		return generated
	}
}

// attrPatternMatch matches a root .gitattributes pattern against a repository
// path as git does: a pattern with no slash matches the basename at any depth;
// one with a slash is anchored at the root and matched a segment at a time, so
// `*` never crosses a `/`, while a `**` segment does (leading or inner, any
// number of directories including none; trailing, everything inside).
func attrPatternMatch(pattern, p string) bool {
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(p))
		return ok
	}
	return matchSegments(strings.Split(strings.TrimPrefix(pattern, "/"), "/"), strings.Split(p, "/"))
}

func matchSegments(pattern, segs []string) bool {
	if len(pattern) == 0 {
		return len(segs) == 0
	}
	if pattern[0] == "**" {
		if len(pattern) == 1 {
			return len(segs) > 0
		}
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pattern[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, _ := path.Match(pattern[0], segs[0])
	return ok && matchSegments(pattern[1:], segs[1:])
}

// abridgeGeneratedDiffs returns the bundle with each `issue.diff` input's
// generated-file hunks elided (DKT-4252). The bundle itself is untouched: it
// also feeds `step context`, which keeps the full diff. A bundle with nothing
// to elide is returned as is.
func abridgeGeneratedDiffs(
	bundle *Context, attrGenerated func(string) bool, blobHeader func(string) bool,
) *Context {
	var inputs []ContextInput
	for i, in := range bundle.Inputs {
		if in.Kind != ArtifactKindIssueDiff {
			continue
		}
		body, elided := elideGeneratedDiff(in.Body, attrGenerated, blobHeader)
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

// startsAtLineOne reports whether a hunk range such as `-1,5`, `-1` or `+1,9`
// starts at line 1 (not 10, 11, ...).
func startsAtLineOne(rng, sign string) bool {
	rest, ok := strings.CutPrefix(rng, sign+"1")
	return ok && (rest == "" || strings.HasPrefix(rest, ","))
}

// indexBlobs are the blobs a section's `index OLD..NEW` line names, each ""
// when it is the all-zero hash (no such side) or the line is missing.
func indexBlobs(section string) (before, after string) {
	for _, line := range strings.Split(section, "\n") {
		rest, ok := strings.CutPrefix(line, "index ")
		if !ok {
			continue
		}
		old, rest, ok := strings.Cut(rest, "..")
		if !ok {
			return "", ""
		}
		after, _, _ = strings.Cut(rest, " ")
		if strings.Trim(old, "0") == "" {
			old = ""
		}
		if strings.Trim(after, "0") == "" {
			after = ""
		}
		return old, after
	}
	return "", ""
}

// blobID is the shape of an object id the blob reader asks git for; anything
// else (a revision expression, a line break) is never sent.
var blobID = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// blobHeaderReader reports whether blobs of root's object database start with
// the generated header. One `git cat-file --batch` serves a whole render,
// started on the first question, and each answer is remembered. A blob the
// database does not hold (a live worktree's unrecorded bytes), or any read
// failure, marks nothing.
type blobHeaderReader struct {
	root   string
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	failed bool
	seen   map[string]bool
}

func newBlobHeaderReader(root string) *blobHeaderReader {
	return &blobHeaderReader{root: root, failed: root == "", seen: map[string]bool{}}
}

// Has reports whether the blob starts with the generated header.
func (r *blobHeaderReader) Has(blob string) bool {
	if r.failed || !blobID.MatchString(blob) {
		return false
	}
	if v, ok := r.seen[blob]; ok {
		return v
	}
	v, err := r.read(blob)
	if err != nil {
		r.fail()
		return false
	}
	r.seen[blob] = v
	return v
}

func (r *blobHeaderReader) read(blob string) (bool, error) {
	if r.cmd == nil {
		cmd := gitCommand("-c", "core.fsmonitor=false", "-C", r.root, "cat-file", "--batch")
		in, err := cmd.StdinPipe()
		if err != nil {
			return false, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return false, err
		}
		if err := cmd.Start(); err != nil {
			return false, err
		}
		r.cmd, r.in, r.out = cmd, in, bufio.NewReader(out)
	}
	if _, err := io.WriteString(r.in, blob+"\n"); err != nil {
		return false, err
	}
	head, err := r.out.ReadString('\n')
	if err != nil {
		return false, err
	}
	// `<oid> <type> <size>`, or `<name> missing` / `<name> ambiguous` with no
	// content after it.
	fields := strings.Fields(head)
	if len(fields) != 3 {
		return false, nil
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return false, err
	}
	content := io.LimitReader(r.out, size)
	found := false
	if fields[1] == "blob" {
		lines := bufio.NewReader(io.LimitReader(content, 4096))
		for i := 0; i < generatedHeaderLines; i++ {
			line, err := lines.ReadString('\n')
			if generatedHeader.MatchString(strings.TrimRight(line, "\r\n")) {
				found = true
				break
			}
			if err != nil {
				break
			}
		}
	}
	// The rest of the object and its trailing newline, so the next answer
	// starts at its own header.
	if _, err := io.Copy(io.Discard, content); err != nil {
		return false, err
	}
	if _, err := r.out.Discard(1); err != nil {
		return false, err
	}
	return found, nil
}

func (r *blobHeaderReader) fail() {
	r.failed = true
	if r.cmd != nil {
		_ = r.cmd.Process.Kill()
	}
}

// Close ends the batch process, if one started.
func (r *blobHeaderReader) Close() {
	if r.cmd == nil {
		return
	}
	_ = r.in.Close()
	_ = r.cmd.Wait()
	r.cmd = nil
}

// perFileDiffCap bounds one binary file's section of a recorded diff. The
// producers read every file as text (--text, so an attribute cannot hide
// content), which would otherwise record a genuine binary byte for byte. A
// section over the cap that git would call binary (a NUL byte in its first 8000
// bytes) keeps its header, whose index line names the full blob ids, and drops
// the hunks. A text section is never capped: an oversize text diff stays whole, and a packet
// too large to load is refused rather than shown with a file missing.
const perFileDiffCap = 64 << 10

// binaryProbeBytes is how far into a file git looks for a NUL byte before it
// calls the file binary. The probe here reads the same window of the hunk text
// (a few bytes narrower than the file's, from the line prefixes), so a file git
// shows as text is never capped.
const binaryProbeBytes = 8000

// capFileDiffs replaces each binary file section over perFileDiffCap with its
// header and a marker. A diff with no such section comes back byte-identical.
func capFileDiffs(body string) string {
	if len(body) <= perFileDiffCap {
		return body
	}
	var out strings.Builder
	for _, s := range splitDiffSections(body) {
		if len(s.text) <= perFileDiffCap {
			out.WriteString(s.text)
			continue
		}
		// The header ends at the first hunk; a section with none (a mode or
		// rename header cannot be this long) is not capped here.
		header, _, hasHunks := strings.Cut(s.text, "\n@@")
		if !hasHunks {
			out.WriteString(s.text)
			continue
		}
		hunks := s.text[len(header):]
		if !looksBinary(hunks) {
			out.WriteString(s.text)
			continue
		}
		out.WriteString(header)
		adds, dels := countHunkLines(hunks)
		fmt.Fprintf(&out, "\n# === capped: +%d -%d, %d bytes of hunks for %s elided at the %d-byte per-file cap; "+
			"the index line above names the blobs ===\n", adds, dels, len(hunks), s.path, perFileDiffCap)
	}
	return out.String()
}

// cappedMarker matches the line capFileDiffs leaves for a capped section; it
// carries the added and removed line counts so the diff facts survive the cap.
// A line starting `#` is never diff content, so a file cannot forge one.
var cappedMarker = regexp.MustCompile(`^# === capped: \+(\d+) -(\d+), `)

// countHunkLines counts the added and removed lines in a section's hunks.
func countHunkLines(hunks string) (adds, dels int) {
	for _, line := range strings.Split(hunks, "\n") {
		switch {
		case strings.HasPrefix(line, "+"):
			adds++
		case strings.HasPrefix(line, "-"):
			dels++
		}
	}
	return adds, dels
}

// looksBinary reports whether git would call the file binary, from the hunk
// text alone: a NUL in the first binaryProbeBytes of the file. That is only
// readable from the diff when the first hunk starts at the top of both sides
// (`-1`/`-0` and `+1`/`+0`); then a line's offset in its file is no larger than
// its offset in the hunk, so a NUL found in the first hunk's window lies inside
// git's. A change that starts lower, or a NUL only in a later hunk, is not
// proof, and the section stays whole.
func looksBinary(hunks string) bool {
	hunks = strings.TrimPrefix(hunks, "\n")
	first, _, _ := strings.Cut(hunks, "\n@@")
	header, _, _ := strings.Cut(first, "\n")
	fields := strings.Fields(header)
	if len(fields) < 3 || fields[0] != "@@" {
		return false
	}
	top := func(rng, sign string) bool {
		return startsAtLineOne(rng, sign) || strings.HasPrefix(rng, sign+"0")
	}
	if !top(fields[1], "-") || !top(fields[2], "+") {
		return false
	}
	return strings.Contains(first[:min(len(first), binaryProbeBytes)], "\x00")
}
