package db

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The shared store's core promise (v12): two projects in one database never
// see each other's rows, and the config ladder resolves project-over-global.

func TestProjectsIsolateIssueLists(t *testing.T) {
	conn := mustOpen(t)
	if err := Initialize(conn); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(conn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	one, _ := EnsureProject(conn, "/repo/one", "one", 1)
	two, _ := EnsureProject(conn, "/repo/two", "two", 2)

	mkIssue := func(pid int, title string) int {
		id, err := CreateIssue(conn, &model.Issue{
			ProjectID: pid, Title: title,
			Status: model.StatusBacklog, Priority: model.PriorityNone,
			Kind: model.IssueKindTask,
		}, []string{"bug"}, nil)
		if err != nil {
			t.Fatalf("CreateIssue(%s): %v", title, err)
		}
		return id
	}
	a := mkIssue(one, "in one")
	b := mkIssue(two, "in two")

	listTitles := func(pid int) []string {
		issues, _, err := ListIssues(conn, ListOptions{ProjectID: pid})
		if err != nil {
			t.Fatalf("ListIssues(%d): %v", pid, err)
		}
		out := make([]string, 0, len(issues))
		for _, i := range issues {
			out = append(out, i.Title)
		}
		return out
	}

	if got := listTitles(one); len(got) != 1 || got[0] != "in one" {
		t.Errorf("project one lists %v, want [in one]", got)
	}
	if got := listTitles(two); len(got) != 1 || got[0] != "in two" {
		t.Errorf("project two lists %v, want [in two]", got)
	}

	// The two `bug` labels are DIFFERENT rows — per-project namespaces.
	var labels int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM labels WHERE name = 'bug'`).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if labels != 2 {
		t.Errorf("%d 'bug' label rows, want 2 (one per project)", labels)
	}

	// Ids remain globally unique — a display id names one issue machine-wide.
	if a == b {
		t.Errorf("issue ids collide across projects (%d)", a)
	}

	// By-id reads stay global: naming another project's issue still resolves.
	got, err := GetIssue(conn, b)
	if err != nil || got.ProjectID != two {
		t.Errorf("GetIssue across projects = (%+v, %v), want project %d", got, err, two)
	}
}

func TestProjectConfigOverridesGlobal(t *testing.T) {
	conn := mustOpen(t)
	if err := Initialize(conn); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(conn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	one, _ := EnsureProject(conn, "/repo/one", "one", 1)
	two, _ := EnsureProject(conn, "/repo/two", "two", 2)

	// A store-wide default, exactly what a legacy DB's existing value is.
	if err := SetConfig(conn, 0, KeyLeaseTTLDefault, "10m"); err != nil {
		t.Fatalf("SetConfig global: %v", err)
	}
	// One project overrides it.
	if err := SetConfig(conn, one, KeyLeaseTTLDefault, "2m"); err != nil {
		t.Fatalf("SetConfig project: %v", err)
	}

	entry, err := GetConfig(conn, one, KeyLeaseTTLDefault)
	if err != nil || entry.Value != "2m" {
		t.Errorf("project one reads %q (%v), want its own 2m", entry.Value, err)
	}
	entry, err = GetConfig(conn, two, KeyLeaseTTLDefault)
	if err != nil || entry.Value != "10m" {
		t.Errorf("project two reads %q (%v), want the store-wide 10m", entry.Value, err)
	}
}
