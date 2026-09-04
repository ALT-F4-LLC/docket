package engine

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// CROSS-PROJECT REGISTRY DRIFT (DKT-614): auto-registration adopts the corpus's
// current contents, but ONLY as a side effect of activating a run IN THAT
// PROJECT. A project nobody has activated lately goes stale against a corpus
// every other project already moved past, and nothing says so.
//
// The measured state that produced this: of eleven projects sharing one store,
// exactly one carried every workflow at its current corpus version. Seven were
// several versions behind across most names and carried no registration at all
// for the two newest ones — and the only way to learn that was to cd into each
// checkout in turn, or to query sqlite directly.
//
// This file answers the question ONCE FOR THE WHOLE STORE. The corpus is
// SHARED — `~/.docket/config` is not per-project — so what "current" means is
// computed from ONE scan and then compared against every project's rows, rather
// than rescanned eleven times to get eleven identical answers.
//
// IT IS A SIBLING OF source_drift.go AND orphan_registration.go, NOT A
// REPLACEMENT for either:
//
//   - CheckWorkflowSource asks whether THE FILE ONE ROW POINTS AT still holds
//     the registered bytes. That is a hash question about a path.
//   - WorkflowOriginIndex asks whether ANY file still declares a registered
//     NAME. That is an existence question about a name, and this file asks the
//     same one — of both registries, for every project at once.
//   - This file adds the third question neither answers: is the version this
//     project registered for that name the version the corpus NOW DECLARES?
//     That is a comparison of two NUMBERS, and a hash check cannot stand in for
//     it: a project sitting on investigation@4 while the corpus declares
//     investigation@8 has a row whose own source file is long gone, so the
//     hash question returns `unreadable` and says nothing about the eight.
//
// AND IT REPAIRS NOTHING. Adopting a bumped definition is what activation does,
// deliberately and inside a transaction, with the validation and the collision
// rules that go with it. A report that quietly registered rows on the way past
// would be doing an activation's work with none of an activation's guarantees.

// CorpusEntry is the CURRENT declaration of one registry name in the shared
// instance-config roots: the highest version any root declares, and the file
// declaring it.
type CorpusEntry struct {
	// Kind is RegistrationKindWorkflow or RegistrationKindSchema.
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	Path    string `json:"path"`
}

// CorpusIndex is one scan of the instance-config roots, read as "what does the
// corpus declare RIGHT NOW, for both registries".
//
// It is WorkflowOriginIndex's shape, with the two differences the audit needs:
// it carries the VERSION rather than only the path, and it covers SCHEMAS as
// well as workflows. The two indexes are kept apart rather than merged because
// WorkflowOriginIndex is built lazily on activation's refusal path, where
// paying for a schema pass to answer a workflow question would be a cost with
// no reader.
type CorpusIndex struct {
	// scan is the discovery this index reads. A nil scan means NO ROOT EXISTS
	// (scanConfigDirs' F17 dormancy) — the state in which every registered name
	// in the store would trivially look orphaned, which is why Scanned() is a
	// question every caller has to ask before believing a verdict.
	scan *configScan

	once sync.Once
	// current maps `kind\x00name` to the highest version declared for it.
	current map[string]CorpusEntry
	// err is the failure that stopped the build. Nothing is classified after
	// one: a half-read root can only manufacture false orphans and false
	// lag, and both cost an operator an investigation.
	err error
}

// ScanCorpus scans the instance-config roots for the audit.
//
// The error is the SCAN's own refusal — a root that is a regular file, a
// dangling symlink — surfaced rather than swallowed, for orphan_registration's
// reason: those are exactly the states in which having looked nowhere would be
// reported as every registration being stale.
func ScanCorpus() (*CorpusIndex, error) {
	scan, err := scanConfigDirs(resolvePaths().InstanceConfigDirs())
	if err != nil {
		return nil, err
	}
	return &CorpusIndex{scan: scan}, nil
}

// Scanned reports whether any instance-config root existed to look in.
func (i *CorpusIndex) Scanned() bool {
	return i != nil && i.scan != nil && len(i.scan.roots) > 0
}

// Roots are the canonicalized roots that were scanned, in precedence order.
func (i *CorpusIndex) Roots() []string {
	if i == nil || i.scan == nil {
		return nil
	}
	return i.scan.roots
}

// Err is the failure that stopped the build, if any.
func (i *CorpusIndex) Err() error {
	if i == nil {
		return nil
	}
	i.build()
	return i.err
}

// Current returns what the corpus declares for one kind and name.
func (i *CorpusIndex) Current(kind, name string) (CorpusEntry, bool) {
	if i == nil {
		return CorpusEntry{}, false
	}
	i.build()
	if i.err != nil {
		return CorpusEntry{}, false
	}
	entry, ok := i.current[kind+"\x00"+name]
	return entry, ok
}

