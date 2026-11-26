package daemon

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// clientWindow is a hosted daemon's service with its handshake gated on the
// client API major window from the daemon config.
type clientWindow struct {
	docketv1connect.DocketServiceHandler
	min, max uint32
}

// WithClientWindow returns svc with Handshake refusing, as INCOMPATIBLE, a
// client whose declared API major lies outside cfg's inclusive window
// [MinClientAPIMajor, MaxClientAPIMajor]. Every other RPC passes to svc. The
// major is the client's own claim, so the window turns away clients that
// would misread the API; it is not access control.
func WithClientWindow(svc docketv1connect.DocketServiceHandler, cfg config.DaemonConfig) docketv1connect.DocketServiceHandler {
	return clientWindow{DocketServiceHandler: svc, min: cfg.MinClientAPIMajor, max: cfg.MaxClientAPIMajor}
}

func (w clientWindow) Handshake(ctx context.Context, req *connect.Request[docketv1.HandshakeRequest]) (*connect.Response[docketv1.HandshakeResponse], error) {
	if major := req.Msg.GetApiMajor(); major < w.min || major > w.max {
		return nil, output.NewConnectError(output.ErrIncompatible, fmt.Errorf(
			"this daemon admits client API majors %d through %d; the client speaks API major %d", w.min, w.max, major))
	}
	return w.DocketServiceHandler.Handshake(ctx, req)
}
