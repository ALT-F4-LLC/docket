package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ResolutionAbandoned is the `issues.resolution` value the `abandon-issue`
// routing and `run abandon --issue` write: the machine stopped working this
// issue and did not finish it. It is the model-side name for what the db layer
// stores as IssueResolutionAbandoned.
const ResolutionAbandoned = "abandoned"

// IDPrefix is the prefix used for issue IDs in display and JSON output.
const IDPrefix = "DKT"

// Status represents the workflow state of an issue.
type Status string

const (
	StatusBacklog    Status = "backlog"
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in-progress"
	StatusReview     Status = "review"
	StatusDone       Status = "done"
)

var validStatuses = []Status{
	StatusBacklog,
	StatusTodo,
	StatusInProgress,
	StatusReview,
	StatusDone,
}

// ValidateStatus returns an error if s is not a recognized status.
func ValidateStatus(s Status) error {
	for _, v := range validStatuses {
		if s == v {
			return nil
		}
	}
	return fmt.Errorf("invalid status %q: must be one of %v", s, validStatuses)
}

// Color returns a color name string suitable for terminal rendering.
func (s Status) Color() string {
	switch s {
	case StatusBacklog:
		return "gray"
	case StatusTodo:
		return "blue"
	case StatusInProgress:
		return "yellow"
	case StatusReview:
		return "magenta"
	case StatusDone:
		return "green"
	default:
		return "white"
	}
}

// Icon returns a Unicode icon representing the status.
func (s Status) Icon() string {
	switch s {
	case StatusBacklog:
		return "○"
	case StatusTodo:
		return "●"
	case StatusInProgress:
		return "◐"
	case StatusReview:
		return "◎"
	case StatusDone:
		return "✔"
	default:
		return "○"
	}
}

// Priority represents the urgency of an issue.
type Priority string

const (
	PriorityCritical Priority = "critical"
	PriorityHigh     Priority = "high"
	PriorityMedium   Priority = "medium"
	PriorityLow      Priority = "low"
	PriorityNone     Priority = "none"
)

var validPriorities = []Priority{
	PriorityCritical,
	PriorityHigh,
	PriorityMedium,
	PriorityLow,
	PriorityNone,
}

// ValidatePriority returns an error if p is not a recognized priority.
func ValidatePriority(p Priority) error {
	for _, v := range validPriorities {
		if p == v {
			return nil
		}
	}
	return fmt.Errorf("invalid priority %q: must be one of %v", p, validPriorities)
}

// Color returns a color name string suitable for terminal rendering.
func (p Priority) Color() string {
	switch p {
	case PriorityCritical:
		return "red"
	case PriorityHigh:
		return "yellow"
	case PriorityMedium:
		return "blue"
	case PriorityLow:
		return "gray"
	case PriorityNone:
		return "white"
	default:
		return "white"
	}
}

// Icon returns a Unicode icon representing the priority level.
func (p Priority) Icon() string {
	switch p {
	case PriorityCritical:
		return "⏫"
	case PriorityHigh:
		return "↑"
	case PriorityMedium:
		return "↔"
	case PriorityLow:
		return "↓"
	case PriorityNone:
		return "•"
	default:
		return "•"
	}
}

// IssueKind represents the category of an issue.
type IssueKind string

const (
	IssueKindBug     IssueKind = "bug"
	IssueKindFeature IssueKind = "feature"
	IssueKindTask    IssueKind = "task"
	IssueKindEpic    IssueKind = "epic"
	IssueKindChore   IssueKind = "chore"
)

var validIssueKinds = []IssueKind{
	IssueKindBug,
	IssueKindFeature,
	IssueKindTask,
	IssueKindEpic,
	IssueKindChore,
}

// ValidIssueKinds returns the closed set of issue kinds, in declaration order.
//
// It exists so a check that must reason about the WHOLE set — the routing
// sweep, which asks whether every kind can still bind a workflow — derives it
// from the same slice ValidateIssueKind enforces, rather than hand-copying it.
// A hand-copied set is exactly the drift that let nine `[match].kind` lists
// enumerate the closed set and go stale the moment a sixth kind was added.
//
// The slice is copied on the way out: the closed set is a constant of the
// model, and handing callers the backing array would let one of them append
// to it.
func ValidIssueKinds() []IssueKind {
	out := make([]IssueKind, len(validIssueKinds))
	copy(out, validIssueKinds)
	return out
}

