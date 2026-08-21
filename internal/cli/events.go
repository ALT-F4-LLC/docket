package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket events list --since` — the §11.4 event shape as a read surface
// (docs/tdd/runs-dispatch.md §8).
//
// WHY `events list` AND NOT BARE `docket events` (§8.1). engine-spec §1's
// summary writes the verb as `docket events --follow [--since SEQ]`, which reads
// as a bare verb with flags. Shipping the read half as bare `docket events` would
// mean S7's `--follow` changes the DEFAULT behavior of an existing verb — or
// worse, that `docket events` with no flags means "list" now and something else
// later. `events list` is chosen, with bare `docket events` printing help, so S7
// adds `events --follow` over the same rows without redefining anything.
//
// That is a NOMINAL DEVIATION from §1's rendering and it is RECORDED,
// not made silently — the same class as the `step`/`issue` subject-key
// note, and resolved the same way: the shape is satisfied, the spelling is
// recorded.

// eventsDefaultLimit is E9's default page size.
//
// A default exists at all because this is a CURSOR feed: a consumer polling it
// wants a bounded page and a `total` telling it whether to poll again
// immediately. An unbounded default would hand a first-time caller the whole
// repo history.
const eventsDefaultLimit = 100

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Read the engine's event log",
	Long: `The event log records every transition the engine made.

` + "`docket events list --since SEQ`" + ` returns events with a seq STRICTLY
GREATER than the cursor, oldest first, so a consumer stores the last seq it saw
and passes it back without re-reading it. A concurrent write lands above the
cursor and arrives on the next call: no event is skipped and none is returned
twice.

A cursor naming events that no longer exist is GONE (exit 9) rather than a
silently short answer.`,
}

var eventsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List events after a cursor",
	Long: `List events with seq > --since, oldest first.

Each event is {seq, at_ms, kind, run?, step?, step_id?, data}. ` + "`data`" + ` is the
transition's own opaque payload, carried verbatim.

Without --run the feed is repo-wide, which is the only place events that belong
to no run — trust grants — are visible.

--tail N returns the NEWEST N events instead of the oldest N, for the
mid-incident question "what just happened". They still arrive oldest-first, so
the last seq is still the cursor to store. --tail and --since are mutually
exclusive: one jumps to the end of the feed, the other walks it forward.

Human mode escapes stored strings on their way to the terminal; --json carries
the raw bytes, because the consumer there is a program.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if follow, _ := cmd.Flags().GetBool("follow"); follow {
			return runEventsFollow(cmd)
		}
		return runEventsList(cmd, getWriter(cmd))
	},
}

// eventsListResult is a Collection (E11), so `--json=v2` yields
// `{items, total, truncated}` uniformly with every other list verb, and v1 is
// the bare array (E12).
type eventsListResult struct {
	Events []engine.Event `json:"events"`
	Total  int            `json:"total"`
	limit  int
}

func (r eventsListResult) CollectionItems() any { return eventsListPayload{events: r.Events} }
func (r eventsListResult) CollectionTotal() int { return r.Total }
func (r eventsListResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.Total, len(r.Events))
}

var _ output.Collection = eventsListResult{}

// eventsListPayload emits the v1 array shape, the same wrapper every other list
// verb uses so a bare slice does not render v1 items inside a v2 envelope.
// eventsProjectScope resolves the feed's project scope (v12): the invoking
// project by default, the whole store under --all-projects. Store-level
// events — trust changes — appear either way; see EventQuery.ProjectID.
func eventsProjectScope(cmd *cobra.Command) int {
	if all, _ := cmd.Flags().GetBool("all-projects"); all {
		return 0
	}
	return getProjectID(cmd)
}

type eventsListPayload struct{ events []engine.Event }

func (p eventsListPayload) MarshalJSON() ([]byte, error) {
	if p.events == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(p.events)
}

func runEventsList(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	since, _ := cmd.Flags().GetInt64("since")
	if since < 0 {
		return cmdErr(
			fmt.Errorf("--since must not be negative: %d", since), output.ErrValidation)
	}
	limit, _ := cmd.Flags().GetInt("limit")
	if err := validateLimit(cmd, limit); err != nil {
		return err
	}
	if limit == 0 {
		limit = eventsDefaultLimit
	}

	tail, _ := cmd.Flags().GetInt("tail")
	if tail < 0 {
		return cmdErr(
			fmt.Errorf("--tail must not be negative: %d", tail), output.ErrValidation)
	}
	// `--tail` answers "what just happened" and `--since` answers "what have I
	// not seen". Combining them reads as a cursor but silently skips whatever
	// falls between the cursor and the tail window, so it is refused rather
	// than served with a plausible-looking short page.
	if tail > 0 && cmd.Flags().Changed("since") {
		return cmdErr(
			fmt.Errorf("--tail and --since are mutually exclusive: "+
				"--since walks the feed forward, --tail jumps to its end"),
			output.ErrValidation)
	}

	runRef, _ := cmd.Flags().GetString("run")
	runID, err := engine.ResolveRunFilter(conn, runRef)
	if err != nil {
		return runErr(err)
	}

	page, err := engine.ListEvents(conn, engine.EventQuery{
		Since: since, RunID: runID, Limit: limit, Tail: tail,
		ProjectID: eventsProjectScope(cmd),
	})
	if err != nil {
		return runErr(err)
	}

	// Under `--tail` the effective window is N, not `--limit`, so the
	// truncation flag is computed against the bound that actually applied.
	effectiveLimit := limit
	if tail > 0 {
		effectiveLimit = tail
	}
	result := eventsListResult{Events: page.Events, Total: page.Total, limit: effectiveLimit}

	var message string
	if !w.JSONMode {
		// The project column appears only in the cross-project view: in a
		// scoped feed every row has the same project, and a column that repeats
		// one value down the page costs width without saying anything.
		allProjects, _ := cmd.Flags().GetBool("all-projects")
		message = renderEventList(page.Events, allProjects)
	}
	w.Success(result, message)
	return nil
}

