// Package verb holds the verb logic the CLI and the daemon's Connect handlers
// share: validation, ref parsing, project resolution and authorization run
// here once, and each transport only decodes its input and encodes the result.
//
// The package imports neither transport (internal/cli, internal/api and the
// packages under it), cobra, connect nor database/sql; it reaches storage
// only through store.Store.
package verb

import "github.com/ALT-F4-LLC/docket/internal/store"

// Env is what a transport supplies to every verb call.
type Env struct {
	Store store.Store
	// Author attributes the writes the verb makes. The CLI fills it from
	// config.DefaultAuthor(); the daemon from the client's request headers.
	Author string
	// Cwd is the caller's working directory, which the engine records on
	// rulings. The daemon fills it from the client's request headers.
	Cwd     string
	Project ProjectInput
	Build   BuildInfo
}

// ProjectInput is the caller's project identity before resolution.
type ProjectInput struct {
	Identity string
	// Anchored reports whether Identity names a deliberate project rather
	// than the directory the caller happened to start in, as
	// config.Config.Anchored does; an unanchored identity never registers a
	// project.
	Anchored bool
}

// BuildInfo is the build a Handshake reports.
type BuildInfo struct {
	Version   string
	Commit    string
	BuildDate string
}
