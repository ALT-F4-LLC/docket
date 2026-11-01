package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// stepClaimer is the engine claim StepClaim binds to. It carries no template
// argument: the daemon renders only the default template, so it never opens
// a path a client chose.
type stepClaimer interface {
	ClaimStepWithGates(stepID int, opts engine.ClaimOptions) (*engine.ClaimResult, error)
	ClaimStepRendered(stepID int, opts engine.ClaimOptions, executor string) (*engine.ClaimResult, *engine.RenderResult, error)
}

// engineClaimer runs the engine's own claim entry points, so a daemon claim
// keeps every guarantee a CLI claim has: readiness, kind and budget refusals,
// the claim-refused event, and the secret-gated re-mint. store.Store has no
// claim verb, so it holds the store's raw handle.
type engineClaimer struct {
	engine *engine.Engine
	conn   store.LegacyDB
}

func (c engineClaimer) ClaimStepWithGates(stepID int, opts engine.ClaimOptions) (*engine.ClaimResult, error) {
	return c.engine.ClaimStepWithGates(c.conn, stepID, opts)
}

func (c engineClaimer) ClaimStepRendered(stepID int, opts engine.ClaimOptions, executor string) (*engine.ClaimResult, *engine.RenderResult, error) {
	return c.engine.ClaimStepRendered(c.conn, stepID, opts, "", executor)
}

// ClaimHandler serves StepClaim; every other RPC stays unimplemented here.
type ClaimHandler struct {
	docketv1connect.UnimplementedDocketServiceHandler
	claimer stepClaimer
}

// NewClaimHandler returns a ClaimHandler that claims through e on conn.
func NewClaimHandler(e *engine.Engine, conn store.LegacyDB) ClaimHandler {
	return ClaimHandler{claimer: engineClaimer{engine: e, conn: conn}}
}

// maxRecoverySecretBytes matches the CLI's cap on a recovery secret file.
const maxRecoverySecretBytes = 4096

// StepClaim takes or re-mints a lease. Every request field is checked before
// the engine is called, so a refused request takes no lease and writes nothing.
func (h ClaimHandler) StepClaim(_ context.Context, req *connect.Request[docketv1.StepClaimRequest]) (*connect.Response[docketv1.StepClaimResponse], error) {
	msg := req.Msg
	stepID, opts, err := parseClaim(msg)
	if err != nil {
		return nil, output.NewConnectError(output.ErrValidation, err)
	}

	var result *engine.ClaimResult
	var packet *engine.RenderResult
	if msg.GetRender() {
		result, packet, err = h.claimer.ClaimStepRendered(stepID, opts, msg.GetExecutor())
	} else {
		result, err = h.claimer.ClaimStepWithGates(stepID, opts)
	}

	var incomplete *engine.IncompleteClaimError
	if errors.As(err, &incomplete) {
		res := claimResponse(incomplete.Result, nil)
		res.ClaimError = incomplete.Error()
		return connect.NewResponse(res), nil
	}
	if err != nil {
		return nil, claimConnectError(err, stepID)
	}
	return connect.NewResponse(claimResponse(result, packet)), nil
}

// parseClaim checks a claim request at the trust boundary and returns the
// engine claim it names. The secret is forwarded byte for byte; it is trimmed
// only to test for emptiness, as the CLI does.
func parseClaim(msg *docketv1.StepClaimRequest) (int, engine.ClaimOptions, error) {
	secret := msg.GetRecoverySecret()
	if strings.TrimSpace(secret) == "" {
		return 0, engine.ClaimOptions{}, errors.New("recovery_secret is required")
	}
	if len(secret) > maxRecoverySecretBytes {
		return 0, engine.ClaimOptions{}, fmt.Errorf("recovery_secret exceeds %d bytes", maxRecoverySecretBytes)
	}
	if msg.GetTemplate() != "" {
		return 0, engine.ClaimOptions{}, errors.New("template is not accepted: the daemon renders the default template only")
	}
	if msg.GetOwner() == "" {
		return 0, engine.ClaimOptions{}, errors.New("owner is required: it identifies the lease holder")
	}
	if msg.TtlMs != nil && msg.GetTtlMs() <= 0 {
		return 0, engine.ClaimOptions{}, fmt.Errorf("ttl_ms must be positive, got %d", msg.GetTtlMs())
	}
	if msg.CostMultiplier != nil {
		m := msg.GetCostMultiplier()
		if !(m > 0) || math.IsInf(m, 0) {
			return 0, engine.ClaimOptions{}, fmt.Errorf("cost_multiplier must be positive and finite, got %g", m)
		}
	}
	stepID, err := model.ParseStepID(msg.GetStep())
	if err != nil {
		return 0, engine.ClaimOptions{}, fmt.Errorf("invalid step ID: %w", err)
	}
	return stepID, engine.ClaimOptions{
		Owner: msg.GetOwner(), TTLOverride: msg.GetTtlMs(), Metadata: msg.GetMetadataJson(),
		CostMultiplier: msg.GetCostMultiplier(), RecoverySecret: secret,
		NowMS: model.NowMS(),
	}, nil
}

func claimResponse(result *engine.ClaimResult, packet *engine.RenderResult) *docketv1.StepClaimResponse {
	res := &docketv1.StepClaimResponse{
		Step: result.Step, Token: result.Token, LeaseExpiresMs: result.LeaseExpiresMS,
		ReMinted: result.ReMinted, Attempt: int32(result.Attempt), Version: int32(result.RowVersion),
	}
	if packet != nil {
		res.Packet = packet.Packet
	}
	return res
}

// claimConnectError maps a claim refusal onto the docket error taxonomy, as
// the CLI's `step claim` does.
func claimConnectError(err error, stepID int) *connect.Error {
	if code, ok := engine.CodeOf(err); ok {
		return output.NewConnectError(output.ErrorCode(code), err)
	}
	label := "step " + model.FormatStepID(stepID)
	switch {
	case errors.Is(err, store.ErrStepNotFound):
		return output.NewConnectError(output.ErrNotFound, fmt.Errorf("%s not found", label))
	case errors.Is(err, store.ErrLeaseHeld):
		return output.NewConnectError(output.ErrConflict, fmt.Errorf("%s is already claimed by another holder", label))
	default:
		return output.NewConnectError(output.ErrGeneral, err)
	}
}
