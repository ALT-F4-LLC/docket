package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// PinShow reads the bytes a run pinned at a config-relative path when it
// activated.
func (s *Service) PinShow(ctx context.Context, req *connect.Request[docketv1.PinShowRequest]) (*connect.Response[docketv1.PinShowResponse], error) {
	return unary(ctx, s, req, decodePinShow, verb.PinShow, encodePinShow)
}

func decodePinShow(req *docketv1.PinShowRequest) (verb.PinShowInput, error) {
	return verb.PinShowInput{Run: req.GetRun(), Path: req.GetPath()}, nil
}

// encodePinShow hashes the body it carries. The verb refuses any body that
// does not match its pin, so this hash is the pinned sha256.
func encodePinShow(res verb.PinShowResult) *docketv1.PinShowResponse {
	sum := sha256.Sum256([]byte(res.Body))
	return &docketv1.PinShowResponse{
		Path:    res.Path,
		Sha256:  hex.EncodeToString(sum[:]),
		Content: []byte(res.Body),
	}
}
