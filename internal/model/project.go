package model

// Project is one repository's tenancy in a shared store — the v12 dimension
// that partitions issues, docs, proposals, labels, workflows, schemas, and
// runs. Identity is the canonical project path config.Resolve derives; it is
// the ONLY key resolution ever matches on. Name is display-only. Prefix is
// reserved for per-project display prefixes; issue identity stays the global
// DKT-N sequence so an id names one issue machine-wide.
type Project struct {
	ID          int    `json:"id"`
	Identity    string `json:"identity"`
	Name        string `json:"name"`
	Prefix      string `json:"prefix"`
	CreatedAtMS int64  `json:"created_at_ms"`
}
