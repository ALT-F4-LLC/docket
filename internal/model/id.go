package model

import (
	"fmt"
	"strconv"
	"strings"
)

// parseEntityID trims input, strips the first prefix in cutPrefixes that
// matches (checked in order, case-insensitively; each prefix must already
// carry the separator it expects to cut, e.g. "DOC-"), and parses what
// remains as a positive integer. label names the entity in error messages
// ("issue", "doc", "proposal").
//
// Shared by ParseID, ParseDocID, and ParseProposalID, whose error shape —
// wrap strconv's own error, then a separate "must be positive" check — is
// identical across all three. ParseRunID and ParseStepID fold both failure
// modes into a single "want PREFIX-N or N" message instead, a genuinely
// different error shape, so they share parseRefID below rather than this
// helper.
func parseEntityID(input, label string, cutPrefixes ...string) (int, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, fmt.Errorf("empty %s ID", label)
	}

	upper := strings.ToUpper(s)
	for _, prefix := range cutPrefixes {
		if strings.HasPrefix(upper, prefix) {
			s = s[len(prefix):]
			break
		}
	}

	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s ID %q: %w", label, input, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid %s ID %q: must be positive", label, input)
	}

	return id, nil
}

// parseRefID trims s, strips a leading "PREFIX-" (case-insensitive) if
// present, and parses the remainder as a positive integer, folding a
// missing, non-numeric, or non-positive value into one "want PREFIX-N or N"
// message. Shared by ParseRunID and ParseStepID.
func parseRefID(s, prefix, label string) (int, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("empty %s ID", label)
	}

	digits := trimmed
	if rest, ok := strings.CutPrefix(strings.ToUpper(trimmed), prefix+"-"); ok {
		digits = rest
	}

	id, err := strconv.Atoi(digits)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid %s ID %q: want %s-N or N", label, s, prefix)
	}
	return id, nil
}
