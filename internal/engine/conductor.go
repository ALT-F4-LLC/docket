package engine

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// The CONDUCTOR CAPABILITY (DKT-2465).
//
// The operator verbs were token-free: "the authority is repository access".
// Under a harness every executor a wave spawns shares the operator's
// checkout, filesystem and environment, so repository access resolved to "any
// executor" — an executor could approve the commit-gate its own `git commit`
// is guarded on, reject a sibling's gate, or park the run — and the engine had
// no field on which to tell a conductor's ruling from an executor's. The only
// thing keeping executors off these verbs was a harness hook, which the engine
// cannot see and which covers one harness's callers only.
//
// The capability is a RUN-SCOPED token on the lease pattern: 256 bits from
// model.MintToken, hash-only storage (`runs.conductor_token_hash`), the same
// env/stdin transport (`DOCKET_TOKEN`, never argv) and the same child-process
// scrub (exec.deniedEnv). It is minted at the run's FIRST ACTIVATION and
// returned once in that response, and re-minted by `run conduct`. On a bound
// run it is required by every verb whose engine path calls authorizeConductor
// or authorizeConductorTx. An executor is never handed it, so the documented
// path refuses it the way `step record` refuses an unclaimed worker.
//
// TWO POSTURES ARE DELIBERATE AND STATED:
//
//   - An UNBOUND run (activated before v29 and never conducted) stays open to
//     the verbs exactly as before — the check binds the moment a capability
//     exists. This is the "guard with no engine = allow" posture: nothing is
//     required where nothing was ever minted, and a migrated store's runs in
//     flight keep working. Every run THIS binary activates is bound at birth.
//   - `run conduct` is itself token-free, because nothing authenticates a
//     caller and a run whose conductor session died must not be un-pausable
//     forever. That makes the mechanism TAMPER-EVIDENT rather than
//     tamper-proof: a caller that takes the seat retires the standing token,
//     so the standing conductor's next ruling refuses AUTH_ERROR, and the
//     `conductor-seated` event names the taker's actor and cwd, and the token
//     name and peer address the daemon observed. A harness that
//     keys callers keeps its executors off `run conduct` rather than off every
//     gated verb.

// ErrNotConductor is the sentinel under an AUTH_ERROR refusal, so a caller
// mapping errors can test for it without matching the message.
var ErrNotConductor = errors.New("not the run's conductor")

// authorizeConductorTx is the check inside a verb's own transaction.
func authorizeConductorTx(tx *sql.Tx, runID int, token, verb string) error {
	hash, err := store.TxOn(tx).RunConductorHashTx(runID)
	if err != nil {
		return err
	}
	return checkConductor(hash, token, runID, verb)
}

// authorizeConductor is the check on the connection, for the two rulings whose
// transactions open deep inside branch-specific paths (approve/reject and
// resolve). It runs after the step is loaded and before any of those paths
// writes; a `run conduct` racing it can only retire a token the ruling was
// about to use, which is the rotation doing its job.
func authorizeConductor(conn *sql.DB, runID int, token, verb string) error {
	hash, err := store.StoreOn(conn).RunConductorHash(runID)
	if err != nil {
		return err
	}
	return checkConductor(hash, token, runID, verb)
}

// AuthorizeConductor is authorizeConductor for a CLI verb whose write lives
// outside the engine (`vote commit`): the verb runs it before writing.
func AuthorizeConductor(conn *sql.DB, runID int, token, verb string) error {
	return authorizeConductor(conn, runID, token, verb)
}

// checkConductor is the refusal matrix, in the lease matrix's shape: no
// capability minted → allow; none supplied → a VALIDATION_ERROR naming both
// channels; wrong → an AUTH_ERROR. The messages never echo the presented
// token, and both name the recovery verb, because the caller most likely to
// hit them is a fresh session driving a run it did not activate.
func checkConductor(hash, token string, runID int, verb string) error {
	if hash == "" {
		return nil
	}
	ref := model.FormatRunID(runID)
	if token == "" {
		return validationErr(
			"%s is bound to a conductor capability and %s requires it: supply the "+
				"run's token via DOCKET_TOKEN or stdin (never argv). It was returned "+
				"once when the run activated; a session that does not hold it takes "+
				"the seat with `docket run conduct %s`, which re-mints it",
			ref, verb, ref)
	}
	if !model.TokenMatches(token, hash) {
		return &Error{Code: CodeAuth, Err: ErrNotConductor, Message: fmt.Sprintf(
			"the supplied token is not %s's conductor capability, which %s "+
				"requires. A session that does not hold the current token takes "+
				"the seat with `docket run conduct %s`, which re-mints it and "+
				"retires the old one",
			ref, verb, ref)}
	}
	return nil
}

