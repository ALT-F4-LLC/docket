package api

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

func TestUnaryMapsDecodeFailureToValidationWithoutRunningOp(t *testing.T) {
	ran := false
	decode := func(*docketv1.HandshakeRequest) (string, error) { return "", errors.New("bad field") }
	op := func(context.Context, *Service, string) (string, error) {
		ran = true
		return "", nil
	}

	_, err := unary(context.Background(), NewService(nil, "dev"),
		connect.NewRequest(&docketv1.HandshakeRequest{}), decode, op, encodeHandshake)

	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("code = %q, want %q (err %v)", got, output.ErrValidation, err)
	}
	if ran {
		t.Error("op ran on a request that failed to decode")
	}
}

func TestUnaryMapsOpFailureToGeneralError(t *testing.T) {
	decode := func(*docketv1.HandshakeRequest) (string, error) { return "in", nil }
	op := func(context.Context, *Service, string) (string, error) { return "", errors.New("op failed") }

	_, err := unary(context.Background(), NewService(nil, "dev"),
		connect.NewRequest(&docketv1.HandshakeRequest{}), decode, op, encodeHandshake)

	if err == nil {
		t.Fatal("unary returned no error for a failed op")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Errorf("connect code = %v, want %v, the code GENERAL_ERROR carries", got, connect.CodeInternal)
	}
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrGeneral {
		t.Errorf("code = %q, want %q", got, output.ErrGeneral)
	}
}
