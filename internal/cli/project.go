package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket project` — the shared store's tenancy surface (v12).
//
// A store used to BE a project, so there was nothing to list. Under ~/.docket
// every repository is a row, and these verbs are the operator's view of that
// dimension: which projects the store holds, which one this invocation is,
// and the one per-project display setting — the issue prefix.

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Inspect and configure the store's projects",
}

// projectView is one row of `project list` on the wire.
type projectView struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	Identity string `json:"identity"`
	Current  bool   `json:"current"`
}

type projectListResult struct {
	Projects []projectView `json:"projects"`
	Total    int           `json:"total"`
}

func (r projectListResult) CollectionItems() any      { return r.Projects }
func (r projectListResult) CollectionTotal() int      { return r.Total }
func (r projectListResult) CollectionTruncated() bool { return false }

var _ output.Collection = projectListResult{}

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the store's projects",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		projects, err := db.ListProjects(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("listing projects: %w", err), output.ErrGeneral)
		}

		current := getProjectID(cmd)
		views := make([]projectView, 0, len(projects))
		for _, p := range projects {
			views = append(views, projectView{
				ID: p.ID, Name: p.Name, Prefix: p.Prefix,
				Identity: p.Identity, Current: p.ID == current,
			})
		}

		var message string
		if !w.JSONMode {
			var b strings.Builder
			fmt.Fprintf(&b, "%-4s %-20s %-8s %s\n", "", "NAME", "PREFIX", "IDENTITY")
			for _, v := range views {
				marker := ""
				if v.Current {
					marker = "*"
				}
				identity := v.Identity
				if identity == "" {
					identity = "(unclaimed)"
				}
				fmt.Fprintf(&b, "%-4s %-20s %-8s %s\n", marker, v.Name, v.Prefix, identity)
			}
			message = strings.TrimRight(b.String(), "\n")
		}
		w.Success(projectListResult{Projects: views, Total: len(views)}, message)
		return nil
	},
}

var projectSetPrefixCmd = &cobra.Command{
	Use:   "set-prefix PREFIX",
	Short: "Set this project's issue display prefix",
	Long: `Set the prefix this project's issue ids render and parse with.

The prefix is DISPLAY ONLY: the number is the identity, global across the
store, so VOR-42 and DKT-42 name the same issue. DKT always parses whatever
the prefix, and a bare number always works — references in old commit
messages and other projects' run records never go stale.

1-8 letters; DOC, RUN, and STEP are reserved for their own entities.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		prefix := strings.ToUpper(strings.TrimSpace(args[0]))
		if err := model.ValidateProjectPrefix(prefix); err != nil {
			return cmdErr(err, output.ErrValidation)
		}

		projectID := getProjectID(cmd)

		// DKT-60: no two projects may display the same prefix. The prefix is a
		// project's ONLY discriminator in a listing, an event feed, or a run
		// report, so a shared one makes every id in the store ambiguous about
		// who owns it — which is exactly what happened when registration wrote
		// a hardcoded DKT for every row.
		holder, err := db.PrefixHolder(conn, prefix, projectID)
		if err != nil {
			return cmdErr(err, output.ErrGeneral)
		}
		if holder != 0 {
			other, gerr := db.GetProject(conn, holder)
			name := fmt.Sprintf("project %d", holder)
			if gerr == nil {
				name = fmt.Sprintf("%q (%s)", other.Name, other.Identity)
			}
			return cmdErr(fmt.Errorf(
				"prefix %s is already held by %s: two projects sharing a prefix "+
					"makes every id in the store ambiguous about its owner",
				prefix, name), output.ErrConflict)
		}

		if err := db.SetProjectPrefix(conn, projectID, prefix); err != nil {
			return cmdErr(fmt.Errorf("setting the prefix: %w", err), output.ErrGeneral)
		}

		// The rest of THIS invocation renders in the new voice too — the
		// confirmation below is the first output that should carry it.
		model.SetDisplayPrefix(prefix)

		project, err := db.GetProject(conn, projectID)
		if err != nil {
			return cmdErr(fmt.Errorf("re-reading the project: %w", err), output.ErrGeneral)
		}
		w.Success(projectView{
			ID: project.ID, Name: project.Name, Prefix: project.Prefix,
			Identity: project.Identity, Current: true,
		}, fmt.Sprintf("Issues here now read as %s-N (the number stays the store-wide id)", prefix))
		return nil
	},
}

