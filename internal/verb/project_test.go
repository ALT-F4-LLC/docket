package verb_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// requireVerbCode asserts err is a *verb.Error carrying code and that its text
// contains want.
func requireVerbCode(t *testing.T, err error, code output.ErrorCode, want string) {
	t.Helper()
	var ve *verb.Error
	if !errors.As(err, &ve) || ve.Code != code {
		t.Fatalf("error = %v, want a *verb.Error with %s", err, code)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error text %q does not contain %q", err.Error(), want)
	}
}

func ensureProject(t *testing.T, st store.Store, identity, name string) int {
	t.Helper()
	id, err := st.EnsureProject(identity, name, 1)
	if err != nil {
		t.Fatalf("EnsureProject(%s): %v", identity, err)
	}
	return id
}

func TestProjectListMarksTheCallersProject(t *testing.T) {
	st := openStore(t)
	first := ensureProject(t, st, "/repos/first", "first")
	second := ensureProject(t, st, "/repos/second", "second")

	got, err := verb.ProjectList(context.Background(), projectEnv(st, "/repos/second", false), verb.ProjectListInput{})
	if err != nil {
		t.Fatalf("ProjectList: %v", err)
	}
	if got.Current != second {
		t.Errorf("Current = %d, want the caller's project %d", got.Current, second)
	}
	ids := map[int]bool{}
	for _, p := range got.Projects {
		ids[p.ID] = true
	}
	if !ids[first] || !ids[second] {
		t.Errorf("Projects = %+v, want both %d and %d", got.Projects, first, second)
	}
}

func TestProjectListNeverRegistersAProject(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")

	got, err := verb.ProjectList(context.Background(), projectEnv(st, "/repos/unseen", true), verb.ProjectListInput{})
	if err != nil {
		t.Fatalf("ProjectList: %v", err)
	}
	if got.Current != store.UnregisteredProjectID {
		t.Errorf("Current = %d, want the unregistered sentinel %d", got.Current, store.UnregisteredProjectID)
	}
	requireUnregistered(t, st, "/repos/unseen")
}

func TestProjectSetPrefixNormalizesAndSetsTheCallersPrefix(t *testing.T) {
	st := openStore(t)
	id := ensureProject(t, st, "/repos/mine", "mine")

	got, err := verb.ProjectSetPrefix(context.Background(), projectEnv(st, "/repos/mine", true),
		verb.ProjectSetPrefixInput{Prefix: "  abc "})
	if err != nil {
		t.Fatalf("ProjectSetPrefix: %v", err)
	}
	if got.Project.ID != id || got.Project.Prefix != "ABC" {
		t.Errorf("Project = %+v, want project %d with prefix ABC", got.Project, id)
	}
	if got.Registered != nil {
		t.Errorf("Registered = %+v, want nil for an already registered project", got.Registered)
	}
	stored, err := st.ProjectPrefix(id)
	if err != nil || stored != "ABC" {
		t.Errorf("stored prefix = (%q, %v), want ABC", stored, err)
	}
}

func TestProjectSetPrefixRefusesAnInvalidPrefix(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/mine", "mine")

	_, err := verb.ProjectSetPrefix(context.Background(), projectEnv(st, "/repos/mine", true),
		verb.ProjectSetPrefixInput{Prefix: "RUN"})
	var ve *verb.Error
	if !errors.As(err, &ve) || ve.Code != output.ErrValidation {
		t.Fatalf("error = %v, want a *verb.Error with %s", err, output.ErrValidation)
	}
}

func TestProjectSetPrefixRefusesAPrefixAnotherProjectHolds(t *testing.T) {
	st := openStore(t)
	other := ensureProject(t, st, "/repos/other", "other")
	if err := st.SetProjectPrefix(other, "TAKEN"); err != nil {
		t.Fatalf("SetProjectPrefix: %v", err)
	}
	mine := ensureProject(t, st, "/repos/mine", "mine")
	before, err := st.ProjectPrefix(mine)
	if err != nil {
		t.Fatalf("ProjectPrefix: %v", err)
	}

	_, err = verb.ProjectSetPrefix(context.Background(), projectEnv(st, "/repos/mine", true),
		verb.ProjectSetPrefixInput{Prefix: "taken"})
	requireVerbCode(t, err, output.ErrConflict,
		`prefix TAKEN is already held by "other" (/repos/other)`)
	if after, _ := st.ProjectPrefix(mine); after != before {
		t.Errorf("prefix changed from %q to %q despite the refusal", before, after)
	}
}

