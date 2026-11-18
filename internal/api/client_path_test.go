package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// TestClientPathFieldsRefusedOverHTTP: a request that names a worktree, a
// directory the daemon would open on its own filesystem, is refused with
// InvalidArgument over HTTP before it reaches the engine, and the same request
// over the socket is served. A source_path is provenance and is accepted over
// HTTP.
func TestClientPathFieldsRefusedOverHTTP(t *testing.T) {
	ctx := context.Background()

	t.Run("StepComplete", func(t *testing.T) {
		f := newStepFixture(t)
		overHTTP := serveStampedHTTP(t, f.st)
		steps, _ := f.boundRun(t)
		id := steps["first@0"]
		token := f.claim(t, id)
		complete := func(worktree string) *connect.Request[docketv1.StepCompleteRequest] {
			return connect.NewRequest(&docketv1.StepCompleteRequest{
				Step: model.FormatStepID(id), Token: token, Artifact: "result", Worktree: worktree,
			})
		}
		before := f.state(t, id)

		for _, worktree := range []string{f.execRoot, "rel/dir"} {
			_, err := overHTTP.StepComplete(ctx, complete(worktree))
			requireRefusal(t, "HTTP StepComplete with worktree "+worktree, err, connect.CodeInvalidArgument, output.ErrValidation)
			if got := f.state(t, id); got != before {
				t.Fatalf("the refused HTTP StepComplete moved the step from %+v to %+v", before, got)
			}
		}

		_, err := f.c.StepComplete(ctx, complete(f.execRoot))
		requireServed(t, "socket StepComplete", err)
		if got := f.state(t, id).status; got != store.StepDone {
			t.Errorf("socket StepComplete left the step %q, want %q", got, store.StepDone)
		}
	})

	t.Run("StepResolve", func(t *testing.T) {
		f := newStepFixture(t)
		overHTTP := serveStampedHTTP(t, f.st)
		steps, conductor := f.boundRun(t)
		id := steps["first@0"]
		_, err := f.c.StepFail(ctx, connect.NewRequest(&docketv1.StepFailRequest{
			Step: model.FormatStepID(id), Token: f.claim(t, id), Note: "gave up",
		}))
		requireServed(t, "StepFail", err)
		f.commit(t, "the patch\n", "patch")
		resolve := connect.NewRequest(&docketv1.StepResolveRequest{
			Step: model.FormatStepID(id), As: engine.ResolveOverridePass, Worktree: f.execRoot,
			Authority: engine.AuthorityOperator, Token: conductor,
		})
		before := f.state(t, id)
		if before.status != store.StepWaitingHuman {
			t.Fatalf("premise: the failed step is %q, want %q", before.status, store.StepWaitingHuman)
		}

		_, err = overHTTP.StepResolve(ctx, resolve)
		requireRefusal(t, "HTTP StepResolve with worktree", err, connect.CodeInvalidArgument, output.ErrValidation)
		if got := f.state(t, id); got != before {
			t.Fatalf("the refused HTTP StepResolve moved the step from %+v to %+v", before, got)
		}

		_, err = f.c.StepResolve(ctx, resolve)
		requireServed(t, "socket StepResolve", err)
		if got := f.state(t, id).status; got == store.StepWaitingHuman {
			t.Errorf("socket StepResolve left the step %q", got)
		}
	})

	t.Run("source_path only", func(t *testing.T) {
		st := openProjectStore(t)
		overHTTP := serveStampedHTTP(t, st)
		res, err := overHTTP.WorkflowRegister(ctx, connect.NewRequest(&docketv1.WorkflowRegisterRequest{
			Body: unitWorkflowBody, SourcePath: "/defs/unit.toml",
		}))
		requireServed(t, "HTTP WorkflowRegister with source_path", err)
		if got := res.Msg.GetWorkflow().GetSourcePath(); got != "/defs/unit.toml" {
			t.Errorf("source_path = %q, want the sent /defs/unit.toml", got)
		}
	})
}

// serveStampedHTTP serves a Service over st with every request stamped
// TransportHTTP, as the daemon's HTTP listener stamps it, and returns a client
// for it.
func serveStampedHTTP(t *testing.T, st store.Store) docketv1connect.DocketServiceClient {
	t.Helper()
	path, handler := NewHandler(NewService(st, "test"), discardLogger)
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(WithTransport(r.Context(), TransportHTTP)))
	}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return client.NewClient(server.Client(), server.URL, discardLogger)
}
