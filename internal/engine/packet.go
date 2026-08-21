package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// PACKET COMPOSITION — the resolution half
// (docs/tdd/packet-composition.md §1.2, §1.3, §1.4).
//
// engine-core §8 enumerates the assembled packet: framing header, THE STEP'S
// DECLARED FILE AT ITS PINNED VERSION, THE FILES ITS FRONTMATTER DECLARES, the
// input artifacts, the output instruction. Items 2 and 3 were absent entirely —
// `== PINNED` emitted a path and a checksum, so an executor received a filename
// for the document it was supposed to work from, and §8's own property ("nothing
// in it is optional — no pointers, no 'read if needed'") was inverted by the
// shipped template.
//
// ASSEMBLY STAYS SNAPSHOT-PINNED. Bytes are admitted only when they hash to what
// activation recorded, so "same step => same packet, byte-identical, even
// mid-run" survives: a post-activation edit is a CONFLICT naming both hashes,
// never a silently different packet. That is the same discipline
// `templateSource` applies to `--template F`, applied to a second class of file
// on purpose — it is already this repo's answer to reading a pinned file safely.
//
// CORE READS BYTES AND ONE DECLARED KEY. It never interprets what a file says,
// never treats a directory as special, and never derives a path it was not
// given.

// PacketFile is one resolved file: where it came from, what it hashed to, and
// its bytes.
//
// The path and hash ride along with the body so provenance survives INTO the
// rendered packet. A worker reading a composed document should be able to say
// which file a passage came from, and an operator reproducing a run should be
// able to check it.
type PacketFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Body   string `json:"body"`
}

// packetIncludesKey is the ONE frontmatter key any engine code reads.
//
// The design review approved this as mechanism rather than interpretation, and
// the boundary is the whole argument: core learns that a file may open with a
// `---` block and that ONE key inside it holds paths to more files for the same
// packet — a generic declared-includes mechanism, the same idea as an import
// list. Every other key is IGNORED ENTIRELY: not validated, not surfaced, not
// errored on. An instance's own frontmatter passes through untouched, so core
// never has an opinion about it.
//
// It is authored with EXPLICIT PATHS, never bare names to expand against a
// directory — expanding names would be exactly the convention knowledge the
// `packet` field exists to keep out of core.
const packetIncludesKey = "packet_includes"

// packetFrontmatterFence delimits a frontmatter block.
const packetFrontmatterFence = "---"

