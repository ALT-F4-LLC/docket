package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/store/storetest"
)

func openProjectStore(t *testing.T) store.Store {
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

// requireServed fails when err is the answer an RPC with no Service method
// gives, then on any other error.
func requireServed(t *testing.T, rpc string, err error) {
	t.Helper()
	var connectErr *connect.Error
	if errors.As(err, &connectErr) && connectErr.Code() == connect.CodeUnimplemented {
		t.Fatalf("%s answered unimplemented: no *Service method binds it to the verb layer", rpc)
	}
	if err != nil {
		t.Fatalf("%s: %v", rpc, err)
	}
}

// inProject returns a request that names identity as the caller's project.
func inProject[T any](msg *T, identity string) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set(projectHeader, identity)
	return req
}

func TestProjectHandlersBindVerbLayer(t *testing.T) {
	const mine, spare = "/repos/mine", "/repos/spare"
	st := openProjectStore(t)
	for _, identity := range []string{mine, spare} {
		if _, err := st.EnsureProject(identity, filepath.Base(identity), 1); err != nil {
			t.Fatalf("EnsureProject(%s): %v", identity, err)
		}
	}
	server := serve(t, NewService(st, "test"))
	c := client.NewClient(server.Client(), server.URL, discardLogger)
	ctx := context.Background()

	list, err := c.ProjectList(ctx, inProject(&docketv1.ProjectListRequest{}, mine))
	requireServed(t, "ProjectList", err)
	listed := map[string]bool{}
	for _, p := range list.Msg.GetItems() {
		listed[p.GetIdentity()] = true
	}
	if !listed[mine] || !listed[spare] {
		t.Errorf("ProjectList items = %v, want both %s and %s", list.Msg.GetItems(), mine, spare)
	}
	if got, want := list.Msg.GetTotal(), int32(len(list.Msg.GetItems())); got != want {
		t.Errorf("ProjectList total = %d, want the item count %d", got, want)
	}

	set, err := c.ProjectSetPrefix(ctx, inProject(&docketv1.ProjectSetPrefixRequest{Prefix: " abc "}, mine))
	requireServed(t, "ProjectSetPrefix", err)
	if got := set.Msg.GetProject(); got.GetIdentity() != mine || got.GetPrefix() != "ABC" {
		t.Errorf("ProjectSetPrefix project = %v, want %s with prefix ABC", got, mine)
	}

	deleted, err := c.ProjectDelete(ctx, inProject(&docketv1.ProjectDeleteRequest{Project: spare}, mine))
	requireServed(t, "ProjectDelete", err)
	if got := deleted.Msg.GetProject().GetIdentity(); got != spare {
		t.Errorf("ProjectDelete project identity = %q, want %q", got, spare)
	}
	if _, found, err := st.LookupProject(spare); err != nil || found {
		t.Errorf("LookupProject(%s) after delete = found %v, err %v; want gone", spare, found, err)
	}
}
