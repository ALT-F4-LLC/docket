package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
)

func TestRequestSizeCapRejectsOversize(t *testing.T) {
	var handlerRan atomic.Bool
	handler := connect.NewUnaryHandler(echoRunProcedure,
		func(_ context.Context, req *connect.Request[docketv1.Run]) (*connect.Response[docketv1.Run], error) {
			handlerRan.Store(true)
			return connect.NewResponse(req.Msg), nil
		},
		handlerOptions(discardLogger),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	oversize := &docketv1.Run{Id: strings.Repeat("x", requestMaxBytes)}

	codecs := []struct {
		name   string
		option connect.ClientOption
	}{
		{"json", connect.WithProtoJSON()},
		{"proto", connect.WithClientOptions()},
		{"gzip json under the cap on the wire", connect.WithClientOptions(connect.WithProtoJSON(), connect.WithSendGzip())},
	}
	for _, codec := range codecs {
		t.Run(codec.name, func(t *testing.T) {
			handlerRan.Store(false)
			client := connect.NewClient[docketv1.Run, docketv1.Run](server.Client(), server.URL+echoRunProcedure, codec.option)

			_, err := client.CallUnary(context.Background(), connect.NewRequest(oversize))

			if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
				t.Errorf("connect code = %v, want %v (error: %v)", got, connect.CodeResourceExhausted, err)
			}
			if handlerRan.Load() {
				t.Error("handler ran on a request over the size cap")
			}

			res, err := client.CallUnary(context.Background(), connect.NewRequest(&docketv1.Run{Id: "RUN-1"}))
			if err != nil {
				t.Fatalf("request under the cap rejected: %v", err)
			}
			if !handlerRan.Load() || res.Msg.GetId() != "RUN-1" {
				t.Errorf("handler ran = %v, echoed id = %q; want the request under the cap served", handlerRan.Load(), res.Msg.GetId())
			}
		})
	}

	// A body that is not JSON at all fails as invalid_argument once decoded;
	// resource_exhausted shows the cap refused it before any decode.
	t.Run("undecodable body", func(t *testing.T) {
		handlerRan.Store(false)
		body := strings.NewReader(strings.Repeat("!", requestMaxBytes+1))
		res, err := server.Client().Post(server.URL+echoRunProcedure, "application/json", body)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer res.Body.Close()
		var wire struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(res.Body).Decode(&wire); err != nil {
			t.Fatalf("decode error body (status %d): %v", res.StatusCode, err)
		}
		if wire.Code != connect.CodeResourceExhausted.String() {
			t.Errorf("wire code = %q (status %d), want %q", wire.Code, res.StatusCode, connect.CodeResourceExhausted)
		}
		if handlerRan.Load() {
			t.Error("handler ran on an undecodable request over the size cap")
		}
	})
}
