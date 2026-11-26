package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// ConfigGet reads one engine config value, or lists them all when no key is
// named.
func (s *Service) ConfigGet(ctx context.Context, req *connect.Request[docketv1.ConfigGetRequest]) (*connect.Response[docketv1.ConfigGetResponse], error) {
	return unary(ctx, s, req, decodeConfigGet, verb.ConfigGet, encodeConfigGet)
}

// ConfigSet stores one engine config value.
func (s *Service) ConfigSet(ctx context.Context, req *connect.Request[docketv1.ConfigSetRequest]) (*connect.Response[docketv1.ConfigSetResponse], error) {
	return unary(ctx, s, req, decodeConfigSet, verb.ConfigSet, encodeConfigSet)
}

func decodeConfigGet(msg *docketv1.ConfigGetRequest) (verb.ConfigGetInput, error) {
	return verb.ConfigGetInput{Key: msg.GetKey(), Global: msg.GetGlobal()}, nil
}

// encodeConfigGet puts the named key's entry, or the full listing, in values;
// ConfigGet fills exactly one of the two.
func encodeConfigGet(r verb.ConfigGetResult) *docketv1.ConfigGetResponse {
	values := make([]*docketv1.ConfigValue, 0, len(r.Entries)+1)
	for _, e := range r.Entries {
		values = append(values, &docketv1.ConfigValue{Key: e.Key, Value: e.Value, Source: e.Source})
	}
	if r.Entry.Key != "" {
		values = append(values, &docketv1.ConfigValue{Key: r.Entry.Key, Value: r.Entry.Value, Source: r.Entry.Source})
	}
	return &docketv1.ConfigGetResponse{Values: values}
}

func decodeConfigSet(msg *docketv1.ConfigSetRequest) (verb.ConfigSetInput, error) {
	return verb.ConfigSetInput{Key: msg.GetKey(), Value: msg.GetValue(), Global: msg.GetGlobal()}, nil
}

// encodeConfigSet carries the stored entry. ConfigSetResponse has no field for
// the result's Warning or Registered project, so neither reaches the client.
func encodeConfigSet(r verb.ConfigSetResult) *docketv1.ConfigSetResponse {
	return &docketv1.ConfigSetResponse{Value: &docketv1.ConfigValue{Key: r.Entry.Key, Value: r.Entry.Value, Source: r.Entry.Source}}
}
