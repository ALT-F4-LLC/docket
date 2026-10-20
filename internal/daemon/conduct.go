package daemon

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Transport is the listener a request reached the daemon through. The zero
// value is the socket, so a caller that never sets it gets the stricter rule.
type Transport int

const (
	TransportSocket Transport = iota
	TransportHTTP
)

// ConductRun is engine.ConductRun with the daemon's transport rule: the first
// conductor of an unbound run is seated only over HTTP, and the Unix socket is
// refused with AUTH_ERROR. A bound run is re-seated over either transport.
//
// The binding is read before the seat in its own query. That is safe because
// a run never returns to unbound: every write of its capability hash stores a
// freshly minted one.
func ConductRun(conn *sql.DB, runID int, via Transport, opts engine.ConductOptions) (*engine.ConductResult, error) {
	switch hash, err := db.RunConductorHash(conn, runID); {
	case errors.Is(err, db.ErrRunNotFound):
		// engine.ConductRun reports the missing run as NOT_FOUND.
	case err != nil:
		return nil, err
	case hash == "" && via != TransportHTTP:
		ref := model.FormatRunID(runID)
		return nil, &engine.Error{Code: engine.CodeAuth, Message: fmt.Sprintf(
			"%s holds no conductor capability yet, and its first conductor is "+
				"seated only over the daemon's HTTP listener: run `docket run "+
				"conduct %s` against the HTTP URL, not the Unix socket",
			ref, ref)}
	}
	return engine.ConductRun(conn, runID, opts)
}
