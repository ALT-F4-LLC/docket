package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func TestGateHandlersBindVerbLayer(t *testing.T) {
	ctx := context.Background()
	conn, first, gate := activatedClaimDB(t)
	server := serve(t, NewService(store.StoreOn(conn), "dev"))
	rpc := client.NewClient(server.Client(), server.URL, discardLogger)

	status, err := rpc.GateStatus(ctx, connect.NewRequest(&docketv1.GateStatusRequest{Step: model.FormatStepID(gate)}))
	requireServed(t, "GateStatus", err)
	if got := status.Msg.GetOutcome(); got != string(engine.GateOutcomeOpen) {
		t.Errorf("GateStatus outcome = %q, want %q for an undecided human gate", got, engine.GateOutcomeOpen)
	}
	if status.Msg.GetStep() == "" {
		t.Error("GateStatus step is empty, want the gate step's effective status")
	}
	if status.Msg.GetTally() != "" || len(status.Msg.GetMissingSeats()) != 0 {
		t.Errorf("GateStatus reported vote state for a human gate: %v", status.Msg)
	}

	_, err = rpc.GateStatus(ctx, connect.NewRequest(&docketv1.GateStatusRequest{Step: model.FormatStepID(first)}))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("GateStatus on a non-gate step code = %q, want %q: the verb's refusal reaches the client (err %v)", got, output.ErrValidation, err)
	}
}

func TestEncodeGateStatusFormatsTheTally(t *testing.T) {
	score := 0.75
	for _, tc := range []struct {
		name  string
		tally *engine.GateTally
		want  string
	}{
		{"no proposal", nil, ""},
		{"unscored", &engine.GateTally{Threshold: 0.6}, "unscored/0.6"},
		{"scored", &engine.GateTally{WeightedScore: &score, Threshold: 0.6}, "0.75/0.6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := encodeGateStatus(verb.GateStatusResult{GateStatusResult: engine.GateStatusResult{Tally: tc.tally}})
			if got := res.GetTally(); got != tc.want {
				t.Errorf("tally = %q, want %q", got, tc.want)
			}
		})
	}
}