// Icon returns a Unicode icon representing the issue kind.
func (k IssueKind) Icon() string {
	switch k {
	case IssueKindBug:
		return "■"
	case IssueKindFeature:
		return "✦"
	case IssueKindTask:
		return "▶"
	case IssueKindEpic:
		return "⬡"
	case IssueKindChore:
		return "⚒"
	default:
		return "▶"
	}
}

// Color returns a color name string suitable for terminal rendering.
func (k IssueKind) Color() string {
	switch k {
	case IssueKindBug:
		return "red"
	case IssueKindFeature:
		return "green"
	case IssueKindTask:
		return "blue"
	case IssueKindEpic:
		return "magenta"
	case IssueKindChore:
		return "yellow"
	default:
		return "white"
	}
}

// ValidateIssueKind returns an error if k is not a recognized issue kind.
func ValidateIssueKind(k IssueKind) error {
	return validateOneOf(k, validIssueKinds, "issue kind")
}

// displayPrefix is the ISSUE prefix this invocation renders and accepts —
// the invoking project's `projects.prefix` (v12), defaulting to IDPrefix.
//
// It is DISPLAY ONLY. The number is the identity: `VOR-42` and `DKT-42` name
// the same issue, because the id sequence is global (the v12 amendment's
// deliberate call), and the prefix says whose board you are reading at a
// glance. It is a package variable rather than a threaded parameter because
// it is a per-invocation constant, set once by the CLI root hook before any
// command runs — the same lifecycle DefaultAuthor has.
var displayPrefix = IDPrefix

// SetDisplayPrefix installs the invoking project's prefix. The caller is
// responsible for having validated it (ValidateProjectPrefix); an empty value
// keeps the default.
func SetDisplayPrefix(prefix string) {
	if prefix != "" {
		displayPrefix = strings.ToUpper(prefix)
	}
}

// DisplayPrefix reports the effective issue prefix, for surfaces that print
// it (`docket config`).
func DisplayPrefix() string {
	return displayPrefix
}

// reservedProjectPrefixes are the entity prefixes other reference grammars
// own. A project prefixed RUN would make `RUN-3` mean two different things on
// one command line.
var reservedProjectPrefixes = map[string]bool{
	"DOC": true, "RUN": true, "STEP": true,
}

// ValidateProjectPrefix is `project set-prefix`'s gate: 1-8 ASCII letters,
// not an entity prefix another grammar owns. DKT itself is allowed — it is
// the default, and setting it back must not be an error.
func ValidateProjectPrefix(prefix string) error {
	up := strings.ToUpper(strings.TrimSpace(prefix))
	if len(up) < 1 || len(up) > 8 {
		return fmt.Errorf("invalid prefix %q: want 1-8 letters", prefix)
	}
	for _, r := range up {
		if r < 'A' || r > 'Z' {
			return fmt.Errorf("invalid prefix %q: letters only", prefix)
		}
	}
	if reservedProjectPrefixes[up] {
		return fmt.Errorf(
			"prefix %q is reserved: DOC, RUN, and STEP name other entities and a "+
				"project sharing one would make its references ambiguous", up)
	}
	return nil
}

// ownerPrefix resolves an issue id to the prefix of the project that OWNS it,
// or "" when it cannot be resolved. Installed by the CLI root hook; nil for a
// library caller that never wires it, which keeps the pre-DKT-256 behavior.
//
// It is a FUNCTION rather than a preloaded map because most invocations render
// a handful of ids and a few render none: a map would pay for the whole store
// every time. The installed closure memoizes, so a listing of 200 rows costs
// at most 200 indexed lookups and usually far fewer.
var ownerPrefix func(id int) string

// SetIssueOwnerPrefixResolver installs the id -> owning-project-prefix lookup.
// Same lifecycle as SetDisplayPrefix and SetKnownProjectPrefixes: set once by
// the root hook, before any command formats or parses an id.
func SetIssueOwnerPrefixResolver(resolve func(id int) string) {
	ownerPrefix = resolve
}

