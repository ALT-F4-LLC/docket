package workflow

import (
	"fmt"
	"slices"
)

// A vote step's three authorization switches (DKT-2562) and their closed value
// sets. Each default is the behavior every vote step had before the field
// existed, so a definition that declares none of them tallies exactly as it
// always did.

// Roster: who a cast may be attributed to.
//
// `open` is the default: the step's voter list is COUNTED, and a cast under
// any name fills a seat. `strict` admits only a name on the step's list.
const (
	RosterOpen   = "open"
	RosterStrict = "strict"
)

// Weighting: what one cast is worth in the tally.
//
// `declared` is the default and the existing arithmetic — the caster's own
// confidence times its own domain relevance. `equal` counts every cast the
// same, so a seat cannot price its own testimony.
const (
	WeightingDeclared = "declared"
	WeightingEqual    = "equal"
)

// Recuse: who is excluded from the ballot.
//
// `none` is the default. `executor` refuses a cast from the executor hint of
// the step this vote step `reviews`, so the party whose work is under review
// cannot judge it.
const (
	RecuseNone     = "none"
	RecuseExecutor = "executor"
)

// EffectiveRoster is the step's roster with the default applied. The field
// itself stays empty when undeclared (see Step.Roster), so the pinned form of
// a definition that never declared one is unchanged.
func (s *Step) EffectiveRoster() string {
	if s.Roster == "" {
		return RosterOpen
	}
	return s.Roster
}

// EffectiveWeighting is the step's weighting with the default applied.
func (s *Step) EffectiveWeighting() string {
	if s.Weighting == "" {
		return WeightingDeclared
	}
	return s.Weighting
}

// EffectiveRecuse is the step's recuse with the default applied.
func (s *Step) EffectiveRecuse() string {
	if s.Recuse == "" {
		return RecuseNone
	}
	return s.Recuse
}

// voteSwitches is the register-time table V43 checks: each field, its value
// set, and a reader for the declared value. One table so the three fields
// cannot drift in how they refuse.
var voteSwitches = []struct {
	field   string
	allowed []string
	value   func(*Step) string
}{
	{"roster", []string{RosterOpen, RosterStrict}, func(s *Step) string { return s.Roster }},
	{"weighting", []string{WeightingDeclared, WeightingEqual}, func(s *Step) string { return s.Weighting }},
	{"recuse", []string{RecuseNone, RecuseExecutor}, func(s *Step) string { return s.Recuse }},
}

// validateVoteSwitches is V43: the three switches are valid only on a
// `type="vote"` step, and only at a value in their set. An undeclared field
// is always valid; its effective value is the default.
func validateVoteSwitches(step *Step) error {
	for _, sw := range voteSwitches {
		value := sw.value(step)
		if value == "" {
			continue
		}
		if step.Type != TypeVote {
			return &Error{
				Rule: "V43", Step: step.Name, Field: sw.field,
				Message: fmt.Sprintf(
					"step %q: `%s` is only valid on `type=\"vote\"` steps", step.Name, sw.field),
			}
		}
		if !slices.Contains(sw.allowed, value) {
			return &Error{
				Rule: "V43", Step: step.Name, Field: sw.field,
				Message: fmt.Sprintf(
					"step %q: `%s` must be %s, got %q",
					step.Name, sw.field, quotedList(sw.allowed), value),
			}
		}
	}
	return nil
}
