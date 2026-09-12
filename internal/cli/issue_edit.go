package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
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
--scope edit lands on an issue with live steps in a live run.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runIssueEdit(cmd, args, getWriter(cmd))
	},
}

// runIssueEdit is the verb's body, taking its Writer explicitly the way every
// `watchable` verb's does. The advisory this verb now raises (DKT-741) travels
// on stderr, and a test that could not substitute that stream would have to
// assert the warning somewhere other than where an operator reads it.
func runIssueEdit(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := issueArg(args[0])
	if err != nil {
		return err
	}

	// Verify issue exists. The row is kept: reparenting compares projects
	// issue-to-issue, so the edited issue's own home is needed below.
	issue, err := getIssueOrErr(conn, id, fmt.Sprintf("issue %s", args[0]))
	if err != nil {
		return err
	}

	updates := make(map[string]interface{})
	filesChanged := false

	if cmd.Flags().Changed("title") {
		title, _ := cmd.Flags().GetString("title")
		updates["title"] = title
	}

	if cmd.Flags().Changed("description") {
		description, _ := cmd.Flags().GetString("description")
		if description == "-" {
			const maxStdinSize = 1 << 20 // 1 MiB
			data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinSize))
			if err != nil {
				return cmdErr(fmt.Errorf("reading description from stdin: %w", err), output.ErrGeneral)
			}
			description = strings.TrimRight(string(data), "\n")
		}
		updates["description"] = description
	}

	if cmd.Flags().Changed("status") {
		status, _ := cmd.Flags().GetString("status")
		if err := model.ValidateStatus(model.Status(status)); err != nil {
			return cmdErr(err, output.ErrValidation)
		}
		updates["status"] = status
	}

	if cmd.Flags().Changed("priority") {
		priority, _ := cmd.Flags().GetString("priority")
		if err := model.ValidatePriority(model.Priority(priority)); err != nil {
			return cmdErr(err, output.ErrValidation)
		}
		updates["priority"] = priority
	}

	if cmd.Flags().Changed("type") {
		kind, _ := cmd.Flags().GetString("type")
		if err := model.ValidateIssueKind(model.IssueKind(kind)); err != nil {
			return cmdErr(err, output.ErrValidation)
		}
		updates["kind"] = kind
	}

	if cmd.Flags().Changed("assignee") {
		assignee, _ := cmd.Flags().GetString("assignee")
		updates["assignee"] = assignee
	}

	if cmd.Flags().Changed("file") {
		fileFlag, _ := cmd.Flags().GetStringSlice("file")
		if err := db.SetIssueFiles(conn, id, fileFlag, config.DefaultAuthor()); err != nil {
			return cmdErr(fmt.Errorf("setting files: %w", err), output.ErrGeneral)
		}
		filesChanged = true
	}

	if cmd.Flags().Changed("parent") {
		parent, _ := cmd.Flags().GetString("parent")
		if strings.EqualFold(parent, "0") || strings.EqualFold(parent, "none") {
			updates["parent_id"] = nil
		} else {
			// The parent must exist AND share the edited issue's project
			// (DKT-22) — issue-to-issue, never against the invoking cwd.
			parentIssue, err := resolveParentIssue(conn, parent, issue.ProjectID)
			if err != nil {
				return err
			}
			newParentID := parentIssue.ID
			if newParentID == id {
				return cmdErr(fmt.Errorf("cannot set parent to self"), output.ErrValidation)
			}
			isCycle, err := db.IsDescendant(conn, id, newParentID)
			if err != nil {
				return cmdErr(fmt.Errorf("checking for cycles: %w", err), output.ErrGeneral)
			}
			if isCycle {
				return cmdErr(fmt.Errorf("cannot reparent: would create a cycle"), output.ErrConflict)
			}
			updates["parent_id"] = newParentID
		}
	}

	// `--scope` counts as a change. Without this the early return below
	// would report "No changes specified" for a scope-only edit and, in
	// JSON mode, emit the issue unchanged — after having written the
	// scope. Scope lives on its own column and never enters `updates`,
	// so it has to be counted here explicitly.
	scopeChanged := cmd.Flags().Changed(scopeFlag)
	if err := applyScope(cmd, conn, id); err != nil {
		return err
	}

	// DKT-741: the write above landed on `issues.scope_globs` and reached
	// no already-activated run — packets and recorded diffs read the
	// activation snapshot (§5.1.1), and no EDIT refreshes it (DKT-869's
	// `run refresh-scope` is a separate, explicitly-named second act, which
	// is what the advisory now points at). Named AFTER the write because
	// the answer is a property of the run's frozen snapshot, which the
	// write did not touch; an operator who meant to widen scope to unblock
	// live work needs to learn here that this edit alone did not do it.
	//
	// BOTH CHANNELS, the way every advisory in this CLI travels: the
	// stderr line for a human (suppressed in JSON mode by design), and
	// `warnings` in the envelope for the relay that ran the edit — which
	// is who ran it when this was found.
	var scopeWarnings []string
	if scopeChanged {
		scopeWarnings = engine.ScopeEditFrozenForActiveRuns(conn, id)
		for _, warning := range scopeWarnings {
			w.Warn("%s", warning)
		}
	}

	if len(updates) == 0 && !filesChanged && !scopeChanged {
		if w.JSONMode {
			issue, err := db.GetIssue(conn, id)
			if err != nil {
				return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
			}
			w.Success(withIssueVersion(issue), "")
		} else {
			w.Info("No changes specified")
		}
		return nil
	}

	ifVersion, err := ifVersionOf(cmd)
	if err != nil {
		return err
	}

	if len(updates) > 0 || ifVersion != nil {
		if err := db.UpdateIssueCAS(conn, id, updates, config.DefaultAuthor(), ifVersion); err != nil {
			if e := casError(err, fmt.Sprintf("issue %s", args[0])); e != nil {
				return e
			}
			return cmdErr(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
		}
	}

	updated, err := db.GetIssue(conn, id)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
	}

	if err := hydrateIssueAssociations(conn, updated); err != nil {
		return err
	}

	w.Success(
		withEditWarnings(withIssueVersion(updated), scopeWarnings),
		fmt.Sprintf("Updated %s: %s", model.FormatID(id), updated.Title))

	return nil
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
	editCmd.Flags().StringP("type", "T", "", "Issue type")
	editCmd.Flags().StringP("assignee", "a", "", "Issue assignee")
	editCmd.Flags().StringSliceP("file", "f", nil, "File paths (repeatable, replaces existing)")
	editCmd.Flags().String("parent", "", "Parent issue ID (use \"0\" or \"none\" to make root)")
	addScopeFlag(editCmd)
	addIfVersionFlag(editCmd)
	issueCmd.AddCommand(editCmd)
}
