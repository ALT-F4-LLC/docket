package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
)

// CONFIG WRITES TO SECURITY-LOAD-BEARING KEYS ARE RECORDED (DKT-2767).
//
// `docket config set` has no authorization: any caller with the store can
// rewrite a vote rule's threshold or criticality, and the tally under that
// rule then judges every later ballot by a number nobody in the record chose.
// Prevention needs a per-caller identity the CLI does not have; what the
// engine can do today is make the rewrite VISIBLE — one `config-changed` event
// per successful write to a `vote.rule.<name>.*` key, in the same transaction
// as the write, carrying what was replaced, by what, and by whom (operator
// decision, 2026-09-17; emission through recordEvent, 2026-09-23).
//
// Roster, weighting and recusal are NOT among these keys any more: they moved
// onto the vote step (DKT-2562, DKT-2764) precisely because a config key could
// not carry authorization. What remains under `vote.rule.` — threshold,
// criticality, sealed, hold_on_dissent — still shapes how a tally decides, and
// that is what this records.

// SetConfig is the engine's config write: db.SetConfigTx plus, for a vote-rule
// key, the `config-changed` event, committed together or not at all.
//
// `changedBy` is the identity the activity log attributes issue edits to
// (config.DefaultAuthor at the CLI). It is recorded verbatim and unverified,
// like every other self-asserted name in the store — the point is a trail,
// not a proof. A write to a key outside the vote-rule family records nothing,
// and a refused write (unknown key, invalid value) records nothing because it
// wrote nothing.
func SetConfig(conn *sql.DB, projectID int, key, value, changedBy string) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("storing config %s: %w", key, err)
	}
	defer tx.Rollback()

	prior, err := db.SetConfigTx(tx, projectID, key, value)
	if err != nil {
		return err
	}
	if strings.HasPrefix(key, db.KeyVoteRulePrefix) {
		if err := recordConfigChange(tx, projectID, key, prior, value, changedBy); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storing config %s: %w", key, err)
	}
	return nil
}

// configChange is the `config-changed` event's data: the key, the project the
// write applied to (0 for `--global`), the scope that project id means, the
// value replaced (empty when the key was unset at that scope), the value
// written, and who wrote it.
type configChange struct {
	Key       string `json:"key"`
	Project   int    `json:"project"`
	Scope     string `json:"scope"`
	Prior     string `json:"prior"`
	Value     string `json:"value"`
	ChangedBy string `json:"changed_by"`
}

// recordConfigChange writes the event in the caller's transaction — the
// write's own, so the record and the change land together.
func recordConfigChange(tx *sql.Tx, projectID int, key, prior, value, changedBy string) error {
	scope := "project"
	if projectID == 0 {
		scope = "global"
	}
	data, err := json.Marshal(configChange{
		Key: key, Project: projectID, Scope: scope,
		Prior: prior, Value: value, ChangedBy: changedBy,
	})
	if err != nil {
		return fmt.Errorf("recording the config change to %s: %w", key, err)
	}
	return recordEvent(tx, eventRecord{Kind: EventConfigChanged, Data: string(data)})
}
