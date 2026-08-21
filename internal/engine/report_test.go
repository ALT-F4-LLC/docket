package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `run report` — R7's genericity line, R8's zero writes, R9's determinism, and
// R10's any-status rule (TDD §4.10, §10.3).

// TestReportIsDeterministic is R9: the same rows produce the SAME DOCUMENT,
// byte for byte.
//
// Every section is ordered by a total key — id, then name, then value — never by
// map iteration, for the same golden-stability reason `referencedSchemas` is
// ordered. This is not a style preference: R7's metadata rollup and R3's status
// counts both build Go maps on their way to a list, and a renderer that ranged
// either would emit a different document on every invocation. An operator
// diffing two reports of an unchanged run would see noise, and a golden test
// over the report would flap.
func TestReportIsDeterministic(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 100)
	e := testEngine()

	// A run with something in every section that group 1 can produce: claims
	// (so there are attempts and a floor), a completion carrying usage in TWO
	// units (so the per-unit rollup has more than one row to order), and the
	// fixture's own metadata on its steps.
	completeWithUsage(t, conn, e, "implement@0", `{"sheets": 2, "pages": 5}`)

	first, err := LoadRunReport(conn, runID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)
	for i := range 8 {
		next, err := LoadRunReport(conn, runID, nowMS)
		testsupport.Must(t, err, "LoadRunReport (pass %d): %v", i, err)
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("pass %d differs from the first; every section must order "+
				"by a total key rather than by map iteration\nfirst: %s\nnext:  %s",
				i, mustJSON(t, first), mustJSON(t, next))
		}
	}

	// And the ordering is the one R9 names, not merely a stable one: units
	// ascend by name.
	units := first.Budget.Reported
	if len(units) != 2 {
		t.Fatalf("the report rolled up %d units, want 2", len(units))
	}
	if units[0].Unit != "pages" || units[1].Unit != "sheets" {
		t.Errorf("units ordered %s, %s; want them ascending by name",
			units[0].Unit, units[1].Unit)
	}
}

