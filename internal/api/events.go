package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// EventsList reads one page of the event log after a cursor.
func (s *Service) EventsList(ctx context.Context, req *connect.Request[docketv1.EventsListRequest]) (*connect.Response[docketv1.EventsListResponse], error) {
	return unary(ctx, s, req, decodeEventsList, verb.EventsList, encodeEventsList)
}

// EventsPrune deletes events below a seq, or reports what it would delete.
func (s *Service) EventsPrune(ctx context.Context, req *connect.Request[docketv1.EventsPruneRequest]) (*connect.Response[docketv1.EventsPruneResponse], error) {
	return unary(ctx, s, req, decodeEventsPrune, verb.EventsPrune, encodeEventsPrune)
}

func decodeEventsList(msg *docketv1.EventsListRequest) (verb.EventsListInput, error) {
	return verb.EventsListInput{Since: msg.GetSince(), Limit: int(msg.GetLimit()), Run: msg.GetRun()}, nil
}

// encodeEventsList sets next_cursor to the page's last seq, the cursor the
// next call passes as since. An empty page leaves it zero: the caller keeps
// the cursor it sent.
func encodeEventsList(res verb.EventsListResult) *docketv1.EventsListResponse {
	out := &docketv1.EventsListResponse{
		Total:     int32(res.Total),
		Truncated: output.IsTruncated(res.Limit, res.Total, len(res.Events)),
	}
	for _, e := range res.Events {
		out.Items = append(out.Items, &docketv1.Event{
			Seq: e.Seq, Kind: e.Kind, Run: e.Run, Step: e.Step, Issue: e.Issue,
			PayloadJson: string(e.Data), AtMs: e.AtMS,
		})
		out.NextCursor = e.Seq
	}
	return out
}

// decodeEventsPrune leaves Yes unset: the request has no confirmation field,
// so the verb refuses every deletion and answers only a dry run.
func decodeEventsPrune(msg *docketv1.EventsPruneRequest) (verb.EventsPruneInput, error) {
	return verb.EventsPruneInput{Before: msg.GetBefore(), DryRun: msg.GetDryRun()}, nil
}

// encodeEventsPrune also encodes a refused prune's partial result, which
// carries no prune outcome.
func encodeEventsPrune(res verb.EventsPruneResult) *docketv1.EventsPruneResponse {
	if res.Prune == nil {
		return &docketv1.EventsPruneResponse{}
	}
	return &docketv1.EventsPruneResponse{Pruned: int64(res.Prune.Pruned), RetainedMinSeq: res.Prune.RetainedMinimum}
}
