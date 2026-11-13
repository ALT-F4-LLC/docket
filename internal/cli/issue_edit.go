package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var editCmd = &cobra.Command{
	Use:   "edit [id]",
	Short: "Edit an existing issue",
	Long: `Edit an existing issue.

A MID-RUN EDIT DOES NOT REACH AN ALREADY-ACTIVATED RUN'S PACKETS. Activation
freezes the issue's description, title, kind, labels and scope into the run,
and every packet renders from that snapshot — so editing --description or
--scope here changes what the NEXT activation will freeze, never what a step
already in flight will read.

--scope is the one whose live column still does something: the scheduler reads
it live for its mutual-exclusion check, so a correction takes effect against
collisions immediately. It does not change any live step's rendered scope or
the paths that step's diff is recorded over. To make a widened scope real for
work already in a run, refresh that run's snapshot as a second, explicit act —
` + "`docket run refresh-scope RUN-N --issue DKT-M --reason R`" + ` — or, where the
run's premise changed rather than one declaration, take the issue out of the
run and re-plan it. This command warns, naming the run and both verbs, when a
--scope edit lands on an issue with live steps in a live run.

--description has no live reader: every packet's REQUEST and issue.body render
the activation snapshot. To make an authorized amendment reach an activated
run's remaining steps, refresh that snapshot as the same kind of second act —
` + "`docket run refresh-body RUN-N --issue DKT-M --reason R`" + `. This command
warns, naming the run and that verb, when a --description edit lands on an
issue with live steps in a live run.`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runIssueEdit(cmd, args, getWriter(cmd))
	},
}

// runIssueEdit is the verb's body, taking its Writer explicitly the way every
// `watchable` verb's does. The advisory this verb now raises (DKT-741) travels
// on stderr, and a test that could not substitute that stream would have to
// assert the warning somewhere other than where an operator reads it.
func runIssueEdit(cmd *cobra.Command, args []string, w *output.Writer) error {
	in := verb.IssueEditInput{
		Issue:    args[0],
		Title:    changedString(cmd, "title"),
		Status:   changedString(cmd, "status"),
		Priority: changedString(cmd, "priority"),
		// An explicit `--size ""` clears the declaration back to SizeNone,
		// the same "changed and empty means clear" reading `--parent 0` and
		// `--scope=` use.
		Size:     changedString(cmd, "size"),
		Kind:     changedString(cmd, "type"),
		Assignee: changedString(cmd, "assignee"),
		Parent:   changedString(cmd, "parent"),
		Scope:    scopeOf(cmd),
	}

	if description := changedString(cmd, "description"); description != nil {
		if *description == "-" {
			const maxStdinSize = 1 << 20 // 1 MiB
			data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinSize))
			if err != nil {
				return cmdErr(fmt.Errorf("reading description from stdin: %w", err), output.ErrGeneral)
			}
			*description = strings.TrimRight(string(data), "\n")
		}
		in.Description = description
	}

	if cmd.Flags().Changed("file") {
		files, _ := cmd.Flags().GetStringSlice("file")
		in.Files = &files
	}

	ifVersion, err := ifVersionOf(cmd)
	if err != nil {
		return err
	}
	in.IfVersion = ifVersion

	res, err := verb.IssueEdit(cmd.Context(), verbEnv(cmd), in)
	recordProjectRegistration(cmd, res.Registered)
	// DKT-741: the advisories name the live runs whose frozen snapshot the
	// edit did not reach, on BOTH CHANNELS: the stderr line for a human
	// (suppressed in JSON mode by design) and `warnings` in the envelope for
	// the relay that ran the edit. They travel even when the update itself
	// is then refused, because the scope write before it has landed.
	for _, warning := range res.Warnings {
		w.Warn("%s", warning)
	}
	if err != nil {
		return verbErr(err)
	}

	if !res.Changed {
		if w.JSONMode {
			w.Success(withIssueVersion(res.Issue), "")
		} else {
			w.Info("No changes specified")
		}
		return nil
	}

	updated := res.Issue
	w.Success(
		withEditWarnings(withIssueVersion(updated), res.Warnings),
		fmt.Sprintf("Updated %s: %s", model.FormatID(updated.ID), updated.Title))

	return nil
}

// changedString returns a string flag's value when it was given on the
// command line, and nil otherwise, so an edit leaves that field alone.
func changedString(cmd *cobra.Command, name string) *string {
	if !cmd.Flags().Changed(name) {
		return nil
	}
	value, _ := cmd.Flags().GetString(name)
	return &value
}

// editWarningsPayload is the edited issue plus the advisories the edit raised
// (DKT-741), for the JSON reader that `w.Warn` deliberately does not reach.
//
// It SPLICES a key into whatever shape the wrapped payload already marshals
// to, rather than being a new shape of its own, so v1 and v2 each keep their
// own issue rendering and gain the same one additive key. With no warnings it
// is not constructed at all (withEditWarnings returns the payload unchanged),
// so every edit that raises none emits byte-identical JSON to what it always
// did — including the field ORDER a re-marshal here would otherwise sort.
type editWarningsPayload struct {
	inner    any
	warnings []string
}

func (p editWarningsPayload) MarshalJSON() ([]byte, error) {
	return spliceWarnings(json.Marshal(p.inner))(p.warnings)
}

// VersionedPayload implements output.Versioned: under v2 the inner payload's
// own v2 rendering carries the warnings, so neither envelope loses them.
func (p editWarningsPayload) VersionedPayload() any {
	if v, ok := p.inner.(output.Versioned); ok {
		return editWarningsPayload{inner: v.VersionedPayload(), warnings: p.warnings}
	}
	return p
}

// withEditWarnings wraps payload only when there is something to say.
func withEditWarnings(payload any, warnings []string) any {
	if len(warnings) == 0 {
		return payload
	}
	return editWarningsPayload{inner: payload, warnings: warnings}
}

// spliceWarnings adds a `warnings` array to an already-marshaled JSON object.
// Curried so the (bytes, error) pair of a Marshal call feeds it directly.
func spliceWarnings(raw []byte, err error) func([]string) ([]byte, error) {
	return func(warnings []string) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("adding warnings to the issue payload: %w", err)
		}
		encoded, err := json.Marshal(warnings)
		if err != nil {
			return nil, fmt.Errorf("encoding warnings: %w", err)
		}
		fields["warnings"] = encoded
		return json.Marshal(fields)
	}
}

func init() {
	editCmd.Flags().StringP("title", "t", "", "Issue title")
	editCmd.Flags().StringP("description", "d", "", "Issue description (use \"-\" for stdin)")
	editCmd.Flags().StringP("status", "s", "", "Issue status")
	editCmd.Flags().StringP("priority", "p", "", "Issue priority")
	editCmd.Flags().String("size", "", "Issue size (trivial, small, bounded, needs-design, unknown; empty clears it)")
	editCmd.Flags().StringP("type", "T", "", "Issue type")
	editCmd.Flags().StringP("assignee", "a", "", "Issue assignee")
	editCmd.Flags().StringSliceP("file", "f", nil, "File paths (repeatable, replaces existing)")
	editCmd.Flags().String("parent", "", "Parent issue ID (use \"0\" or \"none\" to make root)")
	addScopeFlag(editCmd)
	addIfVersionFlag(editCmd)
	issueCmd.AddCommand(editCmd)
}
