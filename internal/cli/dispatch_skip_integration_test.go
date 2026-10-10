package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `dispatch close --skip-integration-check` records an override every later
// close of the run honors, so on a bound run it requires the run's conductor
// capability. A plain close, and any close on an unbound run, needs none.

const skipReason = "hand-integrated, verified by the operator"

// skipCloseWave is a bound run with an open dispatch whose one step finished
// without reporting usage. The run's conductor token is held in DOCKET_TOKEN;
// it returns the run, the step, and that token.
func skipCloseWave(t *testing.T, conn *sql.DB) (runID, stepID int, token string) {
	t.Helper()
	runID, _ = seedRun(t, conn)
	activateHolding(t, conn, runID, model.NowMS())
	token = envToken(t)
	stepID = waveWithUnreportedUsage(t, conn, runID)
	return runID, stepID, token
}

// backfillStandalone settles the step's usage through `backfill-usage`, so a
// close without --backfill-from has no discrepancy to refuse on.
func backfillStandalone(t *testing.T, conn *sql.DB, runID, stepID int) {
	t.Helper()
	_, err := engine.NewEngine().BackfillUsage(conn, runID, []engine.BackfillRow{
		{Step: stepID, Unit: "tokens", Quantity: 7},
	}, "", "", model.NowMS())
	testsupport.Must(t, err, "backfill-usage: %v", err)
}

// closeEffects is everything a close, a skip, or a back-fill writes for the
// run: the dispatch row, the close events, any event carrying the skip's
// reason, the usage ledger, and the step's usage flag.
type closeEffects struct {
	dispatchStatus string
	closedEvents   int
	reasonEvents   int
	usageRows      int
	usageRecorded  int
}

func readCloseEffects(t *testing.T, conn *sql.DB, runID, stepID int) closeEffects {
	t.Helper()
	var got closeEffects
	err := conn.QueryRow(`SELECT status FROM dispatches WHERE run_id = ? ORDER BY id DESC LIMIT 1`,
		runID).Scan(&got.dispatchStatus)
	testsupport.Must(t, err, "reading the dispatch row: %v", err)
	err = conn.QueryRow(`SELECT COUNT(*) FROM events WHERE run_id = ? AND kind = ?`,
		runID, engine.EventDispatchClosed).Scan(&got.closedEvents)
	testsupport.Must(t, err, "counting close events: %v", err)
	err = conn.QueryRow(`SELECT COUNT(*) FROM events WHERE run_id = ? AND data LIKE ?`,
		runID, "%"+skipReason+"%").Scan(&got.reasonEvents)
	testsupport.Must(t, err, "counting skip records: %v", err)
	err = conn.QueryRow(`SELECT COUNT(*) FROM usage_ledger WHERE run_id = ?`,
		runID).Scan(&got.usageRows)
	testsupport.Must(t, err, "counting usage rows: %v", err)
	err = conn.QueryRow(`SELECT usage_recorded FROM steps WHERE id = ?`,
		stepID).Scan(&got.usageRecorded)
	testsupport.Must(t, err, "reading usage_recorded: %v", err)
	return got
}

// skipClose runs `dispatch close --skip-integration-check skipReason` with
// stdin as given, and `--backfill-from` when from is non-empty.
func skipClose(conn *sql.DB, runID int, from string, stdin string) (string, error) {
	cmd := dispatchCloseCmdWithDB(conn, model.FormatRunID(runID))
	if err := cmd.Flags().Set("skip-integration-check", skipReason); err != nil {
		return "", err
	}
	if from != "" {
		if err := cmd.Flags().Set("backfill-from", from); err != nil {
			return "", err
		}
	}
	cmd.SetIn(strings.NewReader(stdin))
	w, buf := bufWriter(true)
	err := runDispatchClose(cmd, w)
	return buf.String(), err
}

