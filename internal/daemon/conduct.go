package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// Transport is the listener a request reached the daemon through, as
// api.TransportOf reads it from the request's context. The zero value is the
// socket, so a caller that never sets it gets the stricter rule.
type Transport = api.Transport

const (
	TransportSocket = api.TransportSocket
	TransportHTTP   = api.TransportHTTP
)

// ConductRun is engine.ConductRun with the daemon's transport rule: the first
// conductor of an unbound run is seated only over HTTP, and the Unix socket is
// refused with AUTH_ERROR. A bound run is re-seated over either transport.
func ConductRun(conn *sql.DB, runID int, via Transport, opts engine.ConductOptions) (*engine.ConductResult, error) {
	if err := requireSeatTransport(conn, runID, via); err != nil {
		return nil, err
	}
	return engine.ConductRun(conn, runID, opts)
}

// requireSeatTransport is ConductRun's transport rule: it refuses with
// AUTH_ERROR a seat over anything but HTTP on a run that holds no conductor
// capability.
//
// The binding is read before the seat in its own query. That is safe because
// a run never returns to unbound: every write of its capability hash stores a
// freshly minted one.
func requireSeatTransport(conn *sql.DB, runID int, via Transport) error {
	switch hash, err := store.StoreOn(conn).RunConductorHash(runID); {
	case errors.Is(err, store.ErrRunNotFound):
		// The seat reports the missing run as NOT_FOUND.
	case err != nil:
		return err
	case hash == "" && via != TransportHTTP:
		ref := model.FormatRunID(runID)
		return &engine.Error{Code: engine.CodeAuth, Message: fmt.Sprintf(
			"%s holds no conductor capability yet, and its first conductor is "+
				"seated only over the daemon's HTTP listener: run `docket run "+
				"conduct %s` against the HTTP URL, not the Unix socket",
			ref, ref)}
	}
	return nil
}

// conductorRule is a daemon's service with RunConduct held to ConductRun's
// transport rule.
type conductorRule struct {
	docketv1connect.DocketServiceHandler
	store store.Store
}

// RunConduct refuses the seat wherever ConductRun's transport rule refuses it
// for the listener api.TransportOf reads from ctx, and otherwise passes the
// request to the wrapped service, which seats the conductor. A run reference
// that does not parse passes through for the service to refuse.
func (c conductorRule) RunConduct(ctx context.Context, req *connect.Request[docketv1.RunConductRequest]) (*connect.Response[docketv1.RunConductResponse], error) {
	if runID, err := model.ParseRunID(req.Msg.GetRun()); err == nil {
		conn, err := store.SQLDB(c.store)
		if err == nil {
			err = requireSeatTransport(conn, runID, api.TransportOf(ctx))
		}
		if err != nil {
			code := output.ErrGeneral
			if engineCode, _ := engine.CodeOf(err); engineCode == engine.CodeAuth {
				code = output.ErrAuth
			}
			return nil, output.NewConnectError(code, err)
		}
	}
	return c.DocketServiceHandler.RunConduct(ctx, req)
}