// Entries are every declaration the scan found, ordered by kind then name, for
// a caller that wants to render what "current" meant.
func (i *CorpusIndex) Entries() []CorpusEntry {
	if i == nil {
		return nil
	}
	i.build()
	out := make([]CorpusEntry, 0, len(i.current))
	for _, e := range i.current {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// build derives the identity of every registry file the scan found, ONCE.
//
// THE HIGHEST VERSION WINS, not the first file seen. A corpus bumps a workflow
// IN PLACE — the version lives in the body, so `investigation.toml` simply says
// 8 where it used to say 4 — but schemas are versioned IN THE FILENAME, so
// `findings@1.json` and `findings@2.json` sit side by side and only the second
// is what a project ought to be carrying. Taking the maximum is the one rule
// that reads both layouts correctly.
//
// A file that DOES NOT PARSE declares no identity — configIdentity's rule, for
// registryIdentityKey's reason: the refusal for an unparseable definition
// belongs to registration, which words it precisely. The consequence is worth
// stating, as it is there: while a workflow file is unparseable, the name it
// WOULD declare reads as orphaned here.
//
// A workflow that cannot be READ fails the build instead, and every verdict
// becomes unavailable. That is an environment fault rather than an authoring
// one, and manufacturing an orphan out of a permission error is the one output
// this report must never produce.
func (i *CorpusIndex) build() {
	i.once.Do(func() {
		i.current = make(map[string]CorpusEntry)
		if i.scan == nil {
			return
		}
		for _, path := range i.scan.paths {
			// A SCHEMA'S IDENTITY IS ITS FILENAME, so its document is never
			// opened here. Reading every schema in the corpus to learn a name
			// already spelled in the path would be IO with no reader.
			var src []byte
			if !isSchemaConfigPath(path) {
				body, err := os.ReadFile(path)
				if err != nil {
					i.err = fmt.Errorf(
						"reading the instance-config workflow %s: %w", path, err)
					i.current = nil
					return
				}
				src = body
			}

			kind, name, version, ok := configIdentity(path, src)
			if !ok {
				continue
			}
			key := kind + "\x00" + name
			if prev, dup := i.current[key]; dup && prev.Version >= version {
				continue
			}
			i.current[key] = CorpusEntry{
				Kind: kind, Name: name, Version: version, Path: path,
			}
		}
	})
}

// RegistryLag is one registered name whose highest registered version is BEHIND
// what the corpus now declares.
type RegistryLag struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// RegisteredVersion is the highest version this project holds a row for,
	// retired versions INCLUDED: the question is what the registry contains,
	// not what would bind, and a retired row is still a registration.
	RegisteredVersion int `json:"registered_version"`
	CurrentVersion    int `json:"current_version"`
	// CurrentPath is the corpus file declaring the current version, so the
	// operator can read the definition they are behind without a second search.
	CurrentPath string `json:"current_path,omitempty"`
}

// RegistryOrphan is one registered name that NO file in any scanned root
// declares any more — the state a rename leaves behind, since registering the
// new name never retires the old one.
type RegistryOrphan struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Versions are every registered version of the stranded name, ascending.
	// A rename typically strands several at once, and `workflow deprecate`
	// takes them one at a time.
	Versions []int `json:"versions"`
	// Retired is true when EVERY version listed is already deprecated — the
	// orphan an operator has finished with. It is reported rather than filtered
	// out, for `workflow list --orphans`' reason: hiding an already-retired
	// orphan would leave a cleanup pass unable to see its own work. Schemas
	// carry no deprecation, so it is always false for them.
	Retired bool `json:"retired"`
}

// ProjectRegistryAudit is one project's verdict.
type ProjectRegistryAudit struct {
	ProjectID int    `json:"project_id"`
	Project   string `json:"project"`
	Prefix    string `json:"prefix,omitempty"`
	Identity  string `json:"identity,omitempty"`
	// Compared is how many distinct registered names were classified. It is
	// carried so "no findings" is distinguishable from "nothing registered
	// here" — two clean-looking results with nothing in common.
	Compared int              `json:"compared"`
	Behind   []RegistryLag    `json:"behind"`
	Orphaned []RegistryOrphan `json:"orphaned"`
}

// Clean reports whether this project matched the corpus on every name.
func (p ProjectRegistryAudit) Clean() bool {
	return len(p.Behind) == 0 && len(p.Orphaned) == 0
}

// RegistryAudit is the whole store's verdict, plus where "current" was read
// from.
type RegistryAudit struct {
	// Roots are the instance-config roots the one scan walked, in precedence
	// order, so a reader can tell WHERE current was read from — and can see
	// when a root they expected is missing.
	Roots    []string               `json:"roots"`
	Projects []ProjectRegistryAudit `json:"projects"`
	// BehindTotal and OrphanedTotal are store-wide counts of the FINDINGS, not
	// of the projects carrying them.
	BehindTotal   int `json:"behind_total"`
	OrphanedTotal int `json:"orphaned_total"`
}

// RegistryAuditOptions narrows the audit.
type RegistryAuditOptions struct {
	// ProjectID audits ONE project; zero audits every project in the store,
	// which is the whole point of the verb and therefore the default.
	ProjectID int
}

