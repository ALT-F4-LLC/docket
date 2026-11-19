package api

import (
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// Service implements the daemon's DocketService. Each RPC is a method whose
// body is one unary call; an RPC with no method answers unimplemented.
type Service struct {
	docketv1connect.UnimplementedDocketServiceHandler
	store store.Store
	build string
}

// NewService returns a Service over st that reports build as its version.
func NewService(st store.Store, build string) *Service {
	return &Service{store: st, build: build}
}
