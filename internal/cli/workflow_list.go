package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// workflowListResult is a Collection (reliability-delta §4.1), so v2 renders
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
	Use:     "list",
	Short:   "List registered workflows",
	Aliases: []string{"ls"},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowList(cmd, args, getWriter(cmd))
	},
}

func runWorkflowList(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	name, _ := cmd.Flags().GetString("name")
	limit, _ := cmd.Flags().GetInt("limit")
	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

	workflows, total, err := db.ListWorkflows(conn, db.WorkflowListOptions{
		ProjectID: getProjectID(cmd),
		Name:      name,
		Limit:     limit,
	})
	if err != nil {
		return cmdErr(fmt.Errorf("listing workflows: %w", err), output.ErrGeneral)
	}

	result := workflowListResult{Workflows: workflows, Total: total, limit: limit}

	var message string
	if !w.JSONMode {
		message = renderWorkflowList(workflows)
	}
	w.Success(result, message)
	return nil
}

func renderWorkflowList(workflows []*model.Workflow) string {
	if len(workflows) == 0 {
		return "No workflows registered."
	}

	// EVERY registered version of each name is listed, not only the binding
	// one — the query is `ORDER BY name ASC, version DESC` with no reduction —
	// so lineage is visible rather than inferred. What was NOT visible before
	// is which of them still binds, which is what the marker below adds.
	var b strings.Builder
	for _, wf := range workflows {
		fmt.Fprintf(&b, "%-28s %s", wf.Ref(), shortSHA(wf.SourceSHA256))
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
	workflowCmd.AddCommand(workflowListCmd)
}
