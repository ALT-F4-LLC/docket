package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"golang.org/x/term"
)

// commentBodyOrEditor settles a comment body that the flag and stdin left
// empty. It returns a non-empty body unchanged.
//
// A blank body is never an invitation to open an editor unless a person can
// type into one. `-m ""` was a malformed substitution in a script, and the
// editor it launched had no terminal: it hung for two minutes and left its log
// in the checkout. So:
//   - a message flag that was given but blank is a validation error;
//   - JSON mode never opens an editor;
//   - the editor opens only when both stdin and stdout are terminals.
//
// An editor that saves nothing still returns "", which the caller reports as a
// cancellation.
func commentBodyOrEditor(body string, messageSet, jsonMode bool) (string, error) {
	if strings.TrimSpace(body) != "" {
		return body, nil
	}
	if messageSet {
		return "", cmdErr(fmt.Errorf("message is empty: -m needs the comment text"), output.ErrValidation)
	}
	if jsonMode {
		return "", cmdErr(fmt.Errorf("message is required in JSON mode"), output.ErrValidation)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return "", cmdErr(fmt.Errorf(
			"message is empty: the editor opens only on a terminal; "+
				"pass -m or pipe the comment on stdin"), output.ErrValidation)
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	tmpFile, err := os.CreateTemp("", "docket-comment-*.md")
	if err != nil {
		return "", cmdErr(fmt.Errorf("creating temp file: %w", err), output.ErrGeneral)
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return "", cmdErr(fmt.Errorf("closing temp file: %w", err), output.ErrGeneral)
	}
	defer os.Remove(tmpPath)

	editorCmd := exec.Command(editor, tmpPath)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return "", cmdErr(fmt.Errorf("editor exited with error: %w", err), output.ErrGeneral)
	}
	content, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", cmdErr(fmt.Errorf("reading temp file: %w", err), output.ErrGeneral)
	}
	return strings.TrimSpace(string(content)), nil
}