// projectDeleteResult is `project delete`'s wire shape.
type projectDeleteResult struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Identity string `json:"identity"`
	Deleted  bool   `json:"deleted"`
}

var projectDeleteCmd = &cobra.Command{
	Use:   "delete PREFIX|NAME|IDENTITY|ID",
	Short: "Delete an empty project row",
	Long: `Delete a project that holds nothing.

The project is named the same four ways '--project' accepts: its display
prefix, its name, its identity path, or its row id — every column
'docket project list' prints.

This is the way back out for a project row created by mistake — before
registration was gated, any command run from a directory that was not a
repository minted one permanently, and the only remedy was a raw sqlite DELETE
against a store shared by every repository on the machine.

It REFUSES a project that any issue, run, document, proposal, workflow, schema,
or label still references, and refuses the default project outright. So it can
remove junk and cannot remove history. To empty a project first, re-home its
issues with 'docket issue move --project'.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		target, err := resolveProjectRef(conn, args[0])
		if err != nil {
			return err
		}

		if err := db.DeleteProject(conn, target.ID); err != nil {
			switch {
			case errors.Is(err, db.ErrProjectIsDefault):
				return cmdErr(fmt.Errorf(
					"project %q is the default project and cannot be deleted: "+
						"every pre-tenancy row in this store belongs to it",
					target.Name), output.ErrValidation)
			case errors.Is(err, db.ErrProjectInUse):
				counts, cerr := db.ProjectRefCounts(conn, target.ID)
				if cerr != nil {
					return cmdErr(cerr, output.ErrGeneral)
				}
				return cmdErr(fmt.Errorf(
					"project %q still holds %s: re-home them with "+
						"`docket issue move --project` before deleting it",
					target.Name, describeRefCounts(counts)), output.ErrConflict)
			default:
				return cmdErr(fmt.Errorf("deleting the project: %w", err), output.ErrGeneral)
			}
		}

		w.Success(projectDeleteResult{
			ID: target.ID, Name: target.Name, Identity: target.Identity, Deleted: true,
		}, fmt.Sprintf("Deleted project %q (%s)", target.Name, target.Identity))
		return nil
	},
}

// resolveProjectRef finds the project a `--project` value (or a `project
// delete` argument) names — by identity path, row id, name, or display PREFIX.
//
// It is the ONE resolver behind every project-addressing surface. There used to
// be two — this one and `issue move`'s — and they diverged: `issue move
// --project FLX` worked while `issue list --project FLX` answered NOT_FOUND
// (DKT-453). A second copy of a resolution ladder is a second set of keys, and
// which surface accepts which key is not something a caller can be expected to
// remember.
//
// FOUR KEYS, because `project list` prints three columns — NAME, PREFIX,
// IDENTITY — and any of them reads as the way to say which project this is.
// The PREFIX matters most: it is the only project discriminator an issue id
// carries (FLX-141), so it is the identifier a caller has actually SEEN before
// typing it, and it was the one key the resolver refused. A RUN-37
// conductor reached for it first, got NOT_FOUND, and fell back to listing every
// project's issues unfiltered.
//
// AMBIGUITY IS REFUSED, NOT RESOLVED. Identity is unique (the column's
// constraint) and a row id names one row; NAME is not unique at all — it is a
// directory basename — and PREFIX is unique by construction (db.availablePrefix
// hands out a free one, `project set-prefix` refuses a held one) but nothing
// makes an imported or hand-edited store obey that. When a key names more than
// one project the candidates are listed instead: the entire point of the flag is
// to say WHICH project, so picking the lowest id would answer a question nobody
// asked.
//
// Name and prefix match case-insensitively — a prefix is displayed uppercase and
// typed however it is typed — but an EXACT match wins outright, so two rows
// differing only in case each still resolve by their own spelling.
func resolveProjectRef(conn *sql.DB, ref string) (*model.Project, error) {
	projects, err := db.ListProjects(conn)
	if err != nil {
		return nil, cmdErr(fmt.Errorf("listing projects: %w", err), output.ErrGeneral)
	}

	// The identity path is the canonical key and is unique, so it is tried first
	// and can never be ambiguous.
	for _, p := range projects {
		if p.Identity == ref {
			return p, nil
		}
	}

	// A numeric ref is a row id and nothing else: names are directory basenames
	// and prefixes are letters only (model.ValidateProjectPrefix), so falling
	// through to them would only make the miss message vaguer.
	if id, err := strconv.Atoi(ref); err == nil {
		for _, p := range projects {
			if p.ID == id {
				return p, nil
			}
		}
		return nil, cmdErr(fmt.Errorf("no project with id %d", id), output.ErrNotFound)
	}

	matching := func(field func(*model.Project) string) []*model.Project {
		var exact, folded []*model.Project
		for _, p := range projects {
			switch value := field(p); {
			case value == "":
			case value == ref:
				exact = append(exact, p)
			case strings.EqualFold(value, ref):
				folded = append(folded, p)
			}
		}
		if len(exact) > 0 {
			return exact
		}
		return folded
	}

	for _, key := range []struct {
		verb  string
		field func(*model.Project) string
	}{
		{"names", func(p *model.Project) string { return p.Name }},
		{"is the prefix of", func(p *model.Project) string { return p.Prefix }},
	} {
		switch hits := matching(key.field); len(hits) {
		case 0:
		case 1:
			return hits[0], nil
		default:
			return nil, cmdErr(fmt.Errorf(
				"%q %s %d projects — address one by id or identity: %s",
				ref, key.verb, len(hits), describeProjectCandidates(hits)),
				output.ErrValidation)
		}
	}

	return nil, cmdErr(fmt.Errorf(
		"no project named %q; `docket project list` shows what this store holds "+
			"(its NAME, PREFIX, and IDENTITY columns all resolve here)",
		ref), output.ErrNotFound)
}

// describeProjectCandidates renders the candidates an ambiguous ref is refused
// with: the id to address one by, plus the name and identity that say which
// checkout each one is. Whichever key was ambiguous repeats across the list, so
// both of the others are printed — one of them is always the discriminator.
func describeProjectCandidates(ps []*model.Project) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		where := p.Identity
		if where == "" {
			where = "unclaimed"
		}
		out = append(out, fmt.Sprintf("%d (%s, %s)", p.ID, projectDisplayName(p), where))
	}
	return strings.Join(out, ", ")
}

// projectDisplayName picks the most human name a project row carries.
func projectDisplayName(p *model.Project) string {
	if p.Name != "" {
		return p.Name
	}
	if p.Identity != "" {
		return p.Identity
	}
	return strconv.Itoa(p.ID)
}

// describeRefCounts renders "3 issues, 1 run" for the refusal message. The
// counts are named because "still has rows" leaves the operator to go find
// which ones.
func describeRefCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "rows"
	}
	tables := make([]string, 0, len(counts))
	for table := range counts {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	parts := make([]string, 0, len(tables))
	for _, table := range tables {
		noun := strings.TrimSuffix(table, "s")
		if counts[table] != 1 {
			noun = table
		}
		parts = append(parts, fmt.Sprintf("%d %s", counts[table], noun))
	}
	return strings.Join(parts, ", ")
}

func init() {
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectSetPrefixCmd)
	projectCmd.AddCommand(projectDeleteCmd)
	rootCmd.AddCommand(projectCmd)
}
