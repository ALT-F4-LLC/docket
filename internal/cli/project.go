package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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
	Use:         "list",
	Short:       "List the store's projects",
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		result, err := verb.ProjectList(cmd.Context(), projectVerbEnv(cmd), verb.ProjectListInput{})
		if err != nil {
			return verbErr(err)
		}

		views := make([]projectView, 0, len(result.Projects))
		for _, p := range result.Projects {
			views = append(views, projectView{
				ID: p.ID, Name: p.Name, Prefix: p.Prefix,
				Identity: p.Identity, Current: p.ID == result.Current,
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
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		env := projectVerbEnv(cmd)
		result, err := verb.ProjectSetPrefix(cmd.Context(), env, verb.ProjectSetPrefixInput{Prefix: args[0]})
		recordRegisteredProject(cmd, env, result.Registered)
		if err != nil {
			return verbErr(err)
		}

		// The rest of THIS invocation renders in the new voice too — the
		// confirmation below is the first output that should carry it.
		project := result.Project
		model.SetDisplayPrefix(project.Prefix)

		w.Success(projectView{
			ID: project.ID, Name: project.Name, Prefix: project.Prefix,
			Identity: project.Identity, Current: true,
		}, fmt.Sprintf("Issues here now read as %s-N (the number stays the store-wide id)", project.Prefix))
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
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		env := projectVerbEnv(cmd)
		result, err := verb.ProjectDelete(cmd.Context(), env, verb.ProjectDeleteInput{Project: args[0]})
		recordRegisteredProject(cmd, env, result.Registered)
		if err != nil {
			return verbErr(err)
		}

		target := result.Project
		w.Success(projectDeleteResult{
			ID: target.ID, Name: target.Name, Identity: target.Identity, Deleted: true,
		}, fmt.Sprintf("Deleted project %q (%s)", target.Name, target.Identity))
		return nil
	},
}

// projectVerbEnv builds the verb.Env the project commands run in.
func projectVerbEnv(cmd *cobra.Command) verb.Env {
	cwd, _ := os.Getwd()
	env := verb.Env{
		Store:  store.StoreOn(getDB(cmd)),
		Author: config.DefaultAuthor(),
		Cwd:    cwd,
	}
	if cfg := getCfg(cmd); cfg != nil {
		env.Project = verb.ProjectInput{Identity: cfg.Identity, Anchored: cfg.Anchored}
	}
	return env
}

// recordRegisteredProject writes the project-registered event (DKT-61) for
// a project a verb registered on first contact, as the root hook does for
// the commands whose project it resolves. A failure is reported, not
// returned: the project exists either way.
func recordRegisteredProject(cmd *cobra.Command, env verb.Env, registered *model.Project) {
	if registered == nil {
		return
	}
	if err := engine.RecordProjectRegisteredEvent(getDB(cmd), engine.ProjectRegistration{
		ID: registered.ID, Name: registered.Name, Identity: registered.Identity,
		Cwd: env.Cwd, Verb: commandPath(cmd), Prefix: registered.Prefix,
	}, time.Now().UnixMilli()); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"docket: registered project %q but could not record the event: %v\n",
			registered.Name, err)
	}
}

// resolveProjectRef forwards to verb.ResolveProjectRef for the CLI surfaces
// that address a project by `--project`.
func resolveProjectRef(conn store.LegacyDB, ref string) (*model.Project, error) {
	return fromVerb(verb.ResolveProjectRef(store.StoreOn(conn), ref))
}

// projectDisplayName forwards to verb.ProjectDisplayName.
func projectDisplayName(p *model.Project) string {
	return verb.ProjectDisplayName(p)
}

func init() {
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectSetPrefixCmd)
	projectCmd.AddCommand(projectDeleteCmd)
	rootCmd.AddCommand(projectCmd)
}
