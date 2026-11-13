package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// guardCfg is an anchored project identity no project row holds yet.
func guardCfg(t *testing.T) *config.Config {
	return &config.Config{Identity: filepath.Join(t.TempDir(), "repo"), Anchored: true}
}

func requireCmdErrCode(t *testing.T, err error, code output.ErrorCode, want string) {
	t.Helper()
	var ce *CmdError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *CmdError", err, err)
	}
	if ce.Code != code {
		t.Errorf("code = %s, want %s (err: %v)", ce.Code, code, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to contain %q", err, want)
	}
}

func TestGuardCommandsResolveTheirOwnProject(t *testing.T) {
	for _, cmd := range guardCmd.Commands() {
		if !verbOwnsProject(cmd) {
			t.Errorf("guard %s is not annotated verbOwnsProject", cmd.Name())
		}
	}
}

func TestGuardAllowRendersAllowedTrue(t *testing.T) {
	conn := newTestDB(t)
	out, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardStopCmd)
	testsupport.Must(t, err, "guard stop: %v", err)
	if !strings.Contains(out, `"data":{"allowed":true}`) {
		t.Errorf("guard stop output = %s, want data {\"allowed\":true}", out)
	}
}

// A denial rides the error channel with NOT_FOUND's code, which exits 2.
func TestGuardDenialIsANotFoundError(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, guardGateCmd.Flags().Set("step", "approve"), "set --step")
	_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardGateCmd)
	requireCmdErrCode(t, err, output.ErrNotFound, "approve")
}

// --active refuses --rows before reading it, so a missing file is a
// validation error rather than NOT_FOUND, and stdin is never read.
func TestGuardSpawnActiveRefusesRowsWithoutReadingThem(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, guardSpawnCmd.Flags().Set("active", "true"), "set --active")
	testsupport.Must(t, guardSpawnCmd.Flags().Set("rows", filepath.Join(t.TempDir(), "absent.json")), "set --rows")
	_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardSpawnCmd)
	requireCmdErrCode(t, err, output.ErrValidation, "--active does not take --rows")
}

func TestGuardSpawnReadsRowsBeforeAskingTheRun(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, guardSpawnCmd.Flags().Set("run", "RUN-999"), "set --run")
	testsupport.Must(t, guardSpawnCmd.Flags().Set("rows", filepath.Join(t.TempDir(), "absent.json")), "set --rows")
	_, err := runLeaseCommandOutput(t, conn, guardCfg(t), guardSpawnCmd)
	requireCmdErrCode(t, err, output.ErrNotFound, "reading proposed rows")
}
