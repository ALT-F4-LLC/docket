package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type docDeleteResult struct {
	ID string `json:"id"`
}

// newDocDeleteCmd builds the command fresh, per newImportCmd's pattern: tests
// drive an instance carrying the same flag registration a real invocation
// parses.
func newDocDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "delete <id>",
		Short:       "Delete a document",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"verbOwnsProject": ""},
		RunE: func(cmd *cobra.Command, args []string) error {
			w := getWriter(cmd)

			cascade, _ := cmd.Flags().GetBool("cascade")
			force, _ := cmd.Flags().GetBool("force")

			in := verb.DocDeleteInput{Doc: args[0], Cascade: cascade, Force: force}
			env := verbEnv(cmd)
			res, err := verb.DocDelete(cmd.Context(), env, in)
			recordRegisteredProject(cmd, env, res.Registered)

			// Without --force the verb deletes nothing and returns the doc
			// with its refusal. An output-format flag must never double as
			// consent for a destructive verb (DKT-15's rule; see
			// events_prune.go's `--yes` comment): JSON mode and
			// non-interactive human mode keep the refusal, and only a real
			// terminal may confirm.
			if err != nil && !force && res.Doc != nil && !w.JSONMode && term.IsTerminal(int(os.Stdin.Fd())) {
				var confirmed bool
				form := huh.NewForm(
					huh.NewGroup(
						huh.NewConfirm().
							Title(fmt.Sprintf("Delete %s: %s?", model.FormatDocID(res.Doc.ID), res.Doc.Title)).
							Value(&confirmed),
					),
				)
				if err := form.Run(); err != nil {
					if errors.Is(err, huh.ErrUserAborted) {
						w.Info("Cancelled.")
						return nil
					}
					return cmdErr(fmt.Errorf("interactive form failed: %w", err), output.ErrGeneral)
				}
				if !confirmed {
					w.Info("Cancelled.")
					return nil
				}
				in.Force = true
				res, err = verb.DocDelete(cmd.Context(), env, in)
				recordRegisteredProject(cmd, env, res.Registered)
			}
			if err != nil {
				return verbErr(err)
			}

			doc := res.Doc
			w.Success(docDeleteResult{ID: model.FormatDocID(doc.ID)}, fmt.Sprintf("Deleted %s: %s", model.FormatDocID(doc.ID), doc.Title))

			return nil
		},
	}
	cmd.Flags().Bool("cascade", false, "Also remove this document's issue/proposal links (the issues and proposals themselves are not deleted)")
	cmd.Flags().BoolP("force", "f", false, "Confirm the deletion (required in JSON and non-interactive mode; skips the terminal confirmation)")
	return cmd
}

var docDeleteCmd = newDocDeleteCmd()

func init() {
	docCmd.AddCommand(docDeleteCmd)
}
