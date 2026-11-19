package api

import (
	"context"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

func openConfigStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "issues.db"))
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

// TestConfigHandlersBindVerbLayer calls each config RPC over Connect against a
// real store: a set followed by a get must round-trip the value, which only a
// handler bound to the verb layer can do.
func TestConfigHandlersBindVerbLayer(t *testing.T) {
	const key, value = "attempt.max", "11"
	st := openConfigStore(t)
	server := serve(t, NewService(st, "dev"))
	c := client.NewClient(server.Client(), server.URL, discardLogger)
	ctx := context.Background()

	t.Run("ConfigSet", func(t *testing.T) {
		res, err := c.ConfigSet(ctx, connect.NewRequest(&docketv1.ConfigSetRequest{Key: key, Value: value, Global: true}))
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			t.Fatalf("ConfigSet answered unimplemented: %v", err)
		}
		if err != nil {
			t.Fatalf("ConfigSet: %v", err)
		}
		got := res.Msg.GetValue()
		if got.GetKey() != key || got.GetValue() != value || got.GetSource() != "set" {
			t.Errorf("ConfigSet value = %v, want %s=%s (set)", got, key, value)
		}
	})

	t.Run("ConfigGet", func(t *testing.T) {
		res, err := c.ConfigGet(ctx, connect.NewRequest(&docketv1.ConfigGetRequest{Key: key, Global: true}))
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			t.Fatalf("ConfigGet answered unimplemented: %v", err)
		}
		if err != nil {
			t.Fatalf("ConfigGet: %v", err)
		}
		values := res.Msg.GetValues()
		if len(values) != 1 || values[0].GetKey() != key || values[0].GetValue() != value || values[0].GetSource() != "set" {
			t.Errorf("ConfigGet values = %v, want the one stored entry %s=%s (set)", values, key, value)
		}
	})

	t.Run("ConfigGet lists every key when none is named", func(t *testing.T) {
		res, err := c.ConfigGet(ctx, connect.NewRequest(&docketv1.ConfigGetRequest{Global: true}))
		if err != nil {
			t.Fatalf("ConfigGet list: %v", err)
		}
		listed := false
		for _, v := range res.Msg.GetValues() {
			listed = listed || (v.GetKey() == key && v.GetValue() == value)
		}
		if !listed {
			t.Errorf("listing omits %s=%s: %v", key, value, res.Msg.GetValues())
		}
	})
}
