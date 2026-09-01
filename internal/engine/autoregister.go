package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// AUTO-REGISTRATION — §9 item 11's machinery (docs/tdd/runs-dispatch.md §9).
//
// engine-spec §2's instance-config lifecycle, verbatim:
//
//	Instance files live in the repo at `.docket/config/` (`workflows/`,
//	`schemas/`, `contracts/`, `fragments/`, `templates/`, `policy.toml`) —
//	git-versioned like any code. … ACTIVATION AUTO-REGISTERS the config
//	directory's current contents (content-hash versioning) — registration is
//	NEVER A MANUAL STEP, and schemas are stable reviewed files, never generated
//	per-run.
//
// "Never a manual step" is the whole requirement, and §9 item 11 is its proof:
// from `workflow init --template` through a completed run, the only human inputs
// are conversational approvals. QA section ZK executes that literally and greps
// the command trace for `workflow register` / `schema register`. Zero hits is
// the criterion — a rehearsal that merely reaches `done` proves the engine
// works, while one that reaches `done` with the register verbs PROVEN ABSENT
// proves item 11.
//
// IT READS AND HASHES; IT EXECUTES NOTHING (§9.5, §1.3). A workflow found in
// `.docket/config/workflows/` that declares gates still requires trust entries
// for those gates, and its fenced commands are still harvested, snapshotted, and
// matched against the user-level allowlist. A malicious clone that ships
// `.docket/config/workflows/evil.toml` gets its definition REGISTERED and then
// gets every one of its gates reported `unmatched` and NOT EXECUTED. ZH10 is
// extended to carry a hostile config directory and prove exactly that, because
// "activation now registers a workflow it found in the repo" is precisely the
// sentence that sounds like drive-by execution and is not.

// Registration is one file the scan acted on — F21's wire row.
type Registration struct {
	// Kind is `schema` or `workflow`. The pinned-only files do not appear here;
	// they are counted (F20's `Pinned` line) rather than listed, because a
	// fragment tree can hold hundreds of files and none of them is a decision.
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	// Outcome is `new` or `unchanged`. F11's identical-bytes case is a SUCCESS
	// THAT CHANGES NOTHING, and reporting it as `unchanged` rather than omitting
	// it is what makes re-activation of an unedited repo legible: the operator
	// sees that the file was considered and found already registered.
	Outcome string `json:"outcome"`
}

const (
	// RegistrationKindSchema and RegistrationKindWorkflow are the only two
	// things core REGISTERS. Everything else under `.docket/config/` is PINNED,
	// which is exactly what §2 says core does with instance files it does not
	// understand.
	RegistrationKindSchema   = "schema"
	RegistrationKindWorkflow = "workflow"

	// RegistrationNew and RegistrationUnchanged are F21's `outcome` values.
	RegistrationNew       = "new"
	RegistrationUnchanged = "unchanged"
)

// configScan is what one activation's scan found: what to register, in order,
// and what to pin.
type configScan struct {
	// registrations are ordered by RegistrationOrder — schemas in full, then
	// everything else, lexically within each group (F1).
	paths []string
	// pins are the content-hash pins for every OTHER file under the config
	// directory (F4).
	pins []db.Pin
	// roots are the config directories the scan walked, IN PRECEDENCE ORDER
	// and already canonicalized — the subset of InstanceConfigDirs() that
	// actually exists.
	//
	// They are carried so a declared `packet` entry — which is RELATIVE to a
	// root — can be mapped onto a pin's ref, which is the path the walk
	// recorded (§1.2). Deriving them a second time at the point of use would
	// mean two places agreeing about where config lives.
	roots []string
}

