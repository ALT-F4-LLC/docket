package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// docListResult is `doc list`'s payload. `Docs` holds SUMMARY rows
// ([]docListRow) by default and the full doc shape ([]*model.Doc) under
// `--with-body`, which is why it is typed `any`: the two shapes differ, and the
// default one is the reason this verb is usable as a picker at all (DKT-1045).
//
// Before this, every row carried its whole `body`, so a type-filtered listing of
// eleven documents was 83KB of JSON — larger than a harness tool result may be —
// to answer a question ("which of these do I want?") that needs only ids and
// titles. The bodies now live where they are asked for: `doc show`.
type docListResult struct {
	Docs  any `json:"docs"`
	Total int `json:"total"`
	// limit is the effective --limit, retained unexported for v2 truncation.
	limit int
	// count is len(the rows in Docs), kept alongside because Docs is `any`.
	count int
}

// docListResult implements output.Collection for the v2 envelope. Total is a
// pre-LIMIT count, so truncation is directly computable.
func (r docListResult) CollectionItems() any { return r.Docs }
func (r docListResult) CollectionTotal() int { return r.Total }
func (r docListResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.Total, r.count)
}

// docListRow is one summary row: everything needed to PICK a document, and
// nothing that scales with its length. `body_bytes` stands in for the omitted
// body so a caller can still see which documents are substantial and which are
// stubs, and can tell an empty body from an absent one.
type docListRow struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	BodyBytes int    `json:"body_bytes"`
}

// summarizeDoc projects a doc onto its list row. Timestamps use the same UTC
// RFC3339 rendering as model.Doc's marshaler, so a row and a `doc show` agree.
func summarizeDoc(d *model.Doc) docListRow {
	return docListRow{
		ID:        model.FormatDocID(d.ID),
		Type:      d.Type,
		Status:    d.Status,
		Title:     d.Title,
		Author:    d.Author,
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: d.UpdatedAt.UTC().Format(time.RFC3339),
		BodyBytes: len(d.Body),
	}
}

var docListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List documents",
	Aliases: []string{"ls"},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runDocList)
	},
}

func runDocList(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	types, _ := cmd.Flags().GetStringSlice("type")
	statuses, _ := cmd.Flags().GetStringSlice("status")
	author, _ := cmd.Flags().GetString("author")
	sortFlag, _ := cmd.Flags().GetString("sort")
	limit, _ := cmd.Flags().GetInt("limit")
	withBody, _ := cmd.Flags().GetBool("with-body")

	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

	opts := db.DocListOptions{
		ProjectID: getProjectID(cmd),
		Types:     types,
		Statuses:  statuses,
		Author:    author,
		Limit:     limit,
	}

	if sortFlag != "" {
		parts := strings.SplitN(sortFlag, ":", 2)
		opts.Sort = parts[0]
		if len(parts) > 1 {
			opts.SortDir = parts[1]
		}
	}

	summaries, total, err := db.ListDocsWithCounts(conn, opts)
	if err != nil {
		return cmdErr(fmt.Errorf("listing docs: %w", err), output.ErrGeneral)
	}

	// `--with-body` re-emits the pre-DKT-1045 row verbatim — the full
	// model.Doc shape, body included — so the one caller that wants bodies in
	// bulk (an export, a grep across a corpus) keeps a single call, and its
	// output is byte-identical to what this verb used to print by default.
	var payload any
	if withBody {
		docs := make([]*model.Doc, 0, len(summaries))
		for _, s := range summaries {
			docs = append(docs, s.Doc)
		}
		payload = docs
	} else {
		rows := make([]docListRow, 0, len(summaries))
		for _, s := range summaries {
			rows = append(rows, summarizeDoc(s.Doc))
		}
		payload = rows
	}

	result := docListResult{Docs: payload, Total: total, limit: limit, count: len(summaries)}

	var message string
	if !w.JSONMode {
		rows := make([]render.DocRow, 0, len(summaries))
		for _, s := range summaries {
			rows = append(rows, render.DocRow{
				Doc:             s.Doc,
				CurrentRevision: s.CurrentRevision,
				RevisionsCount:  s.RevisionsCount,
			})
		}
		message = render.RenderDocList(rows)
	}
	w.Success(result, message)

	return nil
}

func init() {
	docListCmd.Flags().StringSliceP("type", "T", nil, "Filter by type (repeatable)")
	docListCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (repeatable)")
	docListCmd.Flags().StringP("author", "a", "", "Filter by author")
	docListCmd.Flags().String("sort", "", "Sort by field:direction (e.g. updated_at:desc)")
	docListCmd.Flags().Int("limit", 50, "Maximum number of results")
	docListCmd.Flags().Bool("with-body", false,
		"Include each document's full body in --json output (listings are body-free by default)")
	docCmd.AddCommand(docListCmd)
}