// FormatID returns the display form of an issue ID, e.g. "DKT-5" — or
// "VOR-5" in a project whose prefix says so.
//
// THE PREFIX COMES FROM THE ROW'S OWN PROJECT, NOT THE READER'S (DKT-256).
// Issue ids are STORE-GLOBAL — DKT-267 and DOT-268 were minted consecutively
// across two different projects — so the prefix is purely a rendering choice,
// and rendering it from the caller's cwd made every cross-project reference
// silently wrong rather than loudly absent. The same `run report` row showed
// DOT-81 read from dotfiles and ART-81 read from artifacts, neither flagged;
// `issue link add DOT-268 relates_to DKT-267` confirmed success as
// "Linked DOT-268 relates_to DOT-267", renaming another project's issue in the
// act of reporting that the right thing had been done.
//
// The reader's prefix remains the fallback, for an id whose owner cannot be
// resolved: a row that renders as it always did is a smaller loss than one that
// renders as `-42`.
func FormatID(id int) string {
	if ownerPrefix != nil {
		if owner := ownerPrefix(id); owner != "" {
			return fmt.Sprintf("%s-%d", strings.ToUpper(owner), id)
		}
	}
	return fmt.Sprintf("%s-%d", displayPrefix, id)
}

// FormatIDWithPrefix renders an issue id under a NAMED project's prefix rather
// than the invoking project's (DKT-67).
//
// The process-wide prefix is right for every surface that shows one project's
// work, and wrong for the one surface that shows several: `events list
// --all-projects` rendered every id it found under the querying project's
// prefix, so a feed whose whole purpose is to span projects displayed six
// sibling projects' issues as if they belonged to the caller. The prefix is the
// only project discriminator such a row carries, which made it wrong in exactly
// the place it had to be right.
//
// An empty prefix falls back to the process default, so a row whose project
// cannot be resolved renders as it always did rather than as `-42`.
func FormatIDWithPrefix(id int, prefix string) string {
	if prefix == "" {
		return FormatID(id)
	}
	return fmt.Sprintf("%s-%d", strings.ToUpper(prefix), id)
}

// ParseID accepts a bare number, "DKT-5", or the invoking project's own
// prefix ("VOR-5"), case-insensitively, and returns the numeric ID.
//
// DKT ALWAYS PARSES, whatever the project prefix: the number is the global
// identity, ids travel between projects in run records and commit messages,
// and a reference that stopped resolving because a display setting changed
// would be the display leaking into identity. len() slicing is safe because
// both prefixes are ASCII and ToUpper preserves their byte length.
func ParseID(input string) (int, error) {
	id, err := parseEntityID(input, "issue", issueCutPrefixes(input)...)
	if err != nil {
		return 0, err
	}
	if err := checkNamedPrefixOwnsIssue(input, id); err != nil {
		return 0, err
	}
	return id, nil
}

// checkNamedPrefixOwnsIssue refuses a prefixed reference whose prefix does not
// match the resolved row's owning project (DKT-256).
//
// The parser above strips ANY registered prefix and resolves by digits alone,
// which is right for the identity — the number is store-wide — and wrong for
// the reference. Live on nightly-31-g8083dea, cwd = docket.git:
//
//	$ docket issue show DOT-20
//	■ DKT-20  schema/workflow register writes land under project_id=1 ...
//
// DOT-20 is a real issue in a different project. The lookup discarded the
// prefix, resolved 20 under the caller's project, and rendered the result under
// DKT with no warning: the reader asked about one issue and was shown another.
// DKT-110 tightened the prefix SHAPE — `ANSI-20` now errors — but any prefix a
// registered project holds still resolved by digits.
//
// CROSS-PROJECT READS STAY LEGAL. `issue list --project` prints other projects'
// ids precisely so they round-trip (DKT-72), and with FormatID rendering from
// the owner those ids are now correct. What this forbids is only the third
// outcome: a DIFFERENT issue wearing the requested number.
//
// It is silent in three cases, each deliberate: a bare number names no project
// and asserts nothing; an uninstalled resolver is a library caller keeping the
// pre-DKT-256 behavior; and an unresolvable owner declines to guess rather than
// refusing a reference it cannot judge.
func checkNamedPrefixOwnsIssue(input string, id int) error {
	if ownerPrefix == nil {
		return nil
	}
	dash := strings.IndexByte(input, '-')
	if dash <= 0 {
		return nil
	}
	named := strings.ToUpper(strings.TrimSpace(input[:dash]))
	if named == "" || knownProjectPrefixes == nil || !knownProjectPrefixes[named] {
		return nil
	}
	owner := strings.ToUpper(ownerPrefix(id))
	if owner == "" || owner == named {
		return nil
	}
	return fmt.Errorf(
		"%s-%d does not exist: issue %d belongs to %s, not %s. Both prefixes "+
			"name registered projects and issue ids are store-wide, so the "+
			"number alone would have resolved a different issue than you asked "+
			"for — read it as %s-%d, or check the id",
		named, id, id, owner, named, owner, id)
}

