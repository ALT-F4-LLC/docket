package api

import (
	"context"
	"maps"
	"slices"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// Stats counts the caller's project's issues by status, priority and label.
func (s *Service) Stats(ctx context.Context, req *connect.Request[docketv1.StatsRequest]) (*connect.Response[docketv1.StatsResponse], error) {
	return unary(ctx, s, req, decodeStats, verb.Stats, encodeStats)
}

func decodeStats(*docketv1.StatsRequest) (verb.StatsInput, error) {
	return verb.StatsInput{}, nil
}

// encodeStats leaves by_type empty: verb.Stats does not count issues by type.
func encodeStats(out verb.StatsResult) *docketv1.StatsResponse {
	labels := make([]*docketv1.StatusCount, len(out.Labels))
	for i, l := range out.Labels {
		labels[i] = &docketv1.StatusCount{Status: l.Name, Count: int32(l.IssueCount)}
	}
	return &docketv1.StatsResponse{
		Total:      int32(out.Total),
		ByStatus:   keyOrderedCounts(out.ByStatus),
		ByPriority: keyOrderedCounts(out.ByPriority),
		ByLabel:    labels,
	}
}

func keyOrderedCounts(counts map[string]int) []*docketv1.StatusCount {
	out := make([]*docketv1.StatusCount, 0, len(counts))
	for _, key := range slices.Sorted(maps.Keys(counts)) {
		out = append(out, &docketv1.StatusCount{Status: key, Count: int32(counts[key])})
	}
	return out
}
