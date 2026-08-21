package render

import (
	"fmt"
	"strings"
)

// Artifact rendering — human mode for `step artifacts` and `step artifact`.
//
// Same genericity bar as the rest of this package: every column names a CORE
// concept — a reference, a kind, a producing step, a hash, a size. Nothing
// here names an instance concept.

// StepArtifactRow is one row of the `step artifacts` listing.
//
// Mirrors engine.StepArtifact's reported fields rather than importing it, so
// this package stays independent of engine: the CLI converts, which is the
// direction every other renderer here takes its input.
type StepArtifactRow struct {
	Artifact     string
	Kind         string
	Bytes        int
	PayloadBytes int
	Stub         bool
	SHA256       string
}

// RenderStepArtifacts renders the `step artifacts` listing.
//
// Sizes, never bodies: an artifact may be up to 1MiB, and a listing that
// inlined them would be unreadable exactly when it matters most. The empty
// state says a step producing nothing is ordinary, so an operator does not
// read it as a failure.
func RenderStepArtifacts(step string, rows []StepArtifactRow) string {
	if len(rows) == 0 {
		return EmptyState(
			fmt.Sprintf("%s produced no artifacts.", step),
			"Many steps produce none; this is not a failure. "+
				"To see the step itself: docket step show "+step,
			false,
		)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%-12s %-22s %10s %10s  %s\n",
		"ARTIFACT", "KIND", "BYTES", "PAYLOAD", "SHA256")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 76))

	for _, r := range rows {
		payload := "-"
		if r.PayloadBytes > 0 {
			payload = fmt.Sprintf("%d", r.PayloadBytes)
		}
		kind := r.Kind
		if r.Stub {
			// A stub artifact records that a computation did NOT run. Marking
			// it in the listing keeps an operator from reading it as a result.
			kind += " (stub)"
		}
		fmt.Fprintf(&b, "%-12s %-22s %10d %10s  %s\n",
			r.Artifact, truncate(kind, 22), r.Bytes, payload, shortHash(r.SHA256))
	}

	fmt.Fprintf(&b, "\nRead one in full: docket step artifact %s\n", rows[0].Artifact)

	return b.String()
}

// RenderArtifact renders one artifact in full for `step artifact`.
//
// The BODY IS PRINTED VERBATIM and last, with no wrapping or indentation, so
// it can be redirected to a file or piped without the renderer having altered
// the bytes. The metadata goes above it, where it does not interfere.
func RenderArtifact(reference, kind, producer, sha256, body, payload string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s  %s\n", reference, kind)
	if producer != "" {
		fmt.Fprintf(&b, "  producer:  %s\n", producer)
	}
	fmt.Fprintf(&b, "  sha256:    %s\n", sha256)
	fmt.Fprintf(&b, "  bytes:     %d\n", len(body))

	if payload != "" {
		fmt.Fprintf(&b, "\n--- payload ---\n%s\n", strings.TrimRight(payload, "\n"))
	}

	if body != "" {
		fmt.Fprintf(&b, "\n--- body ---\n%s", body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
	}

	return b.String()
}

// shortHash trims a hash for a table column while leaving it recognizable.
// The full value is in the JSON envelope and in `step artifact`'s own output,
// so nothing is lost by shortening it here.
func shortHash(sha string) string {
	const shown = 12
	if len(sha) <= shown {
		return sha
	}
	return sha[:shown]
}