// TestReadVerbsWriteNothing is R8 and §10.3's standing assertion, for group 1's
// rows: `run report` leaves the database BYTE-IDENTICAL.
//
// It hashes the file rather than counting rows, because a row count would miss
// exactly the writes that matter — a reaped lease, a bumped attempt, a
// refreshed `usage_floor` — all of which mutate rows in place. Page-level
// content is the only check that catches an in-place update.
//
// Group 2 extends this test with `dispatch verify`, and group 3 with
// `events list` and both guards.
func TestReadVerbsWriteNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "issues.db")
	conn, err := sql.Open("sqlite", path)
	testsupport.Must(t, err, "opening: %v", err)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)

	runID, _ := budgetRun(t, conn, 100)
	e := testEngine()
	completeWithUsage(t, conn, e, "implement@0", `{"pages": 5}`)

	// A step with a LAPSED lease, which is the write a read verb is most likely
	// to make by accident: `next` would reap it, and a report that shared that
	// code path would too.
	claimInstance(t, conn, "review@0#0", nowMS)
	execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE instance = 'review@0#0'`)

	// Checkpoint the WAL so the file on disk holds everything, then hash it.
	_, err = conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	testsupport.Must(t, err, "checkpointing: %v", err)
	before := hashFile(t, path)

	report, err := LoadRunReport(conn, runID, nowMS+1)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	_, err = conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	testsupport.Must(t, err, "checkpointing after the read: %v", err)
	if after := hashFile(t, path); after != before {
		t.Error("`run report` changed the database; it computes effective status " +
			"at read and must write nothing — not even the reap `next` would do")
	}

	// And it DID report the lapsed lease at its effective status, so the zero
	// writes are not the zero of a verb that read nothing.
	var sawEffective bool
	for _, a := range report.Attempts {
		if a.Instance == "review@0#0" && a.Status != db.StepClaimed {
			sawEffective = true
		}
	}
	if !sawEffective {
		t.Error("the report rendered a lapsed claim as still claimed; effective " +
			"status is computed at read (engine-spec §2)")
	}
}

// TestMetadataRollupReadsNoKey is R7, mechanically: the implementation contains
// NO KEY-NAME LITERAL.
//
// R7 is the genericity line at its thinnest. The rollup groups by key and by
// value, both as opaque strings, and reports counts — it reports
// `{"tier": {"a": 3}}` for exactly the same reason it would report
// `{"desk": {"front": 3}}`. A rollup that special-cased one key would be core
// having an opinion about what a workflow author's bag of strings means, and the
// vocabulary that would arrive through such a key is precisely what
// docs/design/genericity.md exists to keep out of core.
//
// The check parses the source rather than grepping it, so a key name hidden in a
// switch, a map literal, or a comparison is caught the same as one in an if.
func TestMetadataRollupReadsNoKey(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "db", "rollups.go"))
	testsupport.Must(t, err, "reading the rollup source: %v", err)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "rollups.go", src, 0)
	testsupport.Must(t, err, "parsing the rollup source: %v", err)

	// The vocabulary a key-name special case would introduce. It is stated as
	// the SHAPE of the leak rather than as a list of forbidden words: any string
	// literal inside MetadataRollup that is not SQL or an error message is a
	// candidate for being a key name, and there should be none.
	var offenders []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "MetadataRollup" {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			lit, ok := inner.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value := lit.Value
			// SQL and error text are the two legitimate literal classes in a
			// reader function. Neither can be a metadata key: one names columns
			// this schema defines, the other is a sentence.
			if strings.Contains(value, "SELECT") || strings.Contains(value, " ") {
				return true
			}
			offenders = append(offenders, value)
			return true
		})
		return false
	})

	if len(offenders) > 0 {
		t.Errorf("MetadataRollup contains bare string literals %v; a metadata key "+
			"named in core would be core deciding what a workflow author's opaque "+
			"bag means (R7, docs/design/genericity.md)", offenders)
	}
}

// TestMetadataRollupIsVerbatimAndUninterpreted is R7's behavioral half: two
// unrelated vocabularies roll up identically, because the rollup does not know
// either of them.
func TestMetadataRollupIsVerbatimAndUninterpreted(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	steps := runSteps(t, conn, runID)
	if len(steps) < 4 {
		t.Fatalf("the fixture expanded %d steps; this case needs four", len(steps))
	}
	// One vocabulary an instance might use, and one nobody would — the rollup
	// must not be able to tell them apart.
	for i, bag := range []string{
		`{"desk": "front"}`, `{"desk": "front"}`,
		`{"desk": "back"}`, `{"sirens": 3}`,
	} {
		execSQL(t, conn, `UPDATE steps SET metadata = ? WHERE id = ?`, bag, steps[i].ID)
	}

	rollup, err := db.MetadataRollup(conn, runID)
	testsupport.Must(t, err, "MetadataRollup: %v", err)

	want := []db.MetadataKeyRollup{
		{Key: "desk", Values: []db.MetadataValueCount{
			{Value: "back", Count: 1}, {Value: "front", Count: 2},
		}},
		{Key: "sirens", Values: []db.MetadataValueCount{{Value: "3", Count: 1}}},
	}
	if !reflect.DeepEqual(rollup, want) {
		t.Errorf("rollup = %s, want %s", mustJSON(t, rollup), mustJSON(t, want))
	}
}

// TestReportWorksOnAnyRunStatus is R10: a report refusing on a non-terminal run
// would be useless during exactly the run an operator wants to inspect.
func TestReportWorksOnAnyRunStatus(t *testing.T) {
	for _, status := range []model.RunStatus{
		model.RunPlanning, model.RunActive, model.RunWaitingHuman,
		model.RunDone, model.RunAbandoned,
	} {
		t.Run(string(status), func(t *testing.T) {
			conn := mustDB(t)
			runID, _ := budgetRun(t, conn, 0)
			execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
				string(status), runID)

			report, err := LoadRunReport(conn, runID, nowMS)
			testsupport.Must(t, err, "LoadRunReport on a %s run: %v", status, err)
			if report.Run.Status != status {
				t.Errorf("report says %s, want %s", report.Run.Status, status)
			}
		})
	}
}

// TestReportOnAPlanningRunIsAllZeros is R10's first row, stated separately
// because "everything zero" is the specific claim: a run that has not activated
// has no steps, no floor, and no wall clock — and reports none rather than
// reporting 0 as though it had measured one.
func TestReportOnAPlanningRunIsAllZeros(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "not yet activated", "a body", "task", nil)
	run := startRun(t, conn, issue)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport on a planning run: %v", err)

	if report.WallClockMS != 0 {
		t.Errorf("wall clock = %d on a run that never activated; the question has "+
			"no answer and 0 would read as 'instantly'", report.WallClockMS)
	}
	if report.Budget.Floor != 0 || report.Budget.Spend != 0 {
		t.Errorf("floor %g / spend %g on a planning run, want zeros",
			report.Budget.Floor, report.Budget.Spend)
	}
	if len(report.Steps) != 0 {
		t.Errorf("a planning run reported %d step statuses, want none", len(report.Steps))
	}
	if report.Budget.BurnRate != 0 {
		t.Errorf("burn rate = %g with no elapsed time; publishing an infinity "+
			"would assert something about a division that could not be performed",
			report.Budget.BurnRate)
	}
}

// TestReportFloorAgreesWithEnforcement is the property that keeps the report
// honest: the number it publishes is the number a breach was attributed to.
//
// It recomputes the floor through the SAME exported query enforcement runs, and
// never reads `runs.usage_floor`. A report that read the cache would be
// publishing a number no decision was made against — and the whole point of §9
// item 2's audit is that a breach is attributable to the events that caused it.
func TestReportFloorAgreesWithEnforcement(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 100)

	claimInstance(t, conn, "implement@0", nowMS)

	// Poison the cache. The report must ignore it, exactly as enforcement does.
	execSQL(t, conn, `UPDATE runs SET usage_floor = 999 WHERE id = ?`, runID)

	report, err := LoadRunReport(conn, runID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)
	want := runFloor(t, conn, runID)
	if report.Budget.Floor != want {
		t.Errorf("the report published a floor of %g against an enforced floor of "+
			"%g; it read the cache", report.Budget.Floor, want)
	}
}

// TestReportArtifactIndexCarriesNoBodies is R6: the index names what was
// produced and how big it is, NEVER the bytes.
//
// A rollup that inlined bodies would turn a status check into a document dump —
// and the bodies are one `step context` away for anyone who wants them.
func TestReportArtifactIndexCarriesNoBodies(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	e := testEngine()

	const body = "the-artifact-body-nobody-should-see-in-a-rollup"
	claimAndComplete(t, conn, e, "implement@0", body, "")

	report, err := LoadRunReport(conn, runID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)
	if len(report.Artifacts) == 0 {
		t.Fatal("the report indexed no artifact after a completion")
	}

	encoded := mustJSON(t, report)
	if strings.Contains(encoded, body) {
		t.Error("the report carries an artifact BODY; R6 is an index — id, kind, " +
			"producer, sha256, bytes")
	}
	entry := report.Artifacts[0]
	if entry.Bytes != len(body) {
		t.Errorf("indexed %d bytes, want %d", entry.Bytes, len(body))
	}
	if entry.SHA256 == "" || entry.Producer == "" || entry.Kind == "" {
		t.Errorf("the index row is incomplete: %+v", entry)
	}
}

// hashFile hashes a file's bytes — the page-level content check R8 needs.
func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	testsupport.Must(t, err, "reading %s: %v", path, err)
	sum := sha256.Sum256(data)
	return string(sum[:])
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	testsupport.Must(t, err, "encoding: %v", err)
	return string(out)
}

// TestRunReportRollsUpVoteMetadata pins DKT-71's read half: the metadata
// claims cast votes carry — model, effort, spend, whatever a seat asserted —
// surface in the run report as their own rollup, opaque keys to counted
// values, exactly as step metadata does. Vote seats are the one spend the
// usage ledger cannot attribute (a vote step is never claimed), so before
// this section existed, tribunal routing and cost were unverifiable from the
// run document: the v2 re-ratification had to size seats on vote OUTCOMES
// alone.
func TestRunReportRollsUpVoteMetadata(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "report-panel", "seat-a,seat-b")
	driveToReconcile(t, conn, e, clusteredPayload)
	nextRun(t, conn, e)

	held := heldInstances(t, conn)
	if len(held) == 0 {
		t.Fatal("nothing held")
	}
	proposalID := heldProposalID(t, conn, e, held[0])
	for _, seat := range []string{"seat-a", "seat-b"} {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID:      proposalID,
			VoterName:       seat,
			Verdict:         model.VerdictApprove,
			Confidence:      0.9,
			DomainRelevance: 0.8,
			Metadata:        map[string]any{"routed_to": "seat-target-x"},
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	var rollup *db.MetadataKeyRollup
	for i := range report.VoteMetadata {
		if report.VoteMetadata[i].Key == "routed_to" {
			rollup = &report.VoteMetadata[i]
		}
	}
	if rollup == nil {
		t.Fatalf("vote_metadata carries no routed_to rollup: %+v",
			report.VoteMetadata)
	}
	if len(rollup.Values) != 1 || rollup.Values[0].Value != "seat-target-x" ||
		rollup.Values[0].Count != 2 {
		t.Errorf("routed_to rollup = %+v, want seat-target-x counted twice",
			rollup.Values)
	}

	// The casts' claims must NOT leak into the STEP rollup: the two sections
	// answer different questions about different actors.
	for _, key := range report.Metadata {
		if key.Key == "routed_to" {
			t.Errorf("a cast vote's claim leaked into the step metadata rollup")
		}
	}
}

// TestRunReportRollsUpVoteUsage pins DKT-95's read half: the spend seats
// report at cast time surfaces in the run report as a per-unit sum, from the
// vote_usage ledger — the one spend usage_ledger cannot key (a vote step's
// attempt is permanently 0, so seats collide). Before this section, a run
// report read vote spend as an absent zero until an operator ran
// `dispatch backfill-usage`.
func TestRunReportRollsUpVoteUsage(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "spend-panel", "seat-a,seat-b")
	driveToReconcile(t, conn, e, clusteredPayload)
	nextRun(t, conn, e)

	held := heldInstances(t, conn)
	if len(held) == 0 {
		t.Fatal("nothing held")
	}
	proposalID := heldProposalID(t, conn, e, held[0])
	for i, seat := range []string{"seat-a", "seat-b"} {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID:      proposalID,
			VoterName:       seat,
			Verdict:         model.VerdictApprove,
			Confidence:      0.9,
			DomainRelevance: 0.8,
			Usage:           map[string]float64{"tokens": float64(100 * (i + 1))},
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	var tokens *db.UnitTotal
	for i := range report.VoteUsage {
		if report.VoteUsage[i].Unit == "tokens" {
			tokens = &report.VoteUsage[i]
		}
	}
	if tokens == nil {
		t.Fatalf("vote_usage carries no tokens rollup: %+v", report.VoteUsage)
	}
	if tokens.Quantity != 300 || tokens.Rows != 2 {
		t.Errorf("tokens rollup = %+v, want 300 across 2 seat reports", tokens)
	}
}
