package api

import (
	"context"
	"errors"

	"github.com/ALT-F4-LLC/docket/internal/output"
)

// refuseClientWorktreeOverHTTP refuses a request that arrived over HTTP and
// names a worktree. The engine runs git and gates in a worktree on the
// daemon's own filesystem, and an HTTP client's checkouts live on its own
// machine, so any non-empty value, absolute or relative, is refused. Socket
// clients share the daemon's host and may name one. A WorkflowRegister
// source_path sent over HTTP is dropped at ingress and never stored or read.
func refuseClientWorktreeOverHTTP(ctx context.Context, msg any) error {
	if TransportOf(ctx) != TransportHTTP {
		return nil
	}
	named, ok := msg.(interface{ GetWorktree() string })
	if !ok || named.GetWorktree() == "" {
		return nil
	}
	return output.NewConnectError(output.ErrValidation, errors.New(
		"the request names a worktree, which the daemon would open on its own filesystem; "+
			"worktree is accepted only over the local socket"))
}
