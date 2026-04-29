package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ALT-F4-LLC/docket/internal/output"
)

func TestTUIHelpIncludesInteractiveDescription(t *testing.T) {
	var buf bytes.Buffer

	root := &cobra.Command{Use: "docket"}
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.AddCommand(newTUICommand())
	root.SetArgs([]string{"tui", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	output := buf.String()
	for _, fragment := range []string{"docket tui", "interactive terminal UI", "does not support --json"} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("help output missing %q: %q", fragment, output)
		}
	}
}

func TestTUIRejectsJSONFlag(t *testing.T) {
	cmd := newTUICommand()
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("setting json flag: %v", err)
	}

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}

	var cmdErr *CmdError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T, want *CmdError", err)
	}

	var stdout bytes.Buffer
	w := &output.Writer{JSONMode: true, Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := w.Error(cmdErr.Err, cmdErr.Code); code != output.ExitValidation {
		t.Fatalf("exit code = %d, want %d", code, output.ExitValidation)
	}

	var env struct {
		OK    bool             `json:"ok"`
		Error string           `json:"error"`
		Code  output.ErrorCode `json:"code"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK {
		t.Fatal("ok = true, want false")
	}
	if env.Code != output.ErrValidation {
		t.Fatalf("code = %q, want %q", env.Code, output.ErrValidation)
	}
	if env.Error != "--json is not supported with 'docket tui'" {
		t.Fatalf("error = %q", env.Error)
	}
}

func TestTUIRequiresInteractiveTerminal(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) || term.IsTerminal(int(os.Stdout.Fd())) {
		t.Skip("test requires non-interactive stdio")
	}

	cmd := newTUICommand()
	cmd.Flags().Bool("json", false, "")

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}

	var cmdErr *CmdError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T, want *CmdError", err)
	}
	if cmdErr.Code != output.ErrValidation {
		t.Fatalf("code = %q, want %q", cmdErr.Code, output.ErrValidation)
	}
	if cmdErr.Error() != "'docket tui' requires an interactive terminal" {
		t.Fatalf("error = %q", cmdErr.Error())
	}
}

func TestTUIDebugLogOpenFailureReturnsGeneralError(t *testing.T) {
	cmd := newTUICommand()
	t.Setenv("DOCKET_TUI_DEBUG_LOG", "/definitely/missing/docket-tui.log")

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected general error")
	}

	var cmdErr *CmdError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T, want *CmdError", err)
	}
	if cmdErr.Code != output.ErrGeneral {
		t.Fatalf("code = %q, want %q", cmdErr.Code, output.ErrGeneral)
	}
	if !strings.Contains(cmdErr.Error(), "opening debug log") {
		t.Fatalf("error = %q", cmdErr.Error())
	}
}

func TestTUILegacyUIDebugEnvIsIgnored(t *testing.T) {
	cmd := newTUICommand()
	t.Setenv("DOCKET_UI_DEBUG_LOG", "/definitely/missing/docket-ui.log")

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}

	var cmdErr *CmdError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T, want *CmdError", err)
	}
	if cmdErr.Code != output.ErrValidation {
		t.Fatalf("code = %q, want %q", cmdErr.Code, output.ErrValidation)
	}
	if cmdErr.Error() != "'docket tui' requires an interactive terminal" {
		t.Fatalf("error = %q", cmdErr.Error())
	}
}

func TestTUIRunsProgramWhenInteractive(t *testing.T) {
	restoreTerminal := hasInteractiveTerminal
	restoreRun := runTUIProgram
	t.Cleanup(func() {
		hasInteractiveTerminal = restoreTerminal
		runTUIProgram = restoreRun
	})

	hasInteractiveTerminal = func() bool { return true }
	runCalled := false
	runTUIProgram = func(cmd *cobra.Command) error {
		runCalled = true
		return nil
	}

	cmd := newTUICommand()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !runCalled {
		t.Fatal("expected runTUIProgram to be called")
	}
}

func TestTUIWrapsProgramError(t *testing.T) {
	restoreTerminal := hasInteractiveTerminal
	restoreRun := runTUIProgram
	t.Cleanup(func() {
		hasInteractiveTerminal = restoreTerminal
		runTUIProgram = restoreRun
	})

	hasInteractiveTerminal = func() bool { return true }
	runTUIProgram = func(cmd *cobra.Command) error {
		return io.EOF
	}

	cmd := newTUICommand()
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected general error")
	}

	var cmdErr *CmdError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T, want *CmdError", err)
	}
	if cmdErr.Code != output.ErrGeneral {
		t.Fatalf("code = %q, want %q", cmdErr.Code, output.ErrGeneral)
	}
	if cmdErr.Error() != "running docket tui: EOF" {
		t.Fatalf("error = %q", cmdErr.Error())
	}
}