// scanConfigDirs walks EVERY instance-config root, in precedence order, and
// sorts what they hold into one scan (§9.2).
//
// THE UNION IS THE POINT. `~/.docket/config/` carries the shared corpus and
// `<worktree>/.docket/config/` carries a repository's own additions; a run is
// activated against BOTH, so a repo that ships nothing still gets the corpus and
// a linked worktree with no `.docket/` at all resolves the same files as the
// checkout beside it. What the roots must never do is DISAGREE, so a
// `name@version` or a pinned ref that appears in two roots with different bytes
// refuses the whole activation (see crossRootConflictErr / pinConflictErr).
// That refusal is what makes "first root wins" a deterministic rule at
// resolution rather than a silent shadowing.
//
// F17 IS STILL THE FIRST THING IT DOES, now per root: a root that does not
// exist is skipped in full, and if NO root exists the scan returns nil and
// activation executes exactly the statements v9's did. That is D6's dormancy,
// and it stays structural — "a repo with no `.docket/config/` activates exactly
// as before" is a QA check (F19) because it is a property of these early exits.
//
// F5: the scan is NON-RECURSIVE within each registry directory and RECURSIVE for
// the pinned ones. A schema in a subdirectory of `schemas/` would break the
// two-group ordering's determinism argument — there would be no lexical order
// over a tree that agreed with the flat one — while a fragment three levels deep
// is just a file to hash.
func scanConfigDirs(roots []string) (*configScan, error) {
	scan := &configScan{}

	// The two registry groups accumulate SEPARATELY across roots, because F2's
	// order is global: EVERY schema registers before ANY workflow, or a
	// workflow in the repo root naming a schema from the shared corpus would
	// refuse on a `payload` that is about to exist.
	var schemas, workflows []string

	// Pins are deduplicated by REF — the config-relative path a `packet` entry
	// declares — not by absolute path, since the whole point is that two roots
	// can offer the same ref.
	pinnedRef := make(map[string]db.Pin)
	pinnedFrom := make(map[string]string)

	for _, configured := range roots {
		root, ok, err := resolveConfigRoot(configured)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if contains(scan.roots, root) {
			// Two configured roots that canonicalize to one directory are one
			// root. Scanning it twice would make every file its own duplicate.
			continue
		}
		scan.roots = append(scan.roots, root)

		var found []string
		registryDirs := map[string]bool{
			filepath.Join(root, "schemas"):   true,
			filepath.Join(root, "workflows"): true,
		}

		// F16: ONCE PER ACTIVATION, not once per issue. The walk is here, at the
		// top, and its result is passed down.
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}

			parent := filepath.Dir(path)
			if registryDirs[parent] {
				switch {
				case parent == filepath.Join(root, "schemas") && strings.EqualFold(filepath.Ext(path), ".json"),
					parent == filepath.Join(root, "workflows") && strings.EqualFold(filepath.Ext(path), ".toml"):
					found = append(found, path)
					return nil
				}
				// F6: a file whose extension does not match — `schemas/README.md` —
				// is SKIPPED SILENTLY IN THE REGISTRY and PINNED like any other
				// config file. A refusal here would make a README a run-blocker.
			}

			// F4: everything else is PINNED by content hash and REGISTERED AS
			// NOTHING. §2 is explicit that this is "how the reference instance pins
			// its contracts, fragments, and policy WITHOUT CORE KNOWING WHAT THEY
			// ARE", so core reads bytes, hashes them, records the path, and never
			// opens the content again.
			pin, err := filePin(root, path)
			if err != nil {
				return err
			}
			if prev, dup := pinnedRef[pin.Ref]; dup {
				if prev.SHA256 == pin.SHA256 {
					// The same file in both roots — an instance that vendored a
					// copy of what the corpus ships. Pin it once; there is no
					// ambiguity to report.
					return nil
				}
				return pinConflictErr(pinnedFrom[pin.Ref], path, pin.Ref)
			}
			pinnedRef[pin.Ref] = pin
			pinnedFrom[pin.Ref] = path
			scan.pins = append(scan.pins, pin)
			return nil
		})
		if err != nil {
			return nil, err
		}

		// F1: THE ORDERING IS NOT RE-IMPLEMENTED. The scan collects paths and
		// hands them to RegistrationOrder, which S5 landed against this stage's
		// signature precisely so the ordering contract lives in one place. It is
		// applied PER ROOT — lexical within a root — and the two groups are then
		// concatenated across roots, which is what keeps the global
		// schemas-before-workflows order while precedence stays root order.
		for _, path := range RegistrationOrder(found) {
			if isSchemaConfigPath(path) {
				schemas = append(schemas, path)
				continue
			}
			workflows = append(workflows, path)
		}
	}

	if len(scan.roots) == 0 {
		// F17: no root exists ⇒ the scan is skipped entirely.
		return nil, nil
	}

	scan.paths = append(schemas, workflows...)

	// The pins are ordered too, so two activations of one directory record them
	// identically and the golden diffs do not flap on filesystem walk order.
	sortPins(scan.pins)
	return scan, nil
}