// assertSkipRecorded reads the close's integration verdict out of the JSON
// payload at path (a dotted list of keys under `data`).
func assertSkipRecorded(t *testing.T, payload string, path ...string) {
	t.Helper()
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(payload), &env), "decoding: %v\n%s", nil, payload)
	data := env.Data
	for _, key := range path {
		var next map[string]json.RawMessage
		testsupport.Must(t, json.Unmarshal(data[key], &next), "decoding %s: %v\n%s", key, nil, payload)
		data = next
	}
	var integration engine.IntegrationCheck
	testsupport.Must(t, json.Unmarshal(data["integration"], &integration),
		"decoding integration: %v\n%s", nil, payload)
	if integration.Status != "skipped" || integration.Reason != skipReason {
		t.Errorf("integration = %+v, want skipped with reason %q\n%s", integration, skipReason, payload)
	}
}

// TestDispatchCloseSkipIntegrationRequiresTheConductor: on a bound run the
// skip refuses a missing token VALIDATION_ERROR naming DOCKET_TOKEN and a
// wrong one AUTH_ERROR, writing no close and no skip record; the run's own
// token closes and records the reason.
func TestDispatchCloseSkipIntegrationRequiresTheConductor(t *testing.T) {
	conn := newTestDB(t)
	runID, stepID, token := skipCloseWave(t, conn)
	backfillStandalone(t, conn, runID, stepID)
	before := readCloseEffects(t, conn, runID, stepID)

	refusals := []struct {
		name, token string
		want        output.ErrorCode
	}{
		{"no token", "", output.ErrValidation},
		{"a wrong token", "deadbeef", output.ErrAuth},
	}
	for _, tc := range refusals {
		t.Setenv(TokenEnvVar, tc.token)
		_, err := skipClose(conn, runID, "", "")
		assertCmdCode(t, err, tc.want, "dispatch close --skip-integration-check with "+tc.name)
		if tc.want == output.ErrValidation && !strings.Contains(err.Error(), TokenEnvVar) {
			t.Errorf("%s: the refusal %q does not name %s", tc.name, err, TokenEnvVar)
		}
		if after := readCloseEffects(t, conn, runID, stepID); after != before {
			t.Errorf("%s: the refused skip wrote %+v -> %+v", tc.name, before, after)
		}
	}

	t.Setenv(TokenEnvVar, token)
	payload, err := skipClose(conn, runID, "", "")
	testsupport.Must(t, err, "dispatch close --skip-integration-check with the run's token: %v", err)
	assertSkipRecorded(t, payload)
	after := readCloseEffects(t, conn, runID, stepID)
	if after.dispatchStatus == "open" || after.closedEvents != before.closedEvents+1 ||
		after.reasonEvents != before.reasonEvents+1 {
		t.Errorf("effects %+v -> %+v, want the dispatch closed with one event carrying the reason",
			before, after)
	}
}

// TestDispatchCloseSkipIntegrationWithBackfillRefusesBeforeTheBackfill: the
// same refusals with --backfill-from write no usage rows and no close, and the
// run's token runs all three stages.
func TestDispatchCloseSkipIntegrationWithBackfillRefusesBeforeTheBackfill(t *testing.T) {
	conn := newTestDB(t)
	runID, stepID, token := skipCloseWave(t, conn)
	path := usageJSON(t, fmt.Sprintf(`[{"step":%q,"unit":"tokens","quantity":48211}]`,
		model.FormatStepID(stepID)))
	before := readCloseEffects(t, conn, runID, stepID)

	refusals := []struct {
		name, token string
		want        output.ErrorCode
	}{
		{"no token", "", output.ErrValidation},
		{"a wrong token", "deadbeef", output.ErrAuth},
	}
	for _, tc := range refusals {
		t.Setenv(TokenEnvVar, tc.token)
		_, err := skipClose(conn, runID, path, "")
		assertCmdCode(t, err, tc.want, "dispatch close --backfill-from --skip-integration-check with "+tc.name)
		if after := readCloseEffects(t, conn, runID, stepID); after != before {
			t.Errorf("%s: the refused skip wrote %+v -> %+v", tc.name, before, after)
		}
	}

	t.Setenv(TokenEnvVar, token)
	payload, err := skipClose(conn, runID, path, "")
	testsupport.Must(t, err, "dispatch close --backfill-from --skip-integration-check with the run's token: %v", err)
	assertSkipRecorded(t, payload, "close")
	after := readCloseEffects(t, conn, runID, stepID)
	if after.usageRows != before.usageRows+1 || after.dispatchStatus == "open" ||
		after.reasonEvents != before.reasonEvents+1 {
		t.Errorf("effects %+v -> %+v, want one usage row and a close carrying the reason", before, after)
	}
}

