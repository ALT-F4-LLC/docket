package workflow

// Load is the register-time pipeline in one call: parse (strict), validate
// (V1-V25 incl. V13a), lint (L1-L4). Every caller that accepts workflow source
// goes through it, so no path can register a definition that skipped a rule.
//
// The order is not interchangeable. Validation assumes a decoded definition,
// and the lints assume validation has already established that every `after`
// and `inputs` reference resolves — L1's graph would otherwise be built over
// edges V9 was supposed to reject.
func Load(src []byte) (*Definition, error) {
	def, err := Parse(src)
	if err != nil {
		return nil, err
	}
	if err := Validate(def); err != nil {
		return nil, err
	}
	if err := Lint(def); err != nil {
		return nil, err
	}
	return def, nil
}