// resolveConfigRoot classifies ONE configured root: skip it, scan it, or refuse.
//
// It CANONICALIZES before returning, because the installed shape of the shared
// corpus is a SYMLINK to a real directory and filepath.WalkDir does not follow
// symlinked roots — it would classify the link as a leaf and then fail in
// filePin's os.ReadFile with EISDIR. Resolving the root once here is the whole
// fix, and it is why the walk itself needs no symlink handling (a root with real
// directories behind one link walks correctly).
//
// The four outcomes:
//
//	absent                    skip, silently — dormancy (F17)
//	a regular file            VALIDATION_ERROR
//	a symlink to a directory  canonicalize and scan
//	a symlink resolving to    VALIDATION_ERROR naming the link AND its target
//	nowhere
//
// The last row is the one worth stating. A dangling symlink is DISTINGUISHABLE
// from plain absence — Lstat succeeds where Stat does not — and treating it as
// absence would turn a broken install into a run that quietly registers nothing
// and reports success, which is the failure mode dormancy is supposed to be the
// benign case of.
func resolveConfigRoot(root string) (string, bool, error) {
	if root == "" {
		return "", false, nil
	}

	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the config directory %s: %w", root, err)
	}

	if info.Mode()&fs.ModeSymlink != 0 {
		resolved, rerr := filepath.EvalSymlinks(root)
		if rerr != nil {
			target, _ := os.Readlink(root)
			return "", false, validationErr(
				"the instance-config root %s is a symlink to %q, which does not "+
					"resolve (%v); repair the link or remove it — a broken link is "+
					"not the same as having no config, so activation refuses rather "+
					"than registering nothing and reporting success",
				root, target, rerr)
		}
		root = resolved
		if info, err = os.Stat(root); err != nil {
			return "", false, fmt.Errorf("reading the config directory %s: %w", root, err)
		}
	} else if resolved, rerr := filepath.EvalSymlinks(root); rerr == nil {
		// A real directory can still sit behind a symlinked ANCESTOR (macOS
		// resolves /var to /private/var). Canonicalizing every root means the
		// paths the scan records and the paths resolution joins agree.
		root = resolved
	}

	if !info.IsDir() {
		// A FILE named `config` is not a config directory. Refusing beats
		// guessing: an operator who created one meant something, and silently
		// ignoring it would make the omission invisible.
		return "", false, validationErr(
			"%s is not a directory; an instance-config root holds the instance's "+
				"workflows, schemas, and pinned files", root)
	}
	return root, true, nil
}

// instanceConfigRoots is the RESOLUTION-side twin of the scan's root list: the
// same ordered roots, canonicalized the same way, for the readers that map a
// declared ref onto a file (packet composition).
//
// It is best-effort where the scan is strict. Activation is where a broken root
// is a refusal an operator can act on; a render reading a pinned file has
// already had that check run for it, and a root that will not resolve simply
// holds none of the refs it is asked for.
func instanceConfigRoots() []string {
	dirs := resolvePaths().InstanceConfigDirs()
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		if contains(out, dir) {
			continue
		}
		out = append(out, dir)
	}
	return out
}

