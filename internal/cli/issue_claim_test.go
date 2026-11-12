package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/model"
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
	out := stdout()
	testsupport.Must(t, runErr, "issue %s %v: %v", cmd.Name(), args, runErr)

	var envelope struct {
		Data map[string]any `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &envelope), "payload is not JSON: %q", out)
	return envelope.Data
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

// TestIssueClaimRecordsTheProjectItRegisters keeps the project-registered
// event the root hook wrote before the claim resolved its own project.
func TestIssueClaimRecordsTheProjectItRegisters(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "lease subject", model.StatusTodo, model.PriorityNone)
	cfg := &config.Config{Identity: "/repos/fresh", Anchored: true}

	testsupport.Must(t, claimCmd.Flags().Set("owner", "worker-1"), "set --owner")
	runLeaseCommand(t, conn, cfg, claimCmd, model.FormatID(id))

	var data string
	err := conn.QueryRow(`SELECT data FROM events WHERE kind = 'project-registered'`).Scan(&data)
	testsupport.Must(t, err, "reading the project-registered event: %v", err)
	var event struct {
		Identity string `json:"identity"`
		Verb     string `json:"verb"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(data), &event), "event data is not JSON: %q", data)
	if event.Identity != "/repos/fresh" || event.Verb != "issue claim" {
		t.Errorf("event = %+v, want /repos/fresh registered by issue claim", event)
	}
}