// TestDispatchCloseSkipIntegrationTakesTheTokenFromTheEnvironmentWhenStdinIsTheBatch:
// with `--backfill-from -` stdin carries the usage rows, so the token is read
// from DOCKET_TOKEN alone and never consumes the batch.
func TestDispatchCloseSkipIntegrationTakesTheTokenFromTheEnvironmentWhenStdinIsTheBatch(t *testing.T) {
	conn := newTestDB(t)
	runID, stepID, token := skipCloseWave(t, conn)
	batch := fmt.Sprintf(`[{"step":%q,"unit":"tokens","quantity":900}]`,
		model.FormatStepID(stepID))
	before := readCloseEffects(t, conn, runID, stepID)

	t.Setenv(TokenEnvVar, "")
	_, err := skipClose(conn, runID, "-", batch)
	assertCmdCode(t, err, output.ErrValidation, "dispatch close --backfill-from - with no token")
	if after := readCloseEffects(t, conn, runID, stepID); after != before {
		t.Errorf("the refused skip wrote %+v -> %+v", before, after)
	}

	t.Setenv(TokenEnvVar, token)
	payload, err := skipClose(conn, runID, "-", batch)
	testsupport.Must(t, err, "dispatch close --backfill-from - with the run's token: %v", err)
	assertSkipRecorded(t, payload, "close")
	if after := readCloseEffects(t, conn, runID, stepID); after.usageRows != before.usageRows+1 {
		t.Errorf("effects %+v -> %+v, want the piped row written", before, after)
	}
}

// TestDispatchCloseSkipIntegrationPlainCloseNeedsNoToken: without the skip a
// bound run closes with no token, and stdin is never read.
func TestDispatchCloseSkipIntegrationPlainCloseNeedsNoToken(t *testing.T) {
	conn := newTestDB(t)
	runID, stepID, _ := skipCloseWave(t, conn)
	backfillStandalone(t, conn, runID, stepID)
	t.Setenv(TokenEnvVar, "")

	cmd := dispatchCloseCmdWithDB(conn, model.FormatRunID(runID))
	cmd.SetIn(tripwireReader{t})
	w, buf := bufWriter(true)
	err := runDispatchClose(cmd, w)
	testsupport.Must(t, err, "plain dispatch close with no token: %v", err)
	if strings.Contains(buf.String(), skipReason) {
		t.Errorf("a plain close recorded a skip:\n%s", buf.String())
	}
	if after := readCloseEffects(t, conn, runID, stepID); after.dispatchStatus == "open" {
		t.Errorf("the dispatch is still open after a plain close: %+v", after)
	}
}

// TestDispatchCloseSkipIntegrationOnAnUnboundRunNeedsNoToken: a run with no
// capability minted takes the skip without a token and records the reason.
func TestDispatchCloseSkipIntegrationOnAnUnboundRunNeedsNoToken(t *testing.T) {
	conn := newTestDB(t)
	runID, stepID, _ := skipCloseWave(t, conn)
	backfillStandalone(t, conn, runID, stepID)
	_, err := conn.Exec(`UPDATE runs SET conductor_token_hash = NULL WHERE id = ?`, runID)
	testsupport.Must(t, err, "unbinding: %v", err)
	t.Setenv(TokenEnvVar, "")

	payload, err := skipClose(conn, runID, "", "")
	testsupport.Must(t, err, "dispatch close --skip-integration-check on an unbound run: %v", err)
	assertSkipRecorded(t, payload)
	if after := readCloseEffects(t, conn, runID, stepID); after.dispatchStatus == "open" || after.reasonEvents != 1 {
		t.Errorf("effects %+v, want the dispatch closed with the reason recorded", after)
	}
}