// pinConflictErr refuses an AMBIGUOUS REF: one config-relative path offered by
// two roots with different bytes.
//
// A ref names exactly one file — it is what a `packet` entry declares and what
// the pin row records — so two candidates is not a precedence question core is
// entitled to answer. Choosing the first root would mean an operator's repo-side
// edit silently loses to the corpus (or silently wins over it), discovered later
// as a packet whose contents nobody can account for. Refusing puts the decision
// where §2 puts every decision: with the human, before the run exists.
func pinConflictErr(first, second, ref string) error {
	return conflictErr(
		"the instance-config file %q is present in two config roots with "+
			"different bytes:\n\n  %s\n  %s\n\n"+
			"A ref names one file, so activation refuses rather than choosing "+
			"which root wins. Make the two copies identical, or delete the one "+
			"you did not mean to keep.", ref, first, second)
}

// crossRootConflictErr is collisionErr's cross-root twin: ONE `name@version`
// defined by two roots with different bytes.
//
// The single-root case (F9) is "you edited a registered definition"; this one is
// "the corpus and the repository disagree about what this definition IS", and it
// names BOTH files because neither is more authoritative than the other and the
// operator has to look at both to decide.
func crossRootConflictErr(first, second, name string, version int) error {
	return conflictErr(
		"%s@%d is defined in two config roots with different bytes:\n\n"+
			"  %s\n  %s\n\n"+
			"A registered name@version is frozen so that a run which pinned it "+
			"can reproduce, so activation refuses rather than choosing which "+
			"root wins. Make the two definitions identical, or give one of them "+
			"its own version.", name, version, first, second)
}

// registerScanTx registers what the scan found, IN ORDER, inside activation's
// fat transaction (F8).
//
// F8 IS THE LOAD-BEARING PLACEMENT: this runs INSIDE the transaction, BEFORE
// binding, so a registration failure refuses the WHOLE activation and writes
// nothing. Registering-then-failing would leave a repo carrying definitions from
// a run that never started — and the next activation would then bind against
// them, which is a run inheriting the debris of a failed one.
//
// F7: it reuses the EXISTING register paths — the same validation, the same
// immutability contract, the same CONFLICT on different bytes. THERE IS NO
// "AUTO" VARIANT WITH LOOSER RULES, because a looser rule here is a rule that
// only applies to definitions nobody typed a command for, which is exactly
// backwards.
func registerScanTx(tx *sql.Tx, projectID int, scan *configScan, nowMS int64) ([]Registration, error) {
	if scan == nil || len(scan.paths) == 0 {
		return nil, nil
	}

	out := make([]Registration, 0, len(scan.paths))

	// ONE IDENTITY, ONE REGISTRATION — the union's rule, applied here because
	// here is where the identity is known. A schema's identity is its filename
	// and a workflow's is inside its body, so the scan (which reads no content
	// beyond hashing it) cannot tell two roots' copies apart; this loop can.
	seen := make(map[string]Registration, len(scan.paths))

	for _, path := range scan.paths {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, validationErr("reading %s: %v", path, err)
		}

		key, keyed := registryIdentityKey(path, src)
		if keyed {
			if prev, dup := seen[key]; dup {
				if prev.SHA256 == workflow.SHA256(src) {
					// Byte-identical in both roots: a NO-OP, and reported once.
					// A second `unchanged` row would describe a decision the
					// operator did not make.
					continue
				}
				return nil, crossRootConflictErr(prev.Path, path, prev.Name, prev.Version)
			}
		}

		var reg Registration
		if isSchemaConfigPath(path) {
			reg, err = registerConfigSchemaTx(tx, projectID, path, src, nowMS)
		} else {
			reg, err = registerConfigWorkflowTx(tx, projectID, path, src, nowMS)
		}
		if err != nil {
			return nil, err
		}
		if keyed {
			seen[key] = reg
		}
		out = append(out, reg)
	}
	return out, nil
}

