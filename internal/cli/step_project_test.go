package cli

import (
	"context"
	"database/sql"
	"maps"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// runAddressedStepCommands are the step commands that take their project from
// the step they are handed. Each runs against step and returns the command's
// error; the step's own outcome is not what these tests pin.
var runAddressedStepCommands = []struct {
	verb string
	run  func(cmd *cobra.Command, step string) error
}{
	{"step integrate", func(cmd *cobra.Command, step string) error {
		w, _ := bufWriter(true)
		return runStepIntegrate(cmd, []string{step}, w)
	}},
	{"step pregate", func(cmd *cobra.Command, step string) error {
		cmd.Flags().String("target", strings.Repeat("c", 40), "")
		return stepPreGateCmd.RunE(cmd, []string{step})
	}},
	{"step resume", func(cmd *cobra.Command, step string) error {
		return stepResumeCmd.RunE(cmd, []string{step})
	}},
}

// stepOfAnActivatedRun returns a store holding an activated run and one of
// its never-claimed steps.
func stepOfAnActivatedRun(t *testing.T) (*sql.DB, string) {
	t.Helper()
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	return conn, model.FormatStepID(pendingStepID(t, conn, runID))
}

// TestStepCommandsRecordTheProjectsTheyRegister keeps the project-registered
// event the root hook wrote before step integrate, pregate and resume resolved
// their own project: an anchored checkout is registered on first contact, once,
// whatever the command then does with the step.
func TestStepCommandsRecordTheProjectsTheyRegister(t *testing.T) {
	for _, c := range runAddressedStepCommands {
		t.Run(c.verb, func(t *testing.T) {
			conn, step := stepOfAnActivatedRun(t)
			identity := "/repos/" + strings.ReplaceAll(c.verb, " ", "-")

			_ = c.run(withAnchoredIdentity(cmdWithDB(conn), identity), step)

			want := map[string]string{identity: c.verb}
			if got := registrationEvents(t, conn); !maps.Equal(got, want) {
				t.Errorf("registration events = %v, want %v", got, want)
			}
		})
	}
}

// An unanchored caller is not refused: each command takes its project from
// the step it is handed, so it never asks which project the cwd belongs to.
func TestStepCommandsAcceptAnUnanchoredCaller(t *testing.T) {
	for _, c := range runAddressedStepCommands {
		t.Run(c.verb, func(t *testing.T) {
			conn, step := stepOfAnActivatedRun(t)
			cmd := cmdWithDB(conn)
			cmd.SetContext(context.WithValue(cmd.Context(), cfgKey,
				&config.Config{Identity: "/work/scratch", Anchored: false}))

			err := c.run(cmd, step)
			if err != nil && strings.Contains(err.Error(), "cannot tell which project this belongs to") {
				t.Errorf("%s refused an unanchored caller: %v", c.verb, err)
			}
			if got := registrationEvents(t, conn); len(got) != 0 {
				t.Errorf("registration events = %v, want none", got)
			}
		})
	}
}
