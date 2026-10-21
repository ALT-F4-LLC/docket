package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4275: `run activate --dry-run` lists bound issues that lack the security
// label yet look security-shaped.

// routingRun binds a security-labelled sibling, a DKT-4053-like issue with an
// overlapping scope, a lexicon-only issue, and a plainly unrelated one.
func routingRun(t *testing.T, conn *sql.DB) (run *model.Run, sibling, overlapping, lexical, unrelated int) {
	t.Helper()
	registerFixture(t, conn)
	scoped := func(title, body string, labels []string, scope string) int {
		id := createIssue(t, conn, title, body, "task", labels)
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, id, scope), "declaring scope")
		return id
	}
	sibling = scoped("harden the exec resolver", "path handling", []string{"security"}, `["internal/exec/**"]`)
	overlapping = scoped("Resolve gates against the exec root", "trust boundary for gate argv", nil, `["internal/exec/resolve.go"]`)
	lexical = scoped("Rotate the bearer credential", "the DSN carries a bearer token", nil, `["cmd/serve/**"]`)
	unrelated = scoped("Tidy the README", "fix typos", nil, `["docs/README.md"]`)
	return startRun(t, conn, sibling, overlapping, lexical, unrelated), sibling, overlapping, lexical, unrelated
}

func dryRunWarnings(t *testing.T, conn *sql.DB, runID int) map[string]RoutingWarning {
	t.Helper()
	result, err := Activate(conn, runID, ActivateOptions{NowMS: nowMS, DryRun: true})
	testsupport.Must(t, err, "dry-run activate: %v", err)
	out := map[string]RoutingWarning{}
	for _, w := range result.RoutingWarnings {
		out[w.Issue] = w
	}
	return out
}

func TestDryRunFlagsAScopeOverlapWithASecurityLabelledIssue(t *testing.T) {
	conn := mustDB(t)
	run, sibling, overlapping, lexical, unrelated := routingRun(t, conn)

	got := dryRunWarnings(t, conn, run.ID)

	w, ok := got[model.FormatID(overlapping)]
	if !ok || len(w.Overlaps) != 1 || w.Overlaps[0] != model.FormatID(sibling) {
		t.Fatalf("overlapping issue not flagged against its security sibling: %+v", got)
	}
	if !strings.Contains(w.Reason, "no security label") {
		t.Errorf("reason %q does not say the label is missing", w.Reason)
	}
	for id, why := range map[int]string{
		sibling:   "carries the security label",
		lexical:   "matches no lexicon: none is configured",
		unrelated: "plainly unrelated",
	} {
		if _, flagged := got[model.FormatID(id)]; flagged {
			t.Errorf("%s was flagged but is %s", model.FormatID(id), why)
		}
	}
}

func TestDryRunFlagsALexiconMatchOnlyOnceConfigured(t *testing.T) {
	conn := mustDB(t)
	run, _, overlapping, lexical, unrelated := routingRun(t, conn)
	testsupport.Must(t, db.SetConfig(conn, 1, db.KeyRoutingSecurityLexicon, "bearer,dsn,trust-boundary"), "setting the lexicon")

	got := dryRunWarnings(t, conn, run.ID)

	if w := got[model.FormatID(lexical)]; len(w.Terms) != 2 || w.Terms[0] != "bearer" || w.Terms[1] != "dsn" {
		t.Errorf("lexical issue terms = %+v; want bearer and dsn", w)
	}
	if w := got[model.FormatID(overlapping)]; len(w.Terms) != 1 || w.Terms[0] != "trust-boundary" {
		t.Errorf("trust-boundary did not match 'trust boundary' in prose: %+v", w)
	}
	if _, flagged := got[model.FormatID(unrelated)]; flagged {
		t.Error("the plainly unrelated issue was flagged")
	}
}

func TestRoutingLintIsOffWithAnEmptyLabelAndWritesNothingOnARealActivation(t *testing.T) {
	conn := mustDB(t)
	run, _, _, _, _ := routingRun(t, conn)
	testsupport.Must(t, db.SetConfig(conn, 1, db.KeyRoutingSecurityLabel, ""), "clearing the label")
	if got := dryRunWarnings(t, conn, run.ID); len(got) != 0 {
		t.Errorf("lint ran with an empty label: %+v", got)
	}

	conn2 := mustDB(t)
	run2, _, _, _, _ := routingRun(t, conn2)
	result, err := Activate(conn2, run2.ID, ActivateOptions{NowMS: nowMS})
	testsupport.Must(t, err, "activate: %v", err)
	if len(result.RoutingWarnings) != 0 {
		t.Errorf("a real activation reported routing warnings: %+v", result.RoutingWarnings)
	}
}