// knownProjectPrefixes is the store's registered prefix roster, installed by
// the CLI root hook (DKT-110). nil means "not installed" — a library caller
// that never wires it keeps the pre-roster behavior — while an installed
// roster, empty included, is authoritative: a prefix outside it does not cut.
var knownProjectPrefixes map[string]bool

// SetKnownProjectPrefixes installs the roster of prefixes registered in the
// store, uppercased. Same lifecycle as SetDisplayPrefix: set once by the root
// hook before any command parses an id.
func SetKnownProjectPrefixes(prefixes []string) {
	roster := make(map[string]bool, len(prefixes))
	for _, p := range prefixes {
		if p != "" {
			roster[strings.ToUpper(p)] = true
		}
	}
	knownProjectPrefixes = roster
}

// issueCutPrefixes is the set of leading `PREFIX-` forms ParseID strips.
//
// The invoking project's prefix and `DKT-` are the two that have always
// parsed. ANY OTHER PROJECT'S REGISTERED PREFIX PARSES TOO (DKT-72), because
// the number is the store-wide identity and the prefix is display only —
// refusing `AMS-2` while accepting `DKT-2` and bare `2` for the same issue is
// exactly the display leaking into identity that this type's own comments
// forbid.
//
// It became reachable when `issue list --project` started printing other
// projects' ids: the listing showed `AMS-2` and the very next command could not
// take it back, which is a round-trip an operator reasonably expects and a trap
// the listing itself set.
//
// REGISTERED is the operative word (DKT-110). Cutting any letters-shaped
// prefix made every hyphenated uppercase term in prose an id: `ANSI-16` and
// `ZZZ-16` both resolved issue 16, so a citation gate validated non-citations
// against whatever issue happened to share the digits. A caller that supplied
// a prefix stated an expectation, and a prefix NO project holds is honored by
// refusal: it is left in place so Atoi reports the invalid id it is. The
// roster is consulted when the CLI installed one; a caller that never wired it
// keeps the permissive behavior, exactly as an unwired display prefix keeps
// `DKT`.
//
// RESERVED PREFIXES ARE STILL REFUSED. `DOC-7`, `RUN-3`, and `STEP-12` name
// other entities, and accepting them here would make `docket issue show RUN-3`
// silently show issue 3 — the ambiguity ValidateProjectPrefix exists to
// prevent, reintroduced at the parser instead of at the setter. A prefix that
// is not letters, or that is one of those three, is left in place so Atoi
// reports it as the invalid id it is.
func issueCutPrefixes(input string) []string {
	cuts := []string{displayPrefix + "-", IDPrefix + "-"}

	dash := strings.IndexByte(input, '-')
	if dash <= 0 {
		return cuts
	}
	candidate := strings.ToUpper(strings.TrimSpace(input[:dash]))
	if reservedProjectPrefixes[candidate] {
		return cuts
	}
	if ValidateProjectPrefix(candidate) != nil {
		return cuts
	}
	if knownProjectPrefixes != nil && !knownProjectPrefixes[candidate] {
		return cuts
	}
	return append(cuts, candidate+"-")
}

