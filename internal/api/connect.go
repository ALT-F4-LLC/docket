// Package api holds the daemon's Connect API. `make proto` generates its Go
// messages (docketv1) and Connect client and handler stubs (docketv1connect)
// from proto/docket/v1/docket.proto; the generated files are committed.
package api

import "github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"

// The daemon mounts its service through the generated handler constructor.
// Binding it here makes the build fail when regeneration drops or renames it.
var _ = docketv1connect.NewDocketServiceHandler