func TestProjectSetPrefixRefusesAnUnanchoredUnregisteredCaller(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")

	_, err := verb.ProjectSetPrefix(context.Background(), projectEnv(st, "/work/scratch", false),
		verb.ProjectSetPrefixInput{Prefix: "ABC"})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	requireUnregistered(t, st, "/work/scratch")
}

func TestProjectSetPrefixReportsTheProjectItRegistered(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")

	got, err := verb.ProjectSetPrefix(context.Background(), projectEnv(st, "/repos/fresh", true),
		verb.ProjectSetPrefixInput{Prefix: "NEW"})
	if err != nil {
		t.Fatalf("ProjectSetPrefix: %v", err)
	}
	id, found, err := st.LookupProject("/repos/fresh")
	if err != nil || !found {
		t.Fatalf("LookupProject = (%d, %v, %v), want a registered row", id, found, err)
	}
	reg := got.Registered
	if reg == nil || reg.ID != id || reg.Identity != "/repos/fresh" || reg.Name != "fresh" {
		t.Fatalf("Registered = %+v, want the new row %d for /repos/new", reg, id)
	}
	if reg.Prefix == "" || reg.Prefix == "NEW" {
		t.Errorf("Registered.Prefix = %q, want the prefix the row was registered with, before the set", reg.Prefix)
	}
	if got.Project.Prefix != "NEW" {
		t.Errorf("Project.Prefix = %q, want NEW", got.Project.Prefix)
	}
}

func TestProjectDeleteRemovesAnEmptyProjectByAnyKey(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")
	junk := ensureProject(t, st, "/repos/junk", "junk")

	got, err := verb.ProjectDelete(context.Background(), projectEnv(st, "/repos/first", true),
		verb.ProjectDeleteInput{Project: strconv.Itoa(junk)})
	if err != nil {
		t.Fatalf("ProjectDelete: %v", err)
	}
	if got.Project.ID != junk || got.Project.Identity != "/repos/junk" {
		t.Errorf("Project = %+v, want the deleted row %d", got.Project, junk)
	}
	requireUnregistered(t, st, "/repos/junk")
}

func TestProjectDeleteRefusesTheDefaultProject(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")

	_, err := verb.ProjectDelete(context.Background(), projectEnv(st, "/repos/first", true),
		verb.ProjectDeleteInput{Project: "first"})
	requireVerbCode(t, err, output.ErrValidation, `project "first" is the default project and cannot be deleted`)
}

func TestProjectDeleteRefusesAProjectThatHoldsRows(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")
	busy := ensureProject(t, st, "/repos/busy", "busy")
	if _, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: busy,
		Title:     "kept",
		Status:    model.StatusTodo,
		Priority:  model.PriorityMedium,
		Kind:      model.IssueKindTask,
	}, nil, nil, ""); err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}

	_, err := verb.ProjectDelete(context.Background(), projectEnv(st, "/repos/first", true),
		verb.ProjectDeleteInput{Project: "/repos/busy"})
	requireVerbCode(t, err, output.ErrConflict, `project "busy" still holds 1 issue: re-home them`)
	if _, found, _ := st.LookupProject("/repos/busy"); !found {
		t.Error("the refused project was deleted")
	}
}

func TestProjectDeleteReportsAnUnknownProjectAsNotFound(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")

	_, err := verb.ProjectDelete(context.Background(), projectEnv(st, "/repos/first", true),
		verb.ProjectDeleteInput{Project: "nowhere"})
	requireVerbCode(t, err, output.ErrNotFound, `no project named "nowhere"`)
}

func TestProjectDeleteRefusesAnUnanchoredUnregisteredCaller(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")
	junk := ensureProject(t, st, "/repos/junk", "junk")

	_, err := verb.ProjectDelete(context.Background(), projectEnv(st, "/work/scratch", false),
		verb.ProjectDeleteInput{Project: strconv.Itoa(junk)})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	if _, found, _ := st.LookupProject("/repos/junk"); !found {
		t.Error("the project was deleted although the caller's project was refused")
	}
}