// Issue represents a tracked issue.
type Issue struct {
	ID int
	// ProjectID is the v12 tenancy dimension. Zero means "the default
	// project" on write (the db layer normalizes it), so pre-v12 callers and
	// fixtures need no change. Like Version, it never reaches the frozen v1
	// wire format.
	ProjectID   int
	ParentID    *int
	Title       string
	Description string
	Status      Status
	Priority    Priority
	Kind        IssueKind
	Assignee    string
	Labels      []string
	Files       []string
	Docs        []DocRef
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Version is the optimistic-concurrency counter (schema v5). It is
	// deliberately NOT emitted by MarshalJSON: adding a field to the v1
	// envelope would break byte-compatibility for every existing verb
	// (engine-spec.md §9 item 8). Use WithVersion to surface it under
	// --json=v2.
	Version int
	// Lease is the capability lease held on this issue (schema v6), or nil
	// when unclaimed. Like Version it never reaches the v1 wire format; it
	// surfaces under --json=v2 and only when a lease is actually held, so an
	// unclaimed issue's v2 output is unchanged too.
	Lease *Lease
	// Scope is the declared scope globs (`issues.scope_globs`, schema v7), or
	// nil when none is declared. NULL and `[]` are different facts — "no scope
	// declared" versus "declared to touch nothing" — so the pointer carries
	// the distinction: nil, &[], &[...].
	//
	// DKT-55 amendment to the v1 freeze: `scope` reaches the v1 wire WHEN
	// DECLARED, and only then. An issue with no declared scope marshals
	// byte-identically to the pre-scope era, so dormancy holds for every repo
	// that never used --scope — while a declared scope is no longer invisible
	// on the surface everyone checks first.
	Scope *[]string
	// Resolution is how the machine left the issue, when a routing decided
	// something about it that `status` cannot express (`issues.resolution`,
	// schema v18). Empty means no routing has resolved it.
	//
	// `abandon-issue` is why it exists: the routing stops a RUN's work on an
	// issue without forcing the issue's status, deliberately, so the operator
	// keeps their triage decision — but the issue whose fix step completed is
	// already `done`, and with nothing else recorded the tracker rendered
	// "✔ done" for work a review had reproduced as not fixing anything
	// (DKT-245). Following DKT-55's precedent, it reaches the v1 wire WHEN
	// SET, and only then.
	Resolution string
}

// issueJSONV2 is the v2 wire format: the v1 shape plus the CAS version, plus
// the lease when one is held.
//
// Lease is `omitempty` and typed `any` so an unclaimed issue emits no `lease`
// key at all rather than `"lease":null` — the field appears exactly when there
// is a lease to report.
type issueJSONV2 struct {
	issueJSON
	Version int `json:"version"`
	Lease   any `json:"lease,omitempty"`
}

// VersionedIssue wraps an Issue so it marshals with its `version` field, for
// --json=v2 responses (engine-spec.md §5, "versions in .data").
type VersionedIssue struct{ Issue }

// MarshalJSON emits the v1 issue shape plus `version`, and `lease` when a
// lease is held.
//
// The lease's `live` flag is computed here, at marshal time, from the current
// clock — engine-spec.md §2 requires read verbs to render *effective* status
// with expiry computed at read time and no write. An expired lease therefore
// reads as dead the instant it lapses, without anyone having called claim.
func (v VersionedIssue) MarshalJSON() ([]byte, error) {
	base, err := v.Issue.marshalJSONStruct()
	if err != nil {
		return nil, err
	}
	return json.Marshal(issueJSONV2{
		issueJSON: base,
		Version:   v.Issue.Version,
		Lease:     v.Issue.Lease.LeaseWire(NowMS()),
	})
}

// WithVersion wraps issues for v2 marshaling. Callers pass the result as the
// v2 payload; the v1 path keeps using the bare Issue values.
func WithVersion(issues []*Issue) []VersionedIssue {
	return withVersionSlice(issues, func(i *Issue) VersionedIssue {
		return VersionedIssue{Issue: *i}
	})
}

