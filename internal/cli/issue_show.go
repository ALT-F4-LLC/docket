package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// showResult composes the issue fields with additional detail fields
// (sub-issues, relations, comments, activity) into a single flat JSON object.
type showResult struct {
	Issue           *model.Issue     `json:"-"`
	SubIssues       []*model.Issue   `json:"sub_issues"`
	Relations       []model.Relation `json:"relations"`
	LinkedProposals []model.Proposal `json:"-"`
	Comments        []*model.Comment `json:"comments"`
	Activity        []model.Activity `json:"activity"`

	// RunDisposition is the last terminal ruling a run made about its work on
	// this issue, or nil when no run ever abandoned it (DKT-404).
	//
	// It is a fact about a RUN, not about the issue, which is exactly why the
	// issue row cannot carry it: `abandon-issue` deliberately leaves the
	// tracker status alone, so four issues RUN-14 gave up on sat at `todo`
	// indistinguishable from work nobody had started, and a reader who wanted
	// the disposition had to go to `events list` to find it.
	RunDisposition *model.IssueRunDisposition `json:"-"`

	// Scope is the issue's declared scope globs, or nil when none is declared.
	// It reaches the wire under --json=v2 only; see showResultV2.Scope.
	Scope []string `json:"-"`
	// ScopeDeclared distinguishes a NULL `scope_globs` from a stored `[]`.
	// Those are different facts (see internal/cli/issue_scope.go), so the
	// wire format must not collapse them.
	ScopeDeclared bool `json:"-"`
}

// showResultJSON is the wire format that explicitly lists all fields,
// avoiding the fragile marshal-unmarshal-remarshal pattern.
type showResultJSON struct {
	ID string `json:"id"`
	// Issue mirrors ID under the noun every other verb keys its primary id by
	// (DKT-452). See model.issueJSON.Issue for the whole ruling; this payload
	// is `issue show`'s own flattened shape rather than a marshaled
	// model.Issue, so it needs the field spelled out a second time or the
	// detail view would be the one issue surface still missing the key.
	Issue           string           `json:"issue"`
	ParentID        *string          `json:"parent_id,omitempty"`
	Title           string           `json:"title"`
	Description     string           `json:"description"`
	Status          string           `json:"status"`
	Priority        string           `json:"priority"`
	Kind            string           `json:"kind"`
	Assignee        string           `json:"assignee"`
	Labels          []string         `json:"labels"`
	Files           []string         `json:"files"`
	Docs            []model.DocRef   `json:"docs"`
	CreatedAt       string           `json:"created_at"`
	UpdatedAt       string           `json:"updated_at"`
	SubIssues       []*model.Issue   `json:"sub_issues"`
	Relations       []model.Relation `json:"relations"`
	LinkedProposals []string         `json:"linked_proposals"`
	Comments        []*model.Comment `json:"comments"`
	Activity        []model.Activity `json:"activity"`
	// Scope reaches v1 WHEN DECLARED (DKT-55 amendment): nil emits no key, so
	// a scope-less issue's v1 output stays byte-identical to the frozen shape.
	// showResultV2 shadows this with its always-present tri-state field.
	Scope *[]string `json:"scope,omitempty"`
	// Resolution reaches v1 WHEN SET, the same DKT-55 shape and for the same
	// reason: an unresolved issue's output stays byte-identical, while an
	// abandoned one stops being indistinguishable from a finished one
	// (DKT-245).
	Resolution string `json:"resolution,omitempty"`
	// RunDisposition reaches v1 WHEN A RUN ABANDONED ITS WORK, the same DKT-55
	// shape for the third time: an issue no run gave up on emits no key and
	// keeps the frozen v1 output byte-identical, while an abandoned one stops
	// forcing its reader through `events list` to learn which run stopped, when
	// and why (DKT-404).
	RunDisposition *runDispositionJSON `json:"run_disposition,omitempty"`
}

// runDispositionJSON is the wire shape of model.IssueRunDisposition.
//
// It renders the ids the way every other field of this payload does — `RUN-32`,
// not 32; RFC3339 UTC, not epoch milliseconds — so a consumer reading
// `run_disposition.run` can pass it straight to `docket run report` without
// knowing how the store spells a run id.
type runDispositionJSON struct {
	Run         string `json:"run"`
	Disposition string `json:"disposition"`
	// By is the step instance that decided it, absent when an operator
	// abandoned the issue from outside the graph with `run abandon --issue`.
	By string `json:"by,omitempty"`
	// Reason is the recorded ruling VERBATIM and unabridged — a renderer may
	// show a head, this carries the whole thing — and absent when the deciding
	// path recorded no note at all.
	Reason string `json:"reason,omitempty"`
	At     string `json:"at"`
}

