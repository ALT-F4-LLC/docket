package cli

import (
	"database/sql"
	"fmt"
	"io"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var closeCmd = &cobra.Command{
	Use:   "close [id]",
	Short: "Close an issue (shorthand for move <id> done)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := issueArg(args[0])
		if err != nil {
			return err
		}

		issue, err := getIssueOrErr(conn, id, fmt.Sprintf("issue %s", args[0]))
		if err != nil {
			return err
		}

		if issue.Status == model.StatusDone {
			if w.JSONMode {
				w.Success(withIssueVersion(issue), "")
			} else {
				w.Info("Issue %s is already closed", model.FormatID(id))
			}
			return nil
		}

		ifVersion, err := ifVersionOf(cmd)
		if err != nil {
			return err
		}

		// Closing ends any live lease, so it is lease-mediated: a holder may
		// always close, and a non-holder is refused rather than silently
		// evicting a working holder. The token is OPTIONAL — an unclaimed
		// issue closes exactly as it did before leases existed, which is what
		// keeps this verb byte-compatible for a repo that never claims.
		token := tokenForClose(conn, id, os.Stdin)

		err = db.UpdateIssueCASLease(conn, id, map[string]interface{}{"status": "done"}, config.DefaultAuthor(), ifVersion, token)
		if err != nil {
			label := fmt.Sprintf("issue %s", model.FormatID(id))
			if e := leaseError(err, label); e != nil {
				return e
			}
			if e := casError(err, label); e != nil {
				return e
			}
			return cmdErr(fmt.Errorf("closing issue: %w", err), output.ErrGeneral)
		}

		issue, err = db.GetIssue(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
		}

		w.Success(withIssueVersion(issue), fmt.Sprintf("Closed %s: %s", model.FormatID(id), issue.Title))
		return nil
	},
}

// tokenForClose returns the token to present when closing id. Nothing is
// read unless the issue holds a LIVE lease: the stdin token fallback drains
// its reader to EOF, and an agent whose inherited stdin pipe never closes
// would hang forever on the unclaimed-issue path — which needs no token at
// all. The look here is advisory: the authoritative lease check runs inside
// the update transaction, where a lease claimed after this read still
// refuses the empty token instead of being evicted.
func tokenForClose(conn *sql.DB, id int, stdin io.Reader) string {
	lease, err := db.GetIssueLease(conn, id)
	if err != nil || !lease.Live(model.NowMS()) {
		return ""
	}
	return optionalToken(stdin)
}

func init() {
	addIfVersionFlag(closeCmd)
	issueCmd.AddCommand(closeCmd)
}