// AuditRegistries compares every project's registered workflow and schema rows
// against ONE scan of the shared corpus.
//
// The index is passed in rather than scanned here so the CALLER owns the two
// refusals that precede any verdict — no root exists, and a root that would not
// scan — and can word them for an operator. A report that silently classified
// against an empty index would name every registration in the store.
func AuditRegistries(
	conn *sql.DB, index *CorpusIndex, opts RegistryAuditOptions,
) (*RegistryAudit, error) {
	projects, err := db.ListProjects(conn)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}

	audit := &RegistryAudit{Roots: index.Roots()}
	for _, p := range projects {
		if opts.ProjectID != 0 && p.ID != opts.ProjectID {
			continue
		}
		one, err := auditProject(conn, index, p)
		if err != nil {
			return nil, err
		}
		audit.BehindTotal += len(one.Behind)
		audit.OrphanedTotal += len(one.Orphaned)
		audit.Projects = append(audit.Projects, *one)
	}
	return audit, nil
}

// registered is one name's rows in one project's registry, reduced to the three
// facts the comparison needs.
type registered struct {
	highest  int
	versions []int
	// live is true once any version of the name is still eligible to bind.
	live bool
}

func auditProject(
	conn *sql.DB, index *CorpusIndex, p *model.Project,
) (*ProjectRegistryAudit, error) {
	out := &ProjectRegistryAudit{
		ProjectID: p.ID, Project: p.Name, Prefix: p.Prefix, Identity: p.Identity,
	}

	// EVERY registered version is listed, retired ones included: `workflow
	// list`'s default hides them because they no longer BIND, and this verb is
	// asking what the registry HOLDS. An orphan whose versions were all retired
	// last week is a finished cleanup, and reporting it as finished is more use
	// than not reporting it at all.
	workflows, _, err := db.ListWorkflows(conn, db.WorkflowListOptions{ProjectID: p.ID})
	if err != nil {
		return nil, fmt.Errorf("listing workflows for project %d: %w", p.ID, err)
	}
	byName := make(map[string]*registered, len(workflows))
	for _, wf := range workflows {
		fold(byName, wf.Name, wf.Version, !wf.Deprecated())
	}
	out.Compared += classify(out, index, RegistrationKindWorkflow, byName)

	schemas, _, err := db.ListSchemas(conn, db.SchemaListOptions{ProjectID: p.ID})
	if err != nil {
		return nil, fmt.Errorf("listing schemas for project %d: %w", p.ID, err)
	}
	byName = make(map[string]*registered, len(schemas))
	for _, s := range schemas {
		// THE BUILTIN IS NOT CORPUS-BACKED. `aggregate@1` ships in the binary,
		// is visible to every project by design, and no file in any root
		// declares it — so classifying it would report one unfixable orphan for
		// every project in the store, on every run of this verb.
		if s.Builtin {
			continue
		}
		fold(byName, s.Name, s.Version, true)
	}
	out.Compared += classify(out, index, RegistrationKindSchema, byName)

	sortLags(out.Behind)
	sortOrphans(out.Orphaned)
	return out, nil
}

func fold(index map[string]*registered, name string, version int, live bool) {
	r, ok := index[name]
	if !ok {
		r = &registered{}
		index[name] = r
	}
	r.versions = append(r.versions, version)
	if version > r.highest {
		r.highest = version
	}
	r.live = r.live || live
}

// classify sorts one kind's registered names into the two findings, and returns
// how many names it looked at.
//
// THE VERDICT IS PER NAME, NOT PER VERSION — the rule `workflow list --orphans`
// established. A superseded version whose name is still declared is ordinary
// lineage and not a finding, and a project holding four versions of one stale
// name is one thing to fix, not four.
func classify(
	out *ProjectRegistryAudit, index *CorpusIndex, kind string,
	names map[string]*registered,
) int {
	for name, r := range names {
		sort.Ints(r.versions)
		entry, declared := index.Current(kind, name)
		switch {
		case !declared:
			out.Orphaned = append(out.Orphaned, RegistryOrphan{
				Kind: kind, Name: name, Versions: r.versions, Retired: !r.live,
			})
		case r.highest < entry.Version:
			out.Behind = append(out.Behind, RegistryLag{
				Kind: kind, Name: name,
				RegisteredVersion: r.highest,
				CurrentVersion:    entry.Version,
				CurrentPath:       entry.Path,
			})
		}
	}
	return len(names)
}

func sortLags(lags []RegistryLag) {
	sort.Slice(lags, func(a, b int) bool {
		if lags[a].Kind != lags[b].Kind {
			return lags[a].Kind < lags[b].Kind
		}
		return lags[a].Name < lags[b].Name
	})
}

func sortOrphans(orphans []RegistryOrphan) {
	sort.Slice(orphans, func(a, b int) bool {
		if orphans[a].Kind != orphans[b].Kind {
			return orphans[a].Kind < orphans[b].Kind
		}
		return orphans[a].Name < orphans[b].Name
	})
}