// renderEventList is E13: one line per event, `seq  at  kind  run  issue
// instance detail`, columns aligned.
//
// EVERY STORED STRING GOES THROUGH exec.Render (§8.5, gates-trust §5.7 T18).
// `data` carries `--usage` blobs, gate names, and operator-supplied abandon
// reasons — attacker-influenced text on its way to a TERMINAL, which interprets
// control bytes. `--json` carries the RAW bytes, because encoding/json escapes
// them by contract and the consumer there is a program (E4 of that section).
//
// The kind and the run are NOT escaped: both are core's own closed vocabulary
// (the event set, and `RUN-N`), so there is no untrusted byte to escape and
// quoting them would make every line noisier for no gain. The issue is the
// same closed vocabulary (`DKT-N`) for the same reason.
//
// The PROJECT NAME is escaped, because unlike the columns beside it, it is not
// core's vocabulary: it is derived from a directory name on disk.
func renderEventList(events []engine.Event, withProject bool) string {
	if len(events) == 0 {
		return "No events."
	}

	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "%-8d %-13d %-20s", e.Seq, e.AtMS, e.Kind)
		if withProject {
			// The DASH IS CHOSEN BEFORE RENDERING, not after: exec.Render("")
			// returns a quoted empty string, which dashIfEmpty does not see as
			// empty — so a store-level event's project column printed `""`
			// rather than holding the column open like every other one.
			project := "-"
			if e.Project != "" {
				project = exec.Render(e.Project)
			}
			fmt.Fprintf(&b, " %-20s", project)
		}
		// A trust event has no run, and any event may have no issue; the column
		// is held open with "-" rather than collapsed so unrelated rows stay
		// aligned down the page.
		fmt.Fprintf(&b, " %-8s", dashIfEmpty(e.Run))
		fmt.Fprintf(&b, " %-8s", dashIfEmpty(e.Issue))
		if e.Step != "" {
			fmt.Fprintf(&b, " %s", exec.Render(e.Step))
		}
		if detail := eventDetail(e.Data); detail != "" {
			fmt.Fprintf(&b, " %s", exec.Render(detail))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// dashIfEmpty renders "-" for an empty column so unrelated rows stay aligned,
// rather than collapsing the field width. internal/render's step.go has the
// same one-liner under the same name, but unexported, so it is not reachable
// from this package; this is its own copy for that reason.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// eventDetail renders `data` compactly for the human line (E13).
//
// It drops `instance`, which the writer merges into every step-shaped event's
// payload and which the `step` column already shows — repeating it would take a
// third of the line to say the same thing twice.
//
// The keys are SORTED, because encoding/json sorts map keys and a detail column
// that reordered itself between two reads of the same event would make the feed
// undiffable. Nothing here interprets a key: they are opaque strings rendered in
// a total order, which is the genericity line the report's metadata rollup
// holds too.
func eventDetail(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		// Unreachable through the writer, which normalizes `data` to an object
		// (§7.6). Reachable through a hand-edited row, and rendering the raw
		// bytes is a better answer than dropping the line: an operator looking
		// at a corrupted feed should see that it is corrupted.
		return string(data)
	}
	delete(fields, "instance")
	if len(fields) == 0 {
		return ""
	}

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strings.Trim(string(fields[k]), `"`))
	}
	return strings.Join(parts, " ")
}

func init() {
	eventsListCmd.Flags().Int64(
		"since", 0, "Return events with seq strictly greater than this cursor")
	eventsListCmd.Flags().String("run", "", "Filter to one run (RUN-N)")
	eventsListCmd.Flags().Bool(
		"all-projects", false,
		"Show every project's events instead of this project's (store-level events show either way)")
	// `--tail N` is the mid-incident read: the cursor answers "what
	// have I not seen", which to reach the END of a long feed means paging
	// through all of it first. The rows still arrive oldest-first.
	eventsListCmd.Flags().Int(
		"tail", 0, "Return the newest N events (still oldest-first); excludes --since")
	eventsListCmd.Flags().Int(
		"limit", 0, fmt.Sprintf("Maximum events to return (default %d)", eventsDefaultLimit))

	// `--follow` (docs/tdd/events-follow.md §4). engine-spec §1 writes the verb
	// as `docket events --follow [--since SEQ]`; the flag lands on `list`
	// because that is where the rows are (§8.1's sub-verb argument).
	eventsListCmd.Flags().Bool(
		"follow", false, "Poll for new events and print them as they arrive (Ctrl-C to stop)")

	// `--interval` IS NOT DECLARED HERE. It already exists as a PERSISTENT root
	// flag serving `--watch`, with a 500ms floor enforced in PersistentPreRunE
	// and a hide mechanism (watch_commands.go) that shows it only on eligible
	// commands. Declaring a second one on this command would shadow the global
	// with different validation — two flags of one name, one of which silently
	// bypasses the other's floor.
	//
	// So `events list` joins `watchEligible` instead, and `--follow` reads the
	// flag that was already there. The poll period therefore has ONE definition,
	// one default, and one minimum, across every verb that polls.

	eventsCmd.AddCommand(eventsListCmd)
	rootCmd.AddCommand(eventsCmd)
}
