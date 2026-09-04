package cli

import (
	"encoding/json"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// issueRow is one SUMMARY row of a list-shaped issue verb — `issue list`,
// `next`, `plan`, `board` — and the reason those verbs are usable as pickers
// at all (DKT-1053, the issue-side twin of DKT-1045).
//
// It is the frozen v1 issue shape with exactly one substitution: `description`
// is replaced by `description_bytes`. Every other key keeps its name, its
// position and its conditionality — `issue` mirrors `id` (DKT-452), `scope`
// appears when declared (DKT-55), `resolution` when set (DKT-245) — so a
// consumer that selects any key other than `description` off a list row reads
// exactly what it read before.
//
// Before this every row carried its whole description, so a filtered listing
// of a few dozen issues with real descriptions ran to tens of kilobytes —
// larger than a harness tool result may be — to answer a question ("which of
// these do I want?") that needs ids, titles and status. The description now
// lives where ONE issue is asked for: `issue show`. The byte count stands in
// for it so a caller can still tell a stub from a substantial issue, and an
// empty description from an absent one.
//
// It is a CLI-side projection rather than a change to model.Issue's marshaler
// because that marshaler is the frozen v1 wire format every single-issue verb
// (`issue show`, `create`, `edit`, `claim`, ...) emits, and those verbs are
// asked for one issue's whole body.
type issueRow struct {
	ID               string         `json:"id"`
	Issue            string         `json:"issue"`
	ParentID         *string        `json:"parent_id,omitempty"`
	Title            string         `json:"title"`
	DescriptionBytes int            `json:"description_bytes"`
	Status           string         `json:"status"`
	Priority         string         `json:"priority"`
	Kind             string         `json:"kind"`
	Assignee         string         `json:"assignee"`
	Labels           []string       `json:"labels"`
	Files            []string       `json:"files"`
	Docs             []model.DocRef `json:"docs"`
	Scope            *[]string      `json:"scope,omitempty"`
	Resolution       string         `json:"resolution,omitempty"`
	CreatedAt        string         `json:"created_at"`
	UpdatedAt        string         `json:"updated_at"`
}

// issueRowV2 is the summary row under --json=v2: the v1 row plus the CAS
// version, plus the lease when one is held — the same two additions
// model.VersionedIssue makes to the full shape, so a v2 consumer that read
// `version` or `lease` off a full list row reads them off a summary row too.
type issueRowV2 struct {
	issueRow
	Version int `json:"version"`
	Lease   any `json:"lease,omitempty"`
}

// summarizeIssue projects an issue onto its list row. Ids, timestamps and the
// nil-slice normalization match model.Issue's marshaler exactly, so the summary
// row and the `--with-body` row for one issue differ only in the field this
// type exists to drop.
func summarizeIssue(i *model.Issue) issueRow {
	labels := i.Labels
	if labels == nil {
		labels = []string{}
	}
	files := i.Files
	if files == nil {
		files = []string{}
	}
	docs := i.Docs
	if docs == nil {
		docs = []model.DocRef{}
	}

	id := model.FormatID(i.ID)
	row := issueRow{
		ID:               id,
		Issue:            id,
		Title:            i.Title,
		DescriptionBytes: len(i.Description),
		Status:           string(i.Status),
		Priority:         string(i.Priority),
		Kind:             string(i.Kind),
		Assignee:         i.Assignee,
		Labels:           labels,
		Files:            files,
		Docs:             docs,
		Scope:            i.Scope,
		Resolution:       i.Resolution,
		CreatedAt:        i.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:        i.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if i.ParentID != nil {
		pid := model.FormatID(*i.ParentID)
		row.ParentID = &pid
	}
	return row
}

// summarizeIssues projects a slice. The result is never nil: a listing that
// matches nothing must emit `[]`, not `null`.
func summarizeIssues(issues []*model.Issue) []issueRow {
	rows := make([]issueRow, 0, len(issues))
	for _, i := range issues {
		rows = append(rows, summarizeIssue(i))
	}
	return rows
}

// summarizeIssuesV2 is summarizeIssues with each row's version and lease. The
// lease's `live` flag is computed at nowMS, per read, the way
// model.VersionedIssue computes it (engine-spec.md §2: effective status at
// read time, no write).
func summarizeIssuesV2(issues []*model.Issue, nowMS int64) []issueRowV2 {
	rows := make([]issueRowV2, 0, len(issues))
	for _, i := range issues {
		rows = append(rows, issueRowV2{
			issueRow: summarizeIssue(i),
			Version:  i.Version,
			Lease:    i.Lease.LeaseWire(nowMS),
		})
	}
	return rows
}

// issueRowsPayload wraps issues for emission as summary rows: v1 marshals
// []issueRow, and the v2 collection envelope — which consults output.Versioned
// on the items CONTAINER, as issueListPayload's comment explains — gets
// []issueRowV2. It is issueListPayload's summary-shaped twin.
type issueRowsPayload struct{ issues []*model.Issue }

// MarshalJSON emits the v1 summary-row array. Never null.
func (p issueRowsPayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(summarizeIssues(p.issues))
}

// VersionedPayload implements output.Versioned for a list of summary rows.
func (p issueRowsPayload) VersionedPayload() any {
	return summarizeIssuesV2(p.issues, model.NowMS())
}

// issuesPayload picks a list-shaped verb's row shape: summary rows by default,
// and under `--with-body` the pre-DKT-1053 full issue shape, byte-identical to
// what the verb used to print — for the one caller that wants every
// description in a single call (an export, a grep across a backlog).
func issuesPayload(issues []*model.Issue, withBody bool) any {
	if withBody {
		return issueListPayload{issues: issues}
	}
	return issueRowsPayload{issues: issues}
}

// withBodyHelp is the `--with-body` flag's help text, shared by the four
// list-shaped verbs so `--help` describes the same escape hatch everywhere.
const withBodyHelp = "Include each issue's full description in --json output " +
	"(rows carry description_bytes instead by default; use issue show for one issue's body)"