// ConductOptions are `run conduct`'s inputs.
type ConductOptions struct {
	// By is who is taking the seat and from where. REQUIRED, exactly as on a
	// ruling: the event is the whole audit trail of a token-free verb that
	// mints authority, so an unattributed one must not be written.
	By Attribution
	// Caller is what the daemon observed about the request. The zero value is
	// a caller with no daemon-observed identity: the Unix socket, or a direct
	// store write.
	Caller DaemonCaller
	NowMS  int64
}

// DaemonCaller is the identity the daemon observed on a request, as opposed
// to Attribution's self-reported actor and cwd. TokenName names the token
// that authenticated an HTTP request; PeerAddr is the remote address the
// HTTP listener saw. A Unix-socket request carries neither.
type DaemonCaller struct {
	TokenName string
	PeerAddr  string
}

// addTo writes the caller onto an event's field map. `peer_addr` is always
// written, empty for a caller the daemon saw no address for; `token_name` is
// present only when a token authenticated the request, so a socket seat never
// reads as token-authenticated.
func (c DaemonCaller) addTo(fields map[string]any) map[string]any {
	fields["peer_addr"] = c.PeerAddr
	if c.TokenName != "" {
		fields["token_name"] = c.TokenName
	}
	return fields
}

// ConductResult is what the verb returns: the token, ONCE.
type ConductResult struct {
	Run *model.Run
	// Token is the freshly minted capability. It is never stored; only its
	// hash is.
	Token string
	// Rotated reports that a capability already stood and was retired by this
	// mint — the fact the standing conductor will discover at its next ruling,
	// stated up front to whoever just took the seat.
	Rotated bool
}

// ConductRun takes (or re-takes) a run's conductor seat: it mints the run's
// capability, retiring any standing one, and records who did it.
//
// It refuses a TERMINAL run: there is nothing left to conduct, and a token
// minted on a done run would be a capability for verbs that all refuse it
// anyway. A planning run is conductable — `run abandon` applies to one — and
// its first activation then keeps the capability rather than minting a second.
func ConductRun(conn *sql.DB, runID int, opts ConductOptions) (*ConductResult, error) {
	if err := opts.By.require("run conduct"); err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("conducting %s: %w", model.FormatRunID(runID), err)
	}
	defer tx.Rollback()
	st := store.TxOn(tx)

	run, err := st.GetRunTx(runID)
	if errors.Is(err, store.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}
	if run.Status == model.RunDone || run.Status == model.RunAbandoned {
		return nil, conflictErr("run %s is %s; there is nothing left to conduct",
			run.Ref(), run.Status)
	}

	standing, err := st.RunConductorHashTx(runID)
	if err != nil {
		return nil, err
	}
	token, hash, err := model.MintToken()
	if err != nil {
		return nil, err
	}
	if err := st.SetRunConductorHashTx(runID, hash); err != nil {
		return nil, err
	}

	rotated := standing != ""
	data, err := rulingData(opts.By, opts.Caller.addTo(map[string]any{"rotated": rotated}))
	if err != nil {
		return nil, fmt.Errorf("recording the conductor seat: %w", err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventConductorSeated, RunID: runID, Data: data, AtMS: opts.NowMS,
	}); err != nil {
		return nil, err
	}

	if err := commitWrite(tx); err != nil {
		return nil, fmt.Errorf("conducting %s: %w", model.FormatRunID(runID), err)
	}
	return &ConductResult{Run: run, Token: token, Rotated: rotated}, nil
}

// mintConductorTx binds a run at its first activation (DKT-2465), and is a
// no-op on a run already bound — one conducted while still planning keeps the
// capability its conductor holds. Returns the token, or "" when nothing was
// minted.
func mintConductorTx(tx *sql.Tx, runID int) (string, error) {
	st := store.TxOn(tx)
	standing, err := st.RunConductorHashTx(runID)
	if err != nil {
		return "", err
	}
	if standing != "" {
		return "", nil
	}
	token, hash, err := model.MintToken()
	if err != nil {
		return "", err
	}
	if err := st.SetRunConductorHashTx(runID, hash); err != nil {
		return "", err
	}
	return token, nil
}
