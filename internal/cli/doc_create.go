package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const maxDocBodySize = 1 << 20

func loadDocBody(body string) (string, error) {
	switch {
	case strings.HasPrefix(body, "@"):
		path := body[1:]
		if path == "" {
			return "", cmdErr(fmt.Errorf("empty file path after @"), output.ErrValidation)
		}
		f, err := os.Open(path)
		if err != nil {
			return "", cmdErr(fmt.Errorf("reading body from %q: %w", path, err), output.ErrValidation)
		}
		defer f.Close()
		lr := &io.LimitedReader{R: f, N: maxDocBodySize + 1}
		data, err := io.ReadAll(lr)
		if err != nil {
			return "", cmdErr(fmt.Errorf("reading body from %q: %w", path, err), output.ErrValidation)
		}
		if int64(len(data)) > maxDocBodySize {
			return "", cmdErr(fmt.Errorf("body from %q exceeds %d bytes", path, maxDocBodySize), output.ErrValidation)
		}
		return strings.TrimRight(string(data), "\n"), nil
	case body == "-":
		lr := &io.LimitedReader{R: os.Stdin, N: maxDocBodySize + 1}
		data, err := io.ReadAll(lr)
		if err != nil {
			return "", cmdErr(fmt.Errorf("reading body from stdin: %w", err), output.ErrGeneral)
		}
		if int64(len(data)) > maxDocBodySize {
			return "", cmdErr(fmt.Errorf("body from stdin exceeds %d bytes", maxDocBodySize), output.ErrValidation)
		}
		return strings.TrimRight(string(data), "\n"), nil
	default:
		return body, nil
	}
}

var docCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new document",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		title, _ := cmd.Flags().GetString("title")
		body, _ := cmd.Flags().GetString("description")
		docType, _ := cmd.Flags().GetString("type")
		status, _ := cmd.Flags().GetString("status")
		jsonMode, _ := cmd.Flags().GetBool("json")

		if jsonMode && title == "" {
			return cmdErr(fmt.Errorf("--title is required in JSON mode"), output.ErrValidation)
		}

		if !jsonMode && title == "" {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return cmdErr(fmt.Errorf("non-interactive environment detected; provide all required flags: --title"), output.ErrValidation)
			}
			form := huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title("Title").
						Value(&title).
						Validate(func(s string) error {
							if strings.TrimSpace(s) == "" {
								return fmt.Errorf("title is required")
							}
							return nil
						}),
					huh.NewText().
						Title("Body").
						Value(&body),
					huh.NewInput().
						Title("Type").
						Value(&docType),
					huh.NewInput().
						Title("Status").
						Value(&status),
				),
			)

			if err := form.Run(); err != nil {
				if errors.Is(err, huh.ErrUserAborted) {
					w.Info("Cancelled.")
					return nil
				}
				return cmdErr(fmt.Errorf("interactive form failed: %w", err), output.ErrGeneral)
			}
		}

		loadedBody, err := loadDocBody(body)
		if err != nil {
			return err
		}
		body = loadedBody

		doc := model.Doc{
			Type:   docType,
			Status: status,
			Title:  title,
			Body:   body,
			Author: config.DefaultAuthor(),
		}

		id, err := db.CreateDoc(conn, &doc)
		if err != nil {
			return cmdErr(fmt.Errorf("creating doc: %w", err), output.ErrGeneral)
		}

		created, err := db.GetDoc(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching created doc: %w", err), output.ErrGeneral)
		}

		w.Success(created, fmt.Sprintf("Created %s: %s", model.FormatDocID(id), created.Title))

		return nil
	},
}

func init() {
	docCreateCmd.Flags().StringP("title", "t", "", "Document title")
	docCreateCmd.Flags().StringP("description", "d", "", "Document body (use \"@path\" for a file or \"-\" for stdin)")
	docCreateCmd.Flags().StringP("type", "T", "", "Document type")
	docCreateCmd.Flags().StringP("status", "s", "", "Document status")
	docCmd.AddCommand(docCreateCmd)
}