// resolvePacketFiles resolves a step's declared entries to verified bytes.
//
// Order is fully determined (§1.4): declared order, and after each entry
// immediately its own includes in declared order. Deterministic and stable,
// which R9 requires and the §9-item-5 goldens assert.
//
// De-duplication: a file reachable twice — declared directly and via an include,
// or via two includes — is inlined ONCE, at its first position. Repeating it
// would inflate the closure for no gain.
//
// DEPTH IS EXACTLY ONE. Includes do not themselves include. That is a hard stop
// rather than a limitation to lift later: unbounded recursion needs cycle
// detection and a depth cap, and every level makes the closure size harder for
// an author to predict at the moment the §11.1 caps need it predictable. One
// level matches engine-core §8's structure precisely — the file, and the files
// it declares.
//
// A declared entry is RELATIVE to an instance-config root, while a pin's ref is
// the path the activation scan walked. `roots` is what maps between them, so the
// workflow author writes `checklists/proofing.md` and the run's pin set is
// consulted at the path activation actually recorded.
//
// THE ROOTS ARE A LIST, tried in precedence order (shared corpus first, then the
// repository's own additions), and the first one holding a ref wins. That is
// deterministic rather than arbitrary because activation REFUSES a ref two roots
// offer with different bytes — so a ref resolves to one file's contents no
// matter which root it came from. It is also what makes a packet portable: an
// entry living in the shared root resolves identically from any cwd, including a
// linked worktree that carries no `.docket/` of its own.
func resolvePacketFiles(
	pins map[string]string, roots []string, entries []string,
) ([]PacketFile, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	var out []PacketFile
	seen := make(map[string]bool, len(entries))

	// add resolves one file and appends it, returning the includes it declares.
	// A file already inlined returns no includes, which is what makes the
	// de-duplication also terminate the one-level walk on a diamond.
	add := func(ref string) ([]string, error) {
		if seen[ref] {
			return nil, nil
		}
		seen[ref] = true

		body, hash, err := readPinnedPacketFile(pins, roots, ref)
		if err != nil {
			return nil, err
		}
		includes, stripped, err := parsePacketFrontmatter(ref, body)
		if err != nil {
			return nil, err
		}
		out = append(out, PacketFile{Path: ref, SHA256: hash, Body: stripped})
		return includes, nil
	}

	for _, entry := range entries {
		includes, err := add(entry)
		if err != nil {
			return nil, err
		}
		// ONE LEVEL, AND NO FURTHER: an include's own declared includes are
		// parsed (so a malformed one still refuses) and then discarded.
		for _, include := range includes {
			if _, err := add(include); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// readPinnedPacketFile is §1.2's ladder for one file.
//
//	pinned, bytes match    inlined
//	pinned, bytes differ   CONFLICT (exit 4), BOTH hashes named
//	pinned, file absent    NOT_FOUND (exit 2) — the pin says the run needs it
//	NOT PINNED             VALIDATION_ERROR — the run holds no snapshot
//
// The last row is where this ladder DIFFERS from `templateSource`'s, and the
// difference is deliberate. An unpinned `--template F` renders unverified and
// reports the gap, because the operator chose that file at render time. A
// packet entry is chosen at AUTHORING time and pinned at activation, so an
// unpinned one means the file was not in the config directory when the run
// activated — reading the live tree would break the byte-identical property
// outright. Refusing is the only answer consistent with §8's Properties clause.
func readPinnedPacketFile(
	pins map[string]string, roots []string, ref string,
) (body, hash string, err error) {
	candidates := make([]string, 0, len(roots))
	for _, root := range roots {
		candidates = append(candidates, filepath.Join(root, ref))
	}

	// v12 pins key by the config-relative ref — the same string the packet
	// entry declares, and the one path every checkout of a project agrees on.
	// Runs activated before the change recorded the full walked path, so the
	// absolute form is honored second, at each root; a legacy run keeps
	// resolving in the checkout it was activated in.
	pinnedHash, pinned := pins[ref]
	if !pinned {
		for _, full := range candidates {
			if h, ok := pins[full]; ok {
				pinnedHash, pinned = h, true
				break
			}
		}
	}
	if !pinned {
		return "", "", validationErr(
			"packet file %q is not pinned by this run; a packet reads only files the "+
				"run snapshotted at activation, so add it under an instance-config "+
				"root and start a new run", ref)
	}

	// FIRST ROOT THAT HOLDS IT WINS, and the hash check below then applies to
	// that file. Falling through to a later root on a hash mismatch would let a
	// stale copy stand in for an edited one and turn the CONFLICT into a
	// silently different packet.
	var content []byte
	var missing error
	var found bool
	for _, full := range candidates {
		read, rerr := os.ReadFile(full)
		if rerr == nil {
			content, found = read, true
			break
		}
		if errors.Is(rerr, os.ErrNotExist) {
			missing = rerr
			continue
		}
		return "", "", fmt.Errorf("reading packet file %s: %w", ref, rerr)
	}
	if !found {
		if missing == nil {
			missing = os.ErrNotExist
		}
		// The pin says the run depends on this file. Its absence is a
		// missing dependency, not a bad request.
		return "", "", notFoundErr(missing,
			"packet file %q is pinned by this run but is no longer on disk", ref)
	}

	// A pin recorded before this stage carries no hash to compare against
	// (RA2's inherited pins). Such a file resolves rather than refusing: it was
	// legal when the run started, and re-activation must not reject a run for
	// gaining a check it predates.
	got := sha256Hex(content)
	if pinnedHash != "" && got != pinnedHash {
		// Never a warning, never a silent re-pin. Both hashes are named because
		// an operator needs to know whether to restore the file or re-activate.
		return "", "", conflictErr(
			"packet file %q has changed since this run pinned it: pinned %s, on disk "+
				"%s; restore the file or start a new run", ref, pinnedHash, got)
	}
	return string(content), got, nil
}

// parsePacketFrontmatter reads the one declared-includes key and returns the
// body with the frontmatter block stripped.
//
// WHERE IT REFUSES, AND WHY HERE. These files are PINNED, NOT REGISTERED:
// activation hashes them and never opens them, so activation has no parse step
// to refuse in, and inventing one would mean opening pinned content at
// activation purely to validate an instance file's syntax. So a malformed block
// refuses at claim/render time — the same pass as pin verification, which is
// when the bytes are legitimately opened and hash-checked.
//
// FAIL CLOSED, ALWAYS. Never a warning, never a skip, never a
// partially-composed packet. A packet that silently omitted a declared include
// would reproduce the original failure signature — an instance doing
// everything right, observing nothing, concluding it was itself at fault — one
// level deeper and harder to find.
//
// A file with no frontmatter, or with frontmatter that simply does not carry
// the key, is NOT an error: its body inlines and its other keys are ignored.
func parsePacketFrontmatter(ref, body string) (includes []string, stripped string, err error) {
	rest, ok := strings.CutPrefix(body, packetFrontmatterFence+"\n")
	if !ok {
		return nil, body, nil
	}

	end := strings.Index(rest, "\n"+packetFrontmatterFence+"\n")
	if end < 0 {
		// A trailing fence with no newline after it still closes the block —
		// a file that is nothing but frontmatter is legal.
		if trimmed, cut := strings.CutSuffix(rest, "\n"+packetFrontmatterFence); cut {
			rest, end = trimmed+"\n"+packetFrontmatterFence+"\n", len(trimmed)
		} else {
			return nil, "", validationErr(
				"packet file %q opens a `%s` frontmatter block that is never closed",
				ref, packetFrontmatterFence)
		}
	}

	block := rest[:end]
	stripped = rest[end+len("\n"+packetFrontmatterFence+"\n"):]

	includes, err = parsePacketIncludes(ref, block)
	if err != nil {
		return nil, "", err
	}
	return includes, stripped, nil
}

// parsePacketIncludes extracts the one key from a frontmatter block.
//
// It reads a deliberately SMALL subset of YAML — `key:` followed by `- item`
// lines — rather than pulling in a parser. The engine has exactly one key to
// read, and a full YAML dependency would let core start reading structure it
// has no business reading. Any OTHER key is skipped without inspection, which
// is what makes the concession bounded.
func parsePacketIncludes(ref, block string) ([]string, error) {
	lines := strings.Split(block, "\n")

	for i := 0; i < len(lines); i++ {
		key, value, ok := strings.Cut(lines[i], ":")
		if !ok || strings.TrimSpace(key) != packetIncludesKey {
			continue
		}

		// `packet_includes: a.md` — a scalar where a list belongs. Refused
		// rather than accepted-as-one-item: a shape that silently means
		// something is how two authors write the same file two ways.
		if strings.TrimSpace(value) != "" {
			return nil, validationErr(
				"packet file %q declares `%s` as a scalar; it must be a list of "+
					"paths, one `- path` per line", ref, packetIncludesKey)
		}

		var out []string
		for j := i + 1; j < len(lines); j++ {
			line := strings.TrimSpace(lines[j])
			if line == "" {
				continue
			}
			item, isItem := strings.CutPrefix(line, "-")
			if !isItem {
				break // the next key ends the list
			}

			entry := strings.TrimSpace(item)
			entry = strings.Trim(entry, `"'`)
			if entry == "" {
				return nil, validationErr(
					"packet file %q declares an empty `%s` entry", ref, packetIncludesKey)
			}
			if err := checkPacketRef(ref, entry); err != nil {
				return nil, err
			}
			out = append(out, entry)
		}
		return out, nil
	}
	return nil, nil
}

// checkPacketRef is V32's containment rule applied to an INCLUDE.
//
// A declared `packet` entry is checked at register time, but an include is
// authored in a file core does not register — so the same rule is enforced
// where the include is read. Without it, the containment guarantee would hold
// for the workflow author and not for the corpus author.
func checkPacketRef(ref, entry string) error {
	normalized := strings.ReplaceAll(entry, `\`, "/")
	if strings.HasPrefix(normalized, "/") || strings.HasPrefix(normalized, "~") {
		return validationErr(
			"packet file %q declares the include %q, which is not a relative path "+
				"inside the instance-config directory", ref, entry)
	}
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return validationErr(
				"packet file %q declares the include %q, which escapes the "+
					"instance-config directory with `..`", ref, entry)
		}
	}
	return nil
}

// sha256Hex is workflow.SHA256, named locally so this file reads as one story.
func sha256Hex(content []byte) string { return workflow.SHA256(content) }

// packetPinsForRun indexes a run's FILE pins by ref, for the resolver to verify
// against. Registered-object pins are excluded: a workflow or schema pin names
// a version, not a path, and a packet entry naming one would be a path
// collision rather than a reference.
func packetPinsForRun(pins []db.Pin) map[string]string {
	out := make(map[string]string, len(pins))
	for _, p := range pins {
		if p.Kind == db.PinKindFile {
			out[p.Ref] = p.SHA256
		}
	}
	return out
}

// stepPacketFiles resolves one step's declared packet files for rendering.
//
// The substitution is RE-DERIVED here rather than persisted on the step row,
// which is what keeps this whole patch free of a schema change: the pinned
// definition supplies the declared entries and the step row supplies the
// executor hint that expansion substituted, so the same inputs produce the same
// list. A stored copy would be a second source of one fact, and the two could
// disagree after a re-activation.
//
// `executor` is a RESOLVED hint overriding the declared one (DKT-70), "" for
// the declared behavior — see RenderStepAs.
func stepPacketFiles(
	conn *sql.DB, step *db.Step, spec *workflow.Step, executor string,
) ([]PacketFile, error) {
	if spec == nil || len(spec.Packet) == 0 {
		return nil, nil
	}
	hint := executor
	if hint == "" {
		hint = step.Executor
	}

	entries := make([]string, 0, len(spec.Packet))
	for _, entry := range spec.Packet {
		entries = append(entries, workflow.SubstitutePacketEntry(entry, hint))
	}

	pins, err := db.ListPins(conn, step.RunID)
	if err != nil {
		return nil, err
	}

	return resolvePacketFiles(packetPinsForRun(pins), instanceConfigRoots(), entries)
}