// issueJSON is the JSON wire format for Issue.
type issueJSON struct {
	ID string `json:"id"`
	// Issue is `id` again, under the noun the rest of the wire surface uses
	// for a primary key (DKT-452). `run status` keys its run as `run`, `step
	// show` keys its step as `step`, `dispatch open` keys its dispatch as
	// `dispatch`, and `issue claim` / `release` / `heartbeat` already key
	// theirs as `issue` — only the issue READ verbs broke the pattern by
	// keying `id` and nothing else. A conductor that had just parsed a run or
	// a step therefore reached for `.data.issue`, got null, and had to dump
	// the key set to recover; the guess is cheap to be right about and the
	// recovery is not, so both keys carry the id and neither is wrong.
	//
	// Amendment to the v1 freeze, on DKT-55's precedent but broader: this key
	// is unconditional, so v1 is no longer byte-identical to the pre-DKT-452
	// shape. That is the deliberate trade — an ADDED key cannot break a
	// consumer that selects `.data.id` (every existing reader keeps reading
	// exactly what it read), while a conditional alias would be absent in
	// precisely the case the alias exists to serve.
	Issue       string   `json:"issue"`
	ParentID    *string  `json:"parent_id,omitempty"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    string   `json:"priority"`
	Kind        string   `json:"kind"`
	Assignee    string   `json:"assignee"`
	Labels      []string `json:"labels"`
	Files       []string `json:"files"`
	Docs        []DocRef `json:"docs"`
	// Scope appears ONLY when the issue declares one (DKT-55): a nil pointer
	// emits no key, keeping scope-less issues byte-identical to the frozen v1
	// shape, while a declared scope — the fact an operator actually checks
	// for — is visible on the default surface.
	Scope *[]string `json:"scope,omitempty"`
	// Resolution appears ONLY when a routing has set one, on DKT-55's
	// precedent: an unresolved issue marshals byte-identically to the frozen
	// v1 shape, while an abandoned one stops being indistinguishable from a
	// finished one (DKT-245).
	Resolution string `json:"resolution,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// MarshalJSON implements custom JSON serialization for Issue.
// This is the frozen v1 wire format, with the amendments recorded on issueJSON:
// `scope` (DKT-55) and `resolution` (DKT-245) are emitted when — and only when
// — they are set, so the dormant shape stays byte-identical, and `issue` mirrors
// `id` unconditionally (DKT-452).
func (i Issue) MarshalJSON() ([]byte, error) {
	j, err := i.marshalJSONStruct()
	if err != nil {
		return nil, err
	}
	return json.Marshal(j)
}

// marshalJSONStruct builds the v1 wire struct. Shared by MarshalJSON and the
// v2 wrapper so the two shapes cannot drift apart.
func (i Issue) marshalJSONStruct() (issueJSON, error) {
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
		docs = []DocRef{}
	}

	id := FormatID(i.ID)
	j := issueJSON{
		ID:          id,
		Issue:       id,
		Title:       i.Title,
		Description: i.Description,
		Status:      string(i.Status),
		Priority:    string(i.Priority),
		Kind:        string(i.Kind),
		Assignee:    i.Assignee,
		Labels:      labels,
		Files:       files,
		Docs:        docs,
		Scope:       i.Scope,
		Resolution:  i.Resolution,
		CreatedAt:   i.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   i.UpdatedAt.UTC().Format(time.RFC3339),
	}

	if i.ParentID != nil {
		pid := FormatID(*i.ParentID)
		j.ParentID = &pid
	}

	return j, nil
}

// UnmarshalJSON implements custom JSON deserialization for Issue.
func (i *Issue) UnmarshalJSON(data []byte) error {
	var j issueJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	id, err := ParseID(j.ID)
	if err != nil {
		return fmt.Errorf("parsing issue id: %w", err)
	}
	i.ID = id

	if j.ParentID != nil {
		pid, err := ParseID(*j.ParentID)
		if err != nil {
			return fmt.Errorf("parsing parent id: %w", err)
		}
		i.ParentID = &pid
	}

	i.Title = j.Title
	i.Description = j.Description
	i.Status = Status(j.Status)
	if err := ValidateStatus(i.Status); err != nil {
		return err
	}

	i.Priority = Priority(j.Priority)
	if err := ValidatePriority(i.Priority); err != nil {
		return err
	}

	i.Kind = IssueKind(j.Kind)
	if err := ValidateIssueKind(i.Kind); err != nil {
		return err
	}

	i.Assignee = j.Assignee
	i.Labels = j.Labels
	i.Files = j.Files
	i.Scope = j.Scope

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	i.CreatedAt = createdAt

	updatedAt, err := time.Parse(time.RFC3339, j.UpdatedAt)
	if err != nil {
		return fmt.Errorf("parsing updated_at: %w", err)
	}
	i.UpdatedAt = updatedAt

	return nil
}

type IssueRef struct {
	ID     int
	Kind   string
	Status string
	Title  string
}

type issueRefJSON struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

func (r IssueRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(issueRefJSON{
		ID:     FormatID(r.ID),
		Kind:   r.Kind,
		Title:  r.Title,
		Status: r.Status,
	})
}

func (r *IssueRef) UnmarshalJSON([]byte) error {
	return nil
}
