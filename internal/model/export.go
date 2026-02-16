package model

// IssueLabelMapping represents a row in the issue_labels join table.
type IssueLabelMapping struct {
	IssueID int `json:"issue_id"`
	LabelID int `json:"label_id"`
}

// IssueFileMapping represents a row in the issue_files join table.
type IssueFileMapping struct {
	IssueID  int    `json:"issue_id"`
	FilePath string `json:"file_path"`
}

// ExportData is the top-level structure for a full database export.
type ExportData struct {
	Version            int                `json:"version"`
	ExportedAt         string             `json:"exported_at"`
	Issues             []*Issue           `json:"issues"`
	Comments           []*Comment         `json:"comments"`
	Relations          []Relation         `json:"relations"`
	Labels             []*Label           `json:"labels"`
	IssueLabelMappings []IssueLabelMapping `json:"issue_label_mappings"`
	IssueFileMappings  []IssueFileMapping  `json:"issue_file_mappings"`
}