// runDispositionWire converts the engine's read model to the wire shape, or nil
// for the overwhelmingly common issue no run ever abandoned.
func runDispositionWire(d *model.IssueRunDisposition) *runDispositionJSON {
	if d == nil {
		return nil
	}
	return &runDispositionJSON{
		Run:         model.FormatRunID(d.RunID),
		Disposition: d.Disposition,
		By:          d.By,
		Reason:      d.Reason,
		At:          time.UnixMilli(d.AtMS).UTC().Format(time.RFC3339),
	}
}

// showResultV2 is the v2 payload: the v1 flattened shape plus the CAS version,
// plus the lease when one is held.
//
// Lease is omitempty so an unclaimed issue emits no `lease` key at all — the
// field appears exactly when there is a lease to report.
// Scope carries the issue's declared scope globs — the field `issue create
// --scope` writes and that activation reads to decide claim conflicts. A read
// surface that omitted it forced callers to query sqlite directly.
//
// It is a *[]string, not a []string with omitempty, because NULL and `[]` are
// different facts: "no scope declared" versus "declared to touch nothing"
// (internal/cli/issue_scope.go). A null `scope` and a `scope: []` therefore
// render distinctly, and omitempty — which collapses both to absent — would
// destroy exactly the distinction the column exists to record.
type showResultV2 struct {
	showResultJSON
	Version int       `json:"version"`
	Lease   any       `json:"lease,omitempty"`
	Scope   *[]string `json:"scope"`
}

// VersionedPayload implements output.Versioned, surfacing the issue's CAS
// version and lease under --json=v2 only. The v1 marshaler below is frozen.
//
// The lease's `live` flag is computed here from the current clock, per read:
// engine-spec.md §2 requires effective status at read time with no write, so
// an expired lease reads as dead without anyone having called claim.
func (s showResult) VersionedPayload() any {
	base, err := s.marshalJSONStruct()
	if err != nil {
		// Fall back to the v1 shape; the marshaler will surface the error.
		return s
	}
	version := 0
	var lease any
	if s.Issue != nil {
		version = s.Issue.Version
		lease = s.Issue.Lease.LeaseWire(model.NowMS())
	}

	// A declared-but-empty scope must marshal as `[]`, not `null`, so the
	// pointer is taken from a non-nil slice.
	var scope *[]string
	if s.ScopeDeclared {
		globs := s.Scope
		if globs == nil {
			globs = []string{}
		}
		scope = &globs
	}

	return showResultV2{
		showResultJSON: base,
		Version:        version,
		Lease:          lease,
		Scope:          scope,
	}
}

func (s showResult) MarshalJSON() ([]byte, error) {
	j, err := s.marshalJSONStruct()
	if err != nil {
		return nil, err
	}
	return json.Marshal(j)
}

func (s showResult) marshalJSONStruct() (showResultJSON, error) {
	i := s.Issue

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
	subIssues := s.SubIssues
	if subIssues == nil {
		subIssues = []*model.Issue{}
	}
	relations := s.Relations
	if relations == nil {
		relations = []model.Relation{}
	}
	linkedProposals := make([]string, 0, len(s.LinkedProposals))
	for _, p := range s.LinkedProposals {
		linkedProposals = append(linkedProposals, model.FormatProposalID(p.ID))
	}
	comments := s.Comments
	if comments == nil {
		comments = []*model.Comment{}
	}
	activity := s.Activity
	if activity == nil {
		activity = []model.Activity{}
	}

	id := model.FormatID(i.ID)
	j := showResultJSON{
		ID:              id,
		Issue:           id,
		Title:           i.Title,
		Description:     i.Description,
		Status:          string(i.Status),
		Priority:        string(i.Priority),
		Kind:            string(i.Kind),
		Assignee:        i.Assignee,
		Labels:          labels,
		Files:           files,
		Docs:            docs,
		CreatedAt:       i.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:       i.UpdatedAt.UTC().Format(time.RFC3339),
		SubIssues:       subIssues,
		Relations:       relations,
		LinkedProposals: linkedProposals,
		Comments:        comments,
		Activity:        activity,
		Resolution:      i.Resolution,
		RunDisposition:  runDispositionWire(s.RunDisposition),
	}

	if i.ParentID != nil {
		pid := model.FormatID(*i.ParentID)
		j.ParentID = &pid
	}

	// DKT-55: a DECLARED scope reaches v1. A declared-but-empty scope must
	// marshal as `[]`, not be collapsed into absence, so the pointer is taken
	// from a non-nil slice; an undeclared scope stays a nil pointer and emits
	// no key at all.
	if s.ScopeDeclared {
		globs := s.Scope
		if globs == nil {
			globs = []string{}
		}
		j.Scope = &globs
	}

	return j, nil
}

