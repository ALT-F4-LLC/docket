package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// workflowListResult is a Collection, so v2 renders
// {items, total, truncated}. Total comes from a COUNT(*) that ignores LIMIT, so
// it is already the true pre-limit count and truncation is directly computable.
type workflowListResult struct {
	Workflows []*model.Workflow `json:"workflows"`
	Total     int               `json:"total"`
	// limit is the effective --limit, retained unexported so v1 output is
	// untouched, to compute truncation for the v2 envelope.
	limit int
}

func (r workflowListResult) CollectionItems() any {
	return workflowListPayload{workflows: r.Workflows}
}
func (r workflowListResult) CollectionTotal() int { return r.Total }
func (r workflowListResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.Total, len(r.Workflows))
}

// workflowListPayload wraps the items so the v2 envelope can add each row's
// CAS version. The envelope consults output.Versioned on the items CONTAINER
// rather than on every element, so a bare slice would render v1 items inside a
// v2 envelope — which is what this wrapper exists to prevent. Same shape as
// issueListPayload, for the same reason.
type workflowListPayload struct{ workflows []*model.Workflow }

// MarshalJSON emits the v1 array shape.
func (p workflowListPayload) MarshalJSON() ([]byte, error) {
	if p.workflows == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(p.workflows)
}

// VersionedPayload implements output.Versioned for a list of workflows.
func (p workflowListPayload) VersionedPayload() any {
	return model.WorkflowsWithVersion(p.workflows)
}

var workflowListCmd = &cobra.Command{
	Use:         "list",
	Short:       "List registered workflows",
	Aliases:     []string{"ls"},
	Annotations: map[string]string{"verbOwnsProject": ""},
	Long: `List registered workflows.

--orphans narrows the list to ORPHANED REGISTRATIONS: registered names that no
file in any instance-config root declares any more. A registration is a row and
not a file, so renaming a workflow leaves every version of the old name live and
still binding — which is how one issue's label came to match two workflows, one
of which had not existed on disk for weeks (DKT-609).

The verdict is per NAME, not per version: a superseded version whose name is
still declared somewhere is ordinary lineage, not an orphan. Deprecated rows are
listed like any other, as they are without the flag — they are precisely what a
cleanup pass wants to see, and hiding an already-retired orphan would make the
verb unable to show its own work. --deprecated has no effect under --orphans
for the same reason: orphan status is a filesystem verdict on the NAME, not a
binding verdict on the version, so narrowing by one must not silently narrow
by the other too.

--deprecated includes retired versions (deprecated_at_ms set) in the plain
listing; without it, only versions still eligible to bind are shown.

A registry is PER PROJECT. By default this lists the project the working
directory resolves to; --project lists one other project's registry instead,
with --deprecated, --orphans, and --name applying there exactly as they do
here. It takes the same refs the writing verbs take (PREFIX, NAME, IDENTITY, or
row id), so a project whose checkout is missing from this machine can still be
read from any other. The --orphans verdict is a filesystem fact about the NAME
and does not change with the project: the instance-config roots are THIS
invocation's, so a name another project registered from its own local config
directory reads as orphaned here.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowList(cmd, args, getWriter(cmd))
	},
}

func runWorkflowList(cmd *cobra.Command, args []string, w *output.Writer) error {
	name, _ := cmd.Flags().GetString("name")
	limit, _ := cmd.Flags().GetInt("limit")
	orphans, _ := cmd.Flags().GetBool("orphans")
	deprecated, _ := cmd.Flags().GetBool("deprecated")
	project, _ := cmd.Flags().GetString("project")
	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

	listed, err := verb.WorkflowList(cmd.Context(), verbEnv(cmd), verb.WorkflowListInput{
		Name:              name,
		Limit:             limit,
		IncludeDeprecated: deprecated,
		Orphans:           orphans,
		Project:           project,
	})
	if err != nil {
		return verbErr(err)
	}

	result := workflowListResult{Workflows: listed.Workflows, Total: listed.Total, limit: limit}

	var message string
	if !w.JSONMode {
		message = renderWorkflowList(listed.Workflows, orphans)
	}
	w.Success(result, message)
	return nil
}

func renderWorkflowList(workflows []*model.Workflow, orphans bool) string {
	if len(workflows) == 0 {
		if orphans {
			// NOT "No workflows registered." — under --orphans an empty result
			// is a clean bill of health for a store that may hold dozens of
			// workflows, and the two readings are opposite.
			return "No orphaned registrations: every registered workflow name " +
				"is still declared by a file in the instance config."
		}
		return "No workflows registered."
	}

	// EVERY registered version of each name is listed, not only the binding
	// one — the query is `ORDER BY name ASC, version DESC` with no reduction —
	// so lineage is visible rather than inferred. What was NOT visible before
	// is which of them still binds, which is what the marker below adds.
	var b strings.Builder
	for _, wf := range workflows {
		fmt.Fprintf(&b, "%-28s %s", wf.Ref(), shortSHA(wf.SourceSHA256))
		// The orphan marker prints only where a reader WENT AND LOOKED, which
		// today is --orphans alone. An unmarked row in the default listing
		// therefore means "not asked", never "checked and fine" — the same
		// rule `source_status` follows, and the reason the default output is
		// byte-identical to what it was.
		if wf.Origin.Orphaned() {
			b.WriteString("  [orphaned]")
		}
		if wf.Deprecated() {
			b.WriteString("  [deprecated]")
		}
		if wf.Description != "" {
			fmt.Fprintf(&b, "  %s", wf.Description)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// shortSHA renders the first 12 hex characters — enough to distinguish two
// registrations at a glance, with the full hash available under --json.
func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

func init() {
	workflowListCmd.Flags().String("name", "", "Filter by workflow name")
	workflowListCmd.Flags().Int("limit", 50, "Maximum number of results")
	workflowListCmd.Flags().Bool("orphans", false,
		"List only registrations whose name no file in any instance-config root declares")
	workflowListCmd.Flags().Bool("deprecated", false,
		"Include deprecated (retired) workflow versions in the listing")
	addProjectReadFlag(workflowListCmd, "List the registry")
	workflowCmd.AddCommand(workflowListCmd)
}
