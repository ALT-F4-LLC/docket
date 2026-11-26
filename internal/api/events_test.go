package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/store/storetest"
)

// eventsClient serves a Service over a fresh store and returns a Connect
// client for it.
func eventsClient(t *testing.T) docketv1connect.DocketServiceClient {
	t.Helper()
	server := serve(t, NewService(openAPIStore(t), "dev"))
	return client.NewClient(server.Client(), server.URL, discardLogger)
}

// openAPIStore opens a fresh, migrated SQLite store.
func openAPIStore(t *testing.T) store.Store {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "issues.db")
	storetest.SeedFile(t, storePath)
	st, err := store.OpenSQLite(storePath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

func TestEventsHandlersBindVerbLayer(t *testing.T) {
	c := eventsClient(t)
	ctx := context.Background()
	calls := map[string]func() error{
		"EventsList": func() error {
			_, err := c.EventsList(ctx, connect.NewRequest(&docketv1.EventsListRequest{}))
			return err
		},
		"EventsPrune": func() error {
			_, err := c.EventsPrune(ctx, connect.NewRequest(&docketv1.EventsPruneRequest{Before: 1, DryRun: true}))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if connect.CodeOf(err) == connect.CodeUnimplemented {
				t.Fatalf("%s answered unimplemented: no *Service method serves it", name)
			}
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		})
	}
}

// The wire carries no confirmation field, so a deletion request reaches the
// verb unconfirmed and is refused; only a dry run succeeds.
func TestEventsPruneRefusesDeletionOverConnect(t *testing.T) {
	c := eventsClient(t)

	_, err := c.EventsPrune(context.Background(), connect.NewRequest(&docketv1.EventsPruneRequest{Before: 1}))

	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("code = %q, want %q (err %v)", got, output.ErrValidation, err)
	}
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("err = %v, want the unconfirmed-deletion refusal", err)
	}
}

func TestEventsListEncodesThePage(t *testing.T) {
	st := openAPIStore(t)
	tx, err := st.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	for i := range 2 {
		if err := tx.InsertEventTx(store.NewEvent{AtMS: int64(10 + i), Kind: "run-activated", Data: `{"n":1}`}); err != nil {
			t.Fatalf("InsertEventTx: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	server := serve(t, NewService(st, "dev"))
	c := client.NewClient(server.Client(), server.URL, discardLogger)

	res, err := c.EventsList(context.Background(), connect.NewRequest(&docketv1.EventsListRequest{Limit: 1}))

	if err != nil {
		t.Fatalf("EventsList: %v", err)
	}
	items := res.Msg.GetItems()
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 under limit 1", len(items))
	}
	first := items[0]
	if first.GetKind() != "run-activated" || first.GetAtMs() != 10 || first.GetPayloadJson() != `{"n":1}` {
		t.Errorf("item = %v, want the first seeded event", first)
	}
	if got := res.Msg.GetNextCursor(); got != first.GetSeq() {
		t.Errorf("next_cursor = %d, want the page's last seq %d", got, first.GetSeq())
	}
	if res.Msg.GetTotal() != 2 || !res.Msg.GetTruncated() {
		t.Errorf("total = %d, truncated = %v; want 2, true", res.Msg.GetTotal(), res.Msg.GetTruncated())
	}
}
