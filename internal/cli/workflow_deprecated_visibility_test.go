package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	testsupport.Must(t, err, "marshal: %v", err)
	return string(out)
}

// DKT-82: binding eligibility is auditable from `workflow list` output. The
// human render marks retired versions; the v2 JSON carries the fact as
// `deprecated_at_ms`. v1 is frozen and never gains the field.
func TestWorkflowListExposesDeprecation(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, registerSource(t, conn, minimalWorkflow), "register")
	_, err := db.DeprecateWorkflow(conn, 1, "unit", 1, model.NowMS())
	testsupport.Must(t, err, "deprecate: %v", err)

	workflows, _, err := db.ListWorkflows(conn, db.WorkflowListOptions{})
	testsupport.Must(t, err, "list: %v", err)
	if len(workflows) != 1 {
		t.Fatalf("listed %d workflows, want 1", len(workflows))
	}

	// Human render: the retired version is marked.
	if got := renderWorkflowList(workflows); !strings.Contains(got, "[deprecated]") {
		t.Errorf("human render %q does not mark the retired version", got)
	}

	// v2 items carry the fact; v1 stays frozen without it.
	v2 := mustMarshal(t, model.WorkflowsWithVersion(workflows))
	if !strings.Contains(v2, `"deprecated_at_ms"`) {
		t.Errorf("v2 item %q does not carry deprecated_at_ms (DKT-82)", v2)
	}
	v1 := mustMarshal(t, workflows)
	if strings.Contains(v1, "deprecated_at_ms") {
		t.Errorf("v1 item %q gained a field; v1 is frozen", v1)
	}

	// A version that still binds omits the field: not a fact, not a key.
	testsupport.Must(t, registerSource(t, conn,
		strings.Replace(minimalWorkflow, "version = 1", "version = 2", 1)),
		"register v2")
	workflows, _, err = db.ListWorkflows(conn, db.WorkflowListOptions{})
	testsupport.Must(t, err, "list: %v", err)
	for _, wf := range workflows {
		if wf.Version != 2 {
			continue
		}
		item := mustMarshal(t, model.WorkflowsWithVersion([]*model.Workflow{wf}))
		if strings.Contains(item, "deprecated_at_ms") {
			t.Errorf("a binding version carries deprecated_at_ms: %q", item)
		}
	}
}
