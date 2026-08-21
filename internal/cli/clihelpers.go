package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/watch"
	"github.com/spf13/cobra"
)

// watchable runs a read verb once, or repeatedly under `--watch`.
//
// This is the single copy of a block that used to be pasted, byte-for-byte,
// into every watch-eligible RunE (board.go and config.go were the identical
// pair that made the duplication visible; fourteen more commands carried the
// same 16 lines). A caller's RunE becomes one line:
//
//	RunE: func(cmd *cobra.Command, args []string) error {
//		return watchable(cmd, args, runBoard)
//	},
//
// `run` is the verb's normal one-shot body, called either directly (with
// getWriter's Writer) or once per --watch tick (with the tick's Writer).
func watchable(cmd *cobra.Command, args []string, run func(*cobra.Command, []string, *output.Writer) error) error {
	watchMode, _ := cmd.Flags().GetBool("watch")
	if watchMode {
		interval, _ := cmd.Flags().GetDuration("interval")
		jsonMode, jsonVersion := jsonModeOf(cmd)
		quietMode, _ := cmd.Flags().GetBool("quiet")
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return watch.RunWatch(ctx, watch.Options{
			Interval:    interval,
			JSONMode:    jsonMode,
			JSONVersion: jsonVersion,
			QuietMode:   quietMode,
			IsTTY:       term.IsTerminal(int(os.Stdout.Fd())),
			Stdout:      os.Stdout,
			Stderr:      os.Stderr,
		}, func(ctx context.Context, w *output.Writer) error {
			return run(cmd, args, w)
		})
	}
	return run(cmd, args, getWriter(cmd))
}

// issueArg parses an issue ID out of a command-line source (an args[]
// element or a flag value) and wraps a parse failure in the VALIDATION_ERROR
// every call site returns today.
//
// The source is passed as a string rather than an index or flag name because
// call sites disagree on where the ID comes from — args[0], args[2], or a
// --issue flag — and a single parameter that already holds the raw text
// covers all three without the helper needing to know which.
func issueArg(source string) (int, error) {
	id, err := model.ParseID(source)
	if err != nil {
		return 0, cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
	}
	return id, nil
}

// notFound re-wraps db.ErrNotFound as the NOT_FOUND error every call site
// reports with its own label, and returns nil for any other error so the
// caller's existing fallback (typically a generic ErrGeneral wrap) still
// applies unchanged.
//
// It mirrors leaseError and casError's shape (token.go, ifversion.go),
// including their (err, label) parameter order: a dispatcher that returns a
// *CmdError on the conditions it recognizes and nil otherwise, so a call
// site reads as
//
//	if e := notFound(err, fmt.Sprintf("issue %s", args[0])); e != nil {
//		return e
//	}
//	return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
func notFound(err error, label string) error {
	if errors.Is(err, db.ErrNotFound) {
		return cmdErr(fmt.Errorf("%s not found", label), output.ErrNotFound)
	}
	return nil
}

// getIssueOrErr fetches an issue and wraps a failure exactly as the ~14 call
// sites that used to paste this fetch-and-wrap block did: a NOT_FOUND error
// carrying label when the issue does not exist, and a generic ErrGeneral wrap
// for anything else. label matches whatever text each call site already
// passed to notFound, so error text is unchanged at every site.
func getIssueOrErr(conn *sql.DB, id int, label string) (*model.Issue, error) {
	issue, err := db.GetIssue(conn, id)
	if err != nil {
		if e := notFound(err, label); e != nil {
			return nil, e
		}
		return nil, cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
	}
	return issue, nil
}

// projectLabel renders a project for a refusal message: the row's name when it
// has one, always with the numeric id — names are display-only, so the id is
// what an operator can act on (`docket project list` speaks both).
func projectLabel(conn *sql.DB, id int) string {
	if p, err := db.GetProject(conn, id); err == nil && p.Name != "" {
		return fmt.Sprintf("project %q (id %d)", p.Name, p.ID)
	}
	return fmt.Sprintf("project %d", id)
}

// resolveParentIssue resolves a prospective parent reference and refuses one
// homed in a different project (DKT-22).
//
// Issue ids are store-wide, so any project's issue PARSES as a parent — but a
// subtree spanning projects breaks everything built on subtree homogeneity:
// `issue move --project` migrates whole subtrees and refuses to move a
// sub-issue alone, and per-project tree rendering would show a parent whose
// child no listing in either project can reach. The supported way to re-home
// a subtree is `issue move --project` on its root.
//
// childProjectID is the project the CHILD lives in (or, on create, will be
// created in) — the comparison is issue-to-issue, never against the invoking
// cwd, so editing another project's issue by id still reparents freely within
// that issue's own project.
func resolveParentIssue(conn *sql.DB, ref string, childProjectID int) (*model.Issue, error) {
	pid, err := model.ParseID(ref)
	if err != nil {
		return nil, cmdErr(fmt.Errorf("invalid parent ID: %w", err), output.ErrValidation)
	}
	parent, err := getIssueOrErr(conn, pid, fmt.Sprintf("parent issue %s", ref))
	if err != nil {
		return nil, err
	}
	if parent.ProjectID != childProjectID {
		return nil, cmdErr(fmt.Errorf(
			"parent issue %s belongs to %s, this issue to %s — a sub-issue lives in "+
				"its parent's project; re-home the subtree with `docket issue move "+
				"--project` on its root instead",
			model.FormatID(pid), projectLabel(conn, parent.ProjectID),
			projectLabel(conn, childProjectID)), output.ErrValidation)
	}
	return parent, nil
}

// parseRunFlag parses a --run reference already read from a flag, wrapping a
// malformed reference in the VALIDATION_ERROR every --run parser reports.
// It holds the one piece of logic dispatchRunID, optionalRunFlag, and
// requiredRunFlag shared byte-for-byte; each keeps its own handling of an
// EMPTY ref (required vs. optional vs. falling through to ParseRunID's own
// error), which is what actually differs between the three.
func parseRunFlag(ref string) (int, error) {
	runID, err := model.ParseRunID(ref)
	if err != nil {
		return 0, cmdErr(fmt.Errorf("invalid run ID: %w", err), output.ErrValidation)
	}
	return runID, nil
}

// hydrateIssueAssociations fills an issue's labels, files, and docs from their
// join tables. db.GetIssue reads the issues row alone, so a struct straight
// out of it renders "labels":[] no matter what is linked — which reads as a
// silent drop when the caller just supplied -l (DKT-240). Every command that
// renders a refetched issue as its result payload runs this first, so what is
// echoed back is what is stored.
func hydrateIssueAssociations(conn *sql.DB, issue *model.Issue) error {
	labels, err := db.GetIssueLabels(conn, issue.ID)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching labels: %w", err), output.ErrGeneral)
	}
	issue.Labels = labels

	files, err := db.GetIssueFiles(conn, issue.ID)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching files: %w", err), output.ErrGeneral)
	}
	issue.Files = files

	if err := db.HydrateDocs(conn, []*model.Issue{issue}); err != nil {
		return cmdErr(fmt.Errorf("fetching docs: %w", err), output.ErrGeneral)
	}
	return nil
}
