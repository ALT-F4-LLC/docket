package filter

import "github.com/ALT-F4-LLC/docket/internal/model"

// ToStringSet converts a slice of strings to a set for O(1) membership checks.
func ToStringSet(ss []string) map[string]struct{} {
	if len(ss) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		set[s] = struct{}{}
	}
	return set
}

// HasAllLabels returns true if the issue has every label in the required set.
func HasAllLabels(issue *model.Issue, required map[string]struct{}) bool {
	have := make(map[string]struct{}, len(issue.Labels))
	for _, l := range issue.Labels {
		have[l] = struct{}{}
	}
	for l := range required {
		if _, ok := have[l]; !ok {
			return false
		}
	}
	return true
}
