package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `docket workflow lint` — DKT-38: the register-path validation, reachable
// without a persistent write.

// lintSource writes src to a temp file and lints it, returning the JSON the verb
// printed and the error.
func lintSource(t *testing.T, conn store.LegacyDB, src string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.toml")
	err := os.WriteFile(path, []byte(src), 0o644)
	testsupport.Must(t, err, "writing the definition: %v", err)
	cmd := cmdWithDB(conn)
	w, buf := bufWriter(true)
	err = runWorkflowLint(cmd, []string{path}, w)
	return buf.String(), err
}

func countWorkflows(t *testing.T, conn store.LegacyDB) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM workflows`).Scan(&n)
	testsupport.Must(t, err, "counting workflows: %v", err)
	return n
}

// TestWorkflowLintValidatesWithoutWriting is the point of the verb: the same
// validation register runs, and the registry gains no row.
func TestWorkflowLintValidatesWithoutWriting(t *testing.T) {
	conn := newTestDB(t)

	out, err := lintSource(t, conn, minimalWorkflow)
	testsupport.Must(t, err, "lint: %v", err)
	if countWorkflows(t, conn) != 0 {
		t.Fatal("lint registered the definition; it must write nothing")
	}
	if !strings.Contains(out, `"registration":"new"`) {
		t.Errorf("output %q does not report the slot as new", out)
	}
}

// TestWorkflowLintRefusesWhatRegisterRefuses: an invalid definition fails lint
// with the register path's own error, so the two surfaces cannot disagree.
func TestWorkflowLintRefusesWhatRegisterRefuses(t *testing.T) {
	conn := newTestDB(t)

	// A step with no `after` (and not first) is the classic validation miss.
	invalid := strings.Replace(minimalWorkflow, "after = [\"first\"]\n", "", 1)
	_, err := lintSource(t, conn, invalid)
	if err == nil {
		t.Fatal("lint accepted a definition register would refuse")
	}
	if codeOf(t, err) != output.ErrValidation {
		t.Errorf("code = %v, want VALIDATION_ERROR", codeOf(t, err))
	}
	if countWorkflows(t, conn) != 0 {
		t.Error("a refused lint left a row behind")
	}
}

// TestWorkflowLintReportsRegistrationOutcome: unchanged bytes at a registered
// name@version lint clean; DIFFERENT bytes at that frozen slot are the
// retro-loop's observed trap and fail with CONFLICT naming both hashes and
// the version to bump to.
func TestWorkflowLintReportsRegistrationOutcome(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, registerSource(t, conn, minimalWorkflow), "register")

	out, err := lintSource(t, conn, minimalWorkflow)
	testsupport.Must(t, err, "lint of unchanged bytes: %v", err)
	if !strings.Contains(out, `"registration":"unchanged"`) {
		t.Errorf("output %q does not report unchanged", out)
	}

	_, err = lintSource(t, conn, minimalWorkflow+"\n# edited\n")
	if err == nil {
		t.Fatal("lint passed an edit at a frozen name@version; activation " +
			"would refuse the whole run over exactly this")
	}
	if codeOf(t, err) != output.ErrConflict {
		t.Errorf("code = %v, want CONFLICT", codeOf(t, err))
	}
	if !strings.Contains(err.Error(), "bump [pipeline].version to 2") {
		t.Errorf("refusal does not name the fix: %v", err)
	}
}

func TestWorkflowLintOwnsItsProject(t *testing.T) {
	if _, ok := workflowLintCmd.Annotations["verbOwnsProject"]; !ok {
		t.Error("workflow lint lacks the verbOwnsProject annotation")
	}
}

// The verb, not the root hook, decides which registry judges the file: the
// command carries only the config's identity, and the verdict is that
// project's.
func TestWorkflowLintJudgesTheConfiguredProject(t *testing.T) {
	conn := newTestDB(t)
	cmd := lintCmd(conn, "")
	inCallerProject(t, conn, cmd, "/repo/caller")
	w, _ := bufWriter(true)
	testsupport.Must(t, runWorkflowRegister(fanoutCmd(conn, "caller", false),
		[]string{writeWorkflowFile(t, minimalWorkflow)}, w), "registering unit@1 in the caller's project")

	out, err := lintWith(t, cmd, minimalWorkflow)
	testsupport.Must(t, err, "lint in the configured project: %v", err)
	if !strings.Contains(out, `"registration":"unchanged"`) {
		t.Errorf("the verdict is not the configured project's:\n%s", out)
	}
}
