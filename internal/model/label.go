package model

// Label represents a label that can be attached to an issue.
type Label struct {
	ID int `json:"id"`
	// ProjectID is the v12 tenancy dimension; zero normalizes to the default
	// project on write. Excluded from JSON: the v1 export shape is frozen.
	ProjectID int    `json:"-"`
	Name      string `json:"name"`
	Color     string `json:"color,omitempty"`
}

// LabelWithCount extends Label with the number of issues using it.
type LabelWithCount struct {
	Label
	IssueCount int `json:"issue_count"`
}