// registryIdentityKey derives the `kind + name@version` a config file registers
// under, WITHOUT registering it.
//
// A file whose identity cannot be read — a schema misnamed for the reference it
// registers, a workflow that does not parse — returns false rather than an
// error. The refusal for those belongs to registerConfigSchemaTx /
// registerConfigWorkflowTx, which already word it precisely; a second refusal
// here would be the same failure reported twice, worse.
func registryIdentityKey(path string, src []byte) (string, bool) {
	kind, name, version, ok := configIdentity(path, src)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s\x00%s@%d", kind, name, version), true
}

// configIdentity is registryIdentityKey's three parts, unjoined: the KIND a
// config file registers as, and the NAME and VERSION it registers under.
//
// It exists because the cross-project audit (registry_audit.go) needs the
// version as a NUMBER to compare against a registered one, and re-deriving
// "where does a config file's identity come from" a second time is exactly how
// the audit and registration would come to disagree about what the corpus
// declares. Both callers read it from here, so they cannot.
//
// `src` is consulted ONLY for a workflow — a schema's identity is its filename
// — so a caller auditing a corpus may pass nil for a schema path rather than
// reading a document it will not parse.
func configIdentity(path string, src []byte) (kind, name string, version int, ok bool) {
	if isSchemaConfigPath(path) {
		base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		name, version, err := workflow.ParsePayloadRef(base)
		if err != nil {
			return "", "", 0, false
		}
		return RegistrationKindSchema, name, version, true
	}
	def, err := workflow.Parse(src)
	if err != nil {
		return "", "", 0, false
	}
	return RegistrationKindWorkflow, def.Pipeline.Name, def.Pipeline.Version, true
}

// registerConfigSchemaTx registers one `.docket/config/schemas/NAME@V.json`.
//
// THE IDENTITY COMES FROM THE FILENAME, because auto-registration has no
// argument to carry it. `docket schema register findings@1 findings.json` takes
// `name@version` as an argument; a scan has only the file. The repo's own
// convention already resolves this — the builtin ships as `aggregate@1.json` and
// every fixture is `findings@1.json` — so the filename IS the reference, and the
// same ParsePayloadRef grammar validates it. A file that does not follow the
// convention is refused by name rather than registered under a guessed identity.
func registerConfigSchemaTx(
	tx *sql.Tx, projectID int, path string, src []byte, nowMS int64,
) (Registration, error) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	name, version, err := workflow.ParsePayloadRef(base)
	if err != nil {
		return Registration{}, validationErr(
			"%s: a schema in `.docket/config/schemas/` is named for the reference "+
				"it registers, as `name@version.json` (for example `findings@1.json`); "+
				"%q is not one", path, base)
	}

	// F7: the SAME compilation `schema register` performs. A schema that does
	// not compile is refused while an author is looking at it, and activation is
	// when an author is looking at it.
	compiled, err := schema.Compile(name, version, src)
	if err != nil {
		return Registration{}, validationErr("%s: %v", path, err)
	}
	ordered, err := json.Marshal(compiled.Ordered)
	if err != nil {
		return Registration{}, fmt.Errorf("encoding the ordered index for %s: %w", path, err)
	}

	sum := workflow.SHA256(src)
	_, created, err := db.InsertSchemaTx(tx, &model.Schema{
		ProjectID: projectID,
		Name:      name, Version: version,
		SourcePath: path, SourceSHA256: sum,
		Body: string(src), Ordered: string(ordered),
	}, nowMS)
	if err != nil {
		return Registration{}, collisionErr(err, path, name, version)
	}

	return Registration{
		Kind: RegistrationKindSchema, Name: name, Version: version,
		Path: path, SHA256: sum, Outcome: outcomeOf(created),
	}, nil
}

