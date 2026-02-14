package model

// Label represents a label that can be attached to an issue.
type Label struct {
	ID    int
	Name  string
	Color string // optional, for terminal rendering
}
