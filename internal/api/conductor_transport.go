package api

import "context"

// Transport is the listener a request reached the daemon through. The zero
// value is the Unix socket, so a request no listener stamped is held to the
// stricter conductor rule.
type Transport int

const (
	TransportSocket Transport = iota
	TransportHTTP
)

type transportKey struct{}

// WithTransport returns ctx recording via as the listener its request arrived
// on. Listener wiring sets it when it builds the server, never from anything
// the request carries.
func WithTransport(ctx context.Context, via Transport) context.Context {
	return context.WithValue(ctx, transportKey{}, via)
}

// TransportOf is the listener ctx's request arrived on, as WithTransport
// recorded it, or TransportSocket when nothing recorded one.
func TransportOf(ctx context.Context) Transport {
	via, _ := ctx.Value(transportKey{}).(Transport)
	return via
}