// registerConfigWorkflowTx registers one `.docket/config/workflows/*.toml`.
//
// The identity comes from the definition's own `[pipeline] name`/`version`,
// which is where `workflow register` reads it from too — a workflow declares its
// identity in its body, so the filename is provenance rather than a key.
func registerConfigWorkflowTx(
	tx *sql.Tx, projectID int, path string, src []byte, nowMS int64,
) (Registration, error) {
	def, err := workflow.Parse(src)
	if err != nil {
		return Registration{}, validationErr("%s: %v", path, err)
	}
	if err := workflow.Validate(def); err != nil {
		return Registration{}, validationErr("%s: %v", path, err)
	}

	// V26 and V21a-V21d/V25a, exactly as `workflow register` runs them (F7).
	// THE SCHEMA CHECK IS WHY THE ORDER MATTERS: this refuses a `payload` that
	// is not registered, and it is the refusal §9.1's F2/F3 pair is about. The
	// schemas registered ahead of this loop are what make it pass.
	if err := workflow.ValidateVoteRules(def, txVoteRuleResolver{tx, projectID}); err != nil {
		return Registration{}, validationErr("%s: %v", path, err)
	}
	if err := workflow.ValidateSchemas(def, txSchemaResolver{tx, projectID}); err != nil {
		return Registration{}, validationErr("%s: %v", path, err)
	}

	parsed, err := workflow.Canonical(def)
	if err != nil {
		return Registration{}, fmt.Errorf("rendering %s: %w", path, err)
	}

	sum := workflow.SHA256(src)
	_, created, err := db.InsertWorkflowTx(tx, &model.Workflow{
		ProjectID:   projectID,
		Name:        def.Pipeline.Name,
		Version:     def.Pipeline.Version,
		Description: def.Pipeline.Description,
		SourcePath:  path, SourceSHA256: sum,
		Body: string(src), Parsed: string(parsed),
	}, nowMS)
	if err != nil {
		return Registration{}, collisionErr(err, path, def.Pipeline.Name, def.Pipeline.Version)
	}

	return Registration{
		Kind: RegistrationKindWorkflow, Name: def.Pipeline.Name,
		Version: def.Pipeline.Version, Path: path, SHA256: sum,
		Outcome: outcomeOf(created),
	}, nil
}

// collisionErr is F9, F10, F12, and F13: CHANGED BYTES AT AN UNCHANGED
// `name@version` REFUSES THE WHOLE ACTIVATION, with a conversationally
// actionable message.
//
// §9.3 examined four options and chose this one. The argument, briefly, because
// it will be re-litigated:
//
//   - AUTO-BUMP would mint `name@N+1` from bytes nobody approved as a new
//     version, so the NEXT run silently binds a definition the operator never
//     named. Version numbers would stop meaning "the author decided this is a
//     new version".
//   - OVERWRITE destroys immutability outright: a run that pinned `name@N` by
//     hash can no longer reproduce, and §9 item 5 fails on its face.
//   - IGNORE is the worst of the set — the operator edits a file, activates, and
//     gets the OLD definition with no message.
//
// AGAINST THE ZERO-TOUCH TENET (T9), which is the real tension: T9's zero-touch
// is about who DOES THE WORK, not about the machine never asking. The prescribed
// loop is that the session drafts config, the human approves in conversation, and
// the session runs the commands. A message a session reads, acts on, and brings
// to the human as "I changed the workflow; it needs a version bump to 2 — okay?"
// is THAT LOOP WORKING. What T9 forbids is the human hand-editing config or
// typing `docket workflow register`, and this asks for neither.
//
// CORE NEVER AUTO-BUMPS because the bump is a DECISION, and §2's division puts
// decisions with the human and mechanics with the session. A core that bumped
// would be making the decision and telling nobody.
//
// F13: it is a HARD refusal, not a warning. A warning during a fat transaction
// that then proceeds is a warning nobody reads until the run behaves oddly.
func collisionErr(err error, path, name string, version int) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, db.ErrWorkflowConflict) && !errors.Is(err, db.ErrSchemaConflict) {
		return err
	}

	registered, current := hashesFromConflict(err)

	// F10: the message names the PATH, BOTH HASHES, the REGISTERED VERSION, and
	// the LITERAL EDIT — and states that existing pinned runs are unaffected,
	// because "your edit is refused" without that sentence reads as "you have
	// broken something" rather than "this needs a version number".
	return conflictErr(
		"%s has changed since it was registered as %s@%d.\n\n"+
			"  registered  sha256:%s\n  current     sha256:%s\n\n"+
			"A registered name@version is frozen so that a run which pinned it can "+
			"reproduce. To adopt these changes, bump the definition's version to %d, "+
			"then activate again. Runs already pinned to %s@%d are unaffected.",
		path, name, version, registered, current, version+1, name, version)
}

