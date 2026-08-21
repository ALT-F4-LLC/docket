package model

// withVersionSlice builds a v2 wrapper slice from entity pointers, dropping
// nils on the way — the one shape WithVersion, RunsWithVersion,
// SchemasWithVersion, and WorkflowsWithVersion each hand-rolled before this
// helper existed. wrap builds the type-specific wrapper (VersionedIssue,
// VersionedRun, VersionedSchema, VersionedWorkflow), which differs by
// wrapper type and embedded field name across the four callers, so it stays
// a per-caller closure rather than something this helper can infer.
func withVersionSlice[E, V any](items []*E, wrap func(*E) V) []V {
	out := make([]V, 0, len(items))
	for _, it := range items {
		if it != nil {
			out = append(out, wrap(it))
		}
	}
	return out
}