var showCmd = &cobra.Command{
	Use:   "show id...",
	Short: "Show issue details",
	Long: `Show one or more issues at full detail.

A single id emits the same flat object it always has, unchanged. Two or
more ids emit a JSON array of that same shape under data — the conductor's
most common read shape is a batch of ids, and this is the difference
between one call and a shell for-loop plus jq.

Under --json the issue's id is served under BOTH keys: .data.id, the
original spelling, and .data.issue, the noun every other verb keys its
primary entity by (run status -> run, step show -> step, dispatch open ->
dispatch). They always hold the same value, on --json and --json=v2 alike,
and issue list rows carry the same pair.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runIssueShow)
	},
}

// fetchIssueDetail loads one issue's full show detail: the issue itself plus
// every hydrated collection showResult carries. It is the single-id body
// runIssueShow calls once per argument, so batch and single-id show stay
// byte-identical per issue.
func fetchIssueDetail(conn *sql.DB, source string) (*model.Issue, showResult, error) {
	id, err := issueArg(source)
	if err != nil {
		return nil, showResult{}, err
	}

	issue, err := db.GetIssue(conn, id)
	if err != nil {
		if e := notFound(err, fmt.Sprintf("issue %s", source)); e != nil {
			return nil, showResult{}, e
		}
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
	}

	// Hydrate labels.
	issue.Labels, err = db.GetIssueLabels(conn, id)
	if err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching labels: %w", err), output.ErrGeneral)
	}

	// Hydrate files.
	issue.Files, err = db.GetIssueFiles(conn, id)
	if err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching files: %w", err), output.ErrGeneral)
	}

	if err := db.HydrateDocs(conn, []*model.Issue{issue}); err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching linked docs: %w", err), output.ErrGeneral)
	}

	subIssues, err := db.GetSubIssues(conn, id)
	if err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching sub-issues: %w", err), output.ErrGeneral)
	}

	relations, err := db.GetIssueRelations(conn, id)
	if err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
	}

	linkedProposals, err := db.GetIssueProposals(conn, id)
	if err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching linked proposals: %w", err), output.ErrGeneral)
	}

	comments, err := db.ListComments(conn, id)
	if err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching comments: %w", err), output.ErrGeneral)
	}

	activity, err := db.GetActivity(conn, id, 10)
	if err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching activity: %w", err), output.ErrGeneral)
	}

	// The last run to give up on this issue, if any (DKT-404). Nil is the
	// ordinary answer and costs one indexed read of the event log.
	disposition, err := engine.LatestIssueDisposition(conn, id)
	if err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching the run disposition: %w", err), output.ErrGeneral)
	}

	// Scope is stored as a JSON array in `issues.scope_globs`, or NULL when
	// the issue declares none. The empty string is the NULL case.
	scopeJSON, err := db.IssueScopeGlobs(conn, id)
	if err != nil {
		return nil, showResult{}, cmdErr(fmt.Errorf("fetching scope: %w", err), output.ErrGeneral)
	}
	var scope []string
	scopeDeclared := scopeJSON != ""
	if scopeDeclared {
		if err := json.Unmarshal([]byte(scopeJSON), &scope); err != nil {
			return nil, showResult{}, cmdErr(fmt.Errorf("decoding scope: %w", err), output.ErrGeneral)
		}
	}

	result := showResult{
		Issue:           issue,
		SubIssues:       subIssues,
		Relations:       relations,
		LinkedProposals: linkedProposals,
		Comments:        comments,
		Activity:        activity,
		RunDisposition:  disposition,
		Scope:           scope,
		ScopeDeclared:   scopeDeclared,
	}
	return issue, result, nil
}

func runIssueShow(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	if len(args) == 1 {
		issue, result, err := fetchIssueDetail(conn, args[0])
		if err != nil {
			return err
		}
		var message string
		if !w.JSONMode {
			message = render.RenderDetail(issue, result.SubIssues, result.Relations,
				result.LinkedProposals, result.Comments, result.Activity,
				result.RunDisposition)
		}
		w.Success(result, message)
		return nil
	}

	results := make([]showResult, 0, len(args))
	var messages []string
	for _, arg := range args {
		issue, result, err := fetchIssueDetail(conn, arg)
		if err != nil {
			return err
		}
		results = append(results, result)
		if !w.JSONMode {
			messages = append(messages, render.RenderDetail(issue, result.SubIssues,
				result.Relations, result.LinkedProposals, result.Comments, result.Activity,
				result.RunDisposition))
		}
	}

	var message string
	if !w.JSONMode {
		message = strings.Join(messages, "\n\n")
	}
	w.Success(results, message)
	return nil
}

func init() {
	issueCmd.AddCommand(showCmd)
}