// hashesFromConflict pulls the two hashes out of the storage layer's conflict
// message, which already names both.
//
// It parses rather than re-reading the row because the storage error IS the
// authority on what collided — a second read could race a concurrent
// registration and report a third hash that neither side saw.
func hashesFromConflict(err error) (registered, current string) {
	msg := err.Error()
	fields := strings.Fields(msg)
	var hashes []string
	for _, f := range fields {
		f = strings.Trim(f, ",.")
		if len(f) == 64 && isHex(f) {
			hashes = append(hashes, f)
		}
	}
	if len(hashes) >= 2 {
		return short(hashes[0]), short(hashes[1])
	}
	if len(hashes) == 1 {
		return short(hashes[0]), "unknown"
	}
	return "unknown", "unknown"
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// short renders a hash the way every other operator-facing hash in the repo is
// rendered: enough to compare by eye, not enough to fill the line.
func short(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12] + "…"
}

// outcomeOf maps the registry's `created` flag onto F21's vocabulary.
//
// F11: identical bytes at the same `name@version` is a SUCCESS THAT CHANGES
// NOTHING — the existing contract, and the case that makes re-activation of an
// unedited repo free (C10). Two activations scanning one directory both register
// identical bytes and both succeed; the loser changes nothing.
func outcomeOf(created bool) string {
	if created {
		return RegistrationNew
	}
	return RegistrationUnchanged
}

// filePin hashes one config file for F4's pin.
//
// The pin's REF is RELATIVE TO ITS CONFIG ROOT (v12) — `contracts/fix.md`,
// not the walked absolute path. An absolute ref bound the pin to one checkout:
// under the shared store the config tree lives with the repository, so a run
// activated in one worktree and resumed (or rendered) in another would find
// every pin "not pinned by this run". The relative form is the one path all
// checkouts of a project agree on, and it is also what a workflow's `packet`
// entry already declares, so entry and ref finally speak the same language.
// A path outside the root keeps its walked form — refusing would make an odd
// symlink a run-blocker for files core deliberately does not understand.
func filePin(root, path string) (db.Pin, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return db.Pin{}, validationErr("reading the pinned config file %s: %v", path, err)
	}
	ref := path
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		ref = rel
	}
	return db.Pin{
		Kind: db.PinKindFile, Ref: ref, SHA256: workflow.SHA256(src),
		// Carried in memory for the closure-size arithmetic (§1.5). The
		// bytes are already in hand for the hash, so the size is free here and
		// would cost a second read at expansion.
		Bytes: len(src),
	}, nil
}

// sortPins puts the pin rows in a total order by ref, so two activations over
// one directory record them identically.
//
// sort.Slice rather than sort.SliceStable: the scan dedupes pins BY REF
// (scanConfigDirs' `pinnedRef` map) before they ever reach `scan.pins`, so no
// two pins here can compare equal and there is no tie for stability to
// preserve.
func sortPins(pins []db.Pin) {
	sort.Slice(pins, func(i, j int) bool { return pins[i].Ref < pins[j].Ref })
}

