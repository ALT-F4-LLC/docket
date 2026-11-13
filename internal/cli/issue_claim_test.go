package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestIssueLeaseCommandsOwnTheirProject pins that the root hook leaves
// project resolution to internal/verb for the issue lease commands.
func TestIssueLeaseCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{claimCmd, releaseCmd, heartbeatCmd} {
		if _, ok := cmd.Annotations["verbOwnsProject"]; !ok {
			t.Errorf("issue %s lacks the verbOwnsProject annotation", cmd.Name())
		}
	}
}

// runLeaseCommand runs one issue lease command's RunE against conn under cfg
// with --json=v2 and returns its decoded data payload.
func runLeaseCommand(t *testing.T, conn *sql.DB, cfg *config.Config, cmd *cobra.Command, args ...string) map[string]any {
	t.Helper()
	out, runErr := runLeaseCommandOutput(t, conn, cfg, cmd, args...)
	testsupport.Must(t, runErr, "issue %s %v: %v", cmd.Name(), args, runErr)

	var envelope struct {
		Data map[string]any `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &envelope), "payload is not JSON: %q", out)
	return envelope.Data
}

// runLeaseCommandOutput is runLeaseCommand for a run that may fail: it
// returns the raw stdout and RunE's error.
func runLeaseCommandOutput(t *testing.T, conn *sql.DB, cfg *config.Config, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	testsupport.Must(t, rootCmd.PersistentFlags().Set("json", "v2"), "set --json")
	t.Cleanup(func() { _ = rootCmd.PersistentFlags().Set("json", "") })
	t.Cleanup(func() {
		cmd.Flags().Visit(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	})

	ctx := context.WithValue(context.Background(), dbKey, conn)
	if cfg != nil {
		ctx = context.WithValue(ctx, cfgKey, cfg)
	}
	cmd.SetContext(ctx)
	stdout := captureStdout(t)
	runErr := cmd.RunE(cmd, args)
	return stdout(), runErr
}

func TestIssueLeaseCommandsRenderTheLease(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "lease subject", model.StatusTodo, model.PriorityNone)
	ref := model.FormatID(id)

	testsupport.Must(t, claimCmd.Flags().Set("owner", "worker-1"), "set --owner")
	testsupport.Must(t, claimCmd.Flags().Set("ttl", "5m"), "set --ttl")
	claimed := runLeaseCommand(t, conn, nil, claimCmd, ref)
	token, _ := claimed["token"].(string)
	if claimed["issue"] != ref || len(token) != model.TokenBytes*2 ||
		claimed["attempt"] != 1.0 || claimed["version"] == nil || claimed["lease_expires_ms"] == nil {
		t.Fatalf("claim data = %v, want issue, token, lease_expires_ms, attempt 1 and version", claimed)
	}

	t.Setenv(TokenEnvVar, token)
	beat := runLeaseCommand(t, conn, nil, heartbeatCmd, ref)
	if beat["issue"] != ref || beat["attempt"] != 1.0 || beat["lease_expires_ms"] == nil || beat["token"] != nil {
		t.Errorf("heartbeat data = %v, want issue, lease_expires_ms and attempt 1, and no token", beat)
	}

	released := runLeaseCommand(t, conn, nil, releaseCmd, ref)
	if released["issue"] != ref || released["attempt"] != 1.0 {
		t.Errorf("release data = %v, want issue and attempt 1", released)
	}
}

// TestIssueReleaseAndHeartbeatReportAMalformedRefBeforeTheToken keeps the
// first error a malformed ref with no token reports: the ref, as before the
// commands called internal/verb, not the missing token.
func TestIssueReleaseAndHeartbeatReportAMalformedRefBeforeTheToken(t *testing.T) {
	conn := newTestDB(t)
	t.Setenv(TokenEnvVar, "")
	for _, cmd := range []*cobra.Command{releaseCmd, heartbeatCmd} {
		t.Run(cmd.Name(), func(t *testing.T) {
			withStdin(t, "")
			_, err := runLeaseCommandOutput(t, conn, nil, cmd, "not-an-id")
			var ce *CmdError
			if !errors.As(err, &ce) || ce.Code != output.ErrValidation ||
				!strings.HasPrefix(ce.Error(), "invalid issue ID") {
				t.Errorf("error = %v, want a VALIDATION_ERROR reporting the invalid issue ID", err)
			}
		})
	}
}

// registrationEvents maps each project-registered event's identity to the
// verb that registered it, failing on a second event for one identity.
func registrationEvents(t *testing.T, conn *sql.DB) map[string]string {
	t.Helper()
	rows, err := conn.Query(`SELECT data FROM events WHERE kind = 'project-registered'`)
	testsupport.Must(t, err, "reading the project-registered events: %v", err)
	defer rows.Close()
	verbs := map[string]string{}
	for rows.Next() {
		var data string
		testsupport.Must(t, rows.Scan(&data), "scanning an event")
		var event struct {
			Identity string `json:"identity"`
			Verb     string `json:"verb"`
		}
		testsupport.Must(t, json.Unmarshal([]byte(data), &event), "event data is not JSON: %q", data)
		if prior, dup := verbs[event.Identity]; dup {
			t.Errorf("%s registered twice, by %s and %s", event.Identity, prior, event.Verb)
		}
		verbs[event.Identity] = event.Verb
	}
	testsupport.Must(t, rows.Err(), "iterating the events")
	return verbs
}

// TestIssueLeaseCommandsRecordTheProjectsTheyRegister keeps the
// project-registered event the root hook wrote before each lease command
// resolved its own project, including for a command refused after it
// registered the project.
func TestIssueLeaseCommandsRecordTheProjectsTheyRegister(t *testing.T) {
	conn := newTestDB(t)
	ref := model.FormatID(createIssue(t, conn, "lease subject", model.StatusTodo, model.PriorityNone))
	anchored := func(identity string) *config.Config {
		return &config.Config{Identity: identity, Anchored: true}
	}

	testsupport.Must(t, claimCmd.Flags().Set("owner", "worker-1"), "set --owner")
	claimed := runLeaseCommand(t, conn, anchored("/repos/claim"), claimCmd, ref)
	token, _ := claimed["token"].(string)
	t.Setenv(TokenEnvVar, token)
	runLeaseCommand(t, conn, anchored("/repos/heartbeat"), heartbeatCmd, ref)
	runLeaseCommand(t, conn, anchored("/repos/release"), releaseCmd, ref)
	if _, err := runLeaseCommandOutput(t, conn, anchored("/repos/refused"), claimCmd, "DKT-999"); err == nil {
		t.Fatal("claim of a missing issue succeeded, want NOT_FOUND")
	}
	t.Setenv(TokenEnvVar, "deadbeef")
	if _, err := runLeaseCommandOutput(t, conn, anchored("/repos/refused-heartbeat"), heartbeatCmd, ref); err == nil {
		t.Fatal("heartbeat with a wrong token succeeded, want AUTH_ERROR")
	}
	if _, err := runLeaseCommandOutput(t, conn, anchored("/repos/refused-release"), releaseCmd, ref); err == nil {
		t.Fatal("release with a wrong token succeeded, want AUTH_ERROR")
	}

	want := map[string]string{
		"/repos/claim":             "issue claim",
		"/repos/heartbeat":         "issue heartbeat",
		"/repos/release":           "issue release",
		"/repos/refused":           "issue claim",
		"/repos/refused-heartbeat": "issue heartbeat",
		"/repos/refused-release":   "issue release",
	}
	got := registrationEvents(t, conn)
	if len(got) != len(want) {
		t.Errorf("registration events = %v, want %v", got, want)
	}
	for identity, verb := range want {
		if got[identity] != verb {
			t.Errorf("%s registered by %q, want %q", identity, got[identity], verb)
		}
	}
}
