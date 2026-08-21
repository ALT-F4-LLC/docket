package engine

import "github.com/ALT-F4-LLC/docket/internal/workflow"

// artifactSHA256 is the content address an artifact records — over the BODY
// AND the PAYLOAD (DKT-112). For action and aggregate artifacts the payload is
// the content and the body a short count summary, so a hash over the body
// alone gave a supersession chain — one re-emit per held-cluster approval —
// one hash across artifacts whose payloads differ, and every consumer treating
// sha256 as a content address read real change as duplicates.
//
// A NUL joins the halves, so no (body, payload) pair can collide with a
// different split of the same bytes. A payload-less artifact hashes exactly as
// it always did — gap, diff, and body-only artifacts' recorded hashes did not
// move.
func artifactSHA256(body, payload []byte) string {
	if len(payload) == 0 {
		return workflow.SHA256(body)
	}
	joined := make([]byte, 0, len(body)+1+len(payload))
	joined = append(joined, body...)
	joined = append(joined, 0)
	joined = append(joined, payload...)
	return workflow.SHA256(joined)
}