// RenderRegistrationReport is F20: the human-mode `Registered` block.
//
// One line per file, `<name>@<version>  <path>  (new | unchanged)`, in
// REGISTRATION ORDER — schemas first, then workflows — followed by a `Pinned`
// count. The order is the scan's own, not a re-sort, so what an operator reads
// is the sequence that actually ran.
//
// F23: an activation that registered NOTHING prints no block at all — not an
// empty one — so F17's dormancy is visible in the output. A repo with no
// `.docket/config/` produces exactly the activation output v9 produced.
//
// THE PATH AND THE NAME GO THROUGH THE ESCAPER (T18). Both come from the repo's
// filesystem and a workflow's own `[pipeline] name`, which in a cloned repo are
// attacker-supplied strings heading for a TERMINAL — the same class as a fence
// command, and closed the same way. That matters most in exactly the case §9.5
// is about: a malicious clone whose config directory this block is describing.
func RenderRegistrationReport(
	w interface{ Write([]byte) (int, error) }, regs []Registration, pinned int,
) {
	if len(regs) == 0 {
		// The `Pinned` line rides with the block rather than standing alone: a
		// bare "pinned 3 files" with nothing registered would describe a scan
		// the operator has no other evidence ran.
		return
	}
	fmt.Fprintf(w, "\nregistered from the instance config (%d):\n", len(regs))
	for _, r := range regs {
		fmt.Fprintf(w, "  %s@%d  %s  (%s)\n",
			exec.Render(r.Name), r.Version, exec.Render(r.Path), r.Outcome)
	}
	if pinned > 0 {
		fmt.Fprintf(w, "  pinned %d further config file(s) by content hash\n", pinned)
	}
}

// txSchemaResolver and txVoteRuleResolver adapt the two registries to the
// validators' resolver interfaces, inside a transaction.
//
// They mirror internal/cli's `schemaResolver` and `voteRuleResolver` exactly —
// same queries, same refusals, same compilation of stored bytes. The duplication
// is the transaction, not the logic: F7 requires auto-registration to run the
// SAME validation `workflow register` runs, and the only difference is the
// handle it runs it against.
//
// The schema resolver COMPILES the stored bytes rather than reading a cached
// parse, because V21a-V21d ask about the document's declared fields and their
// orders, and the registered bytes are the only authority on those.
type txSchemaResolver struct {
	tx *sql.Tx
	// projectID scopes resolution: a workflow's `payload` reference resolves
	// against ITS project's registry (plus builtins), never a neighbor's.
	projectID int
}

func (r txSchemaResolver) Schema(name string, version int) (*workflow.Registered, error) {
	row, err := db.GetSchemaTx(r.tx, r.projectID, name, version)
	if errors.Is(err, db.ErrSchemaNotFound) {
		return nil, fmt.Errorf("%w: %s@%d", workflow.ErrNotRegistered, name, version)
	}
	if err != nil {
		return nil, err
	}
	return schema.Compile(row.Name, row.Version, []byte(row.Body))
}

type txVoteRuleResolver struct {
	tx *sql.Tx
	// projectID scopes rule resolution (v12): the run's project sees its own
	// rules plus the store-wide ones.
	projectID int
}

func (r txVoteRuleResolver) VoteRuleExists(rule string) (bool, error) {
	return db.VoteRuleExistsTx(r.tx, r.projectID, rule)
}

func (r txVoteRuleResolver) RuleSetElsewhere(rule string) (int, string, error) {
	return db.VoteRuleSetElsewhereTx(r.tx, r.projectID, rule)
}

func (r txVoteRuleResolver) RegisteredVoteRules() ([]string, error) {
	return db.RegisteredVoteRulesTx(r.tx, r.projectID)
}
