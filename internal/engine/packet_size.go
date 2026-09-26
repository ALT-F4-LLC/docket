package engine

// packetContextSizer counts raw pinned file bytes and ref labels using the
// same file selection as rendering. Includes are cached across issues during
// one activation; malformed or missing includes still fail at render time.
func packetContextSizer(pins map[string]int, roots []string) func([]string) int {
	includes := make(map[string][]string)
	return func(entries []string) int {
		size := 0
		// This visitor cannot fail: activation uses the scan's byte counts and
		// best-effort include metadata, leaving strict validation to rendering.
		_ = walkPacketFiles(entries, func(ref string) ([]string, error) {
			bytes, pinned := pins[ref]
			if !pinned {
				return nil, nil
			}
			size += len(ref) + bytes
			if bytes == 0 {
				// Inherited pins have unknown size. Do not discover includes
				// from current bytes that may differ from their frozen hash.
				return nil, nil
			}
			refs, cached := includes[ref]
			if !cached {
				refs = packetIncludesOf(roots, ref)
				includes[ref] = refs
			}
			return refs, nil
		})
		return size
	}
}
