package db_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/db/pgstore"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// contractStore is one Store implementation under the contract suite. open
// returns a fresh, migrated store that no other case shares and that closes
// when the test ends.
type contractStore struct {
	name string
	open func(t *testing.T) store.Store
}

// contractCase is one behavior every Store implementation must show. It
// reaches the store only through the Store and Tx interfaces, so the same
// case runs unchanged against each implementation.
type contractCase struct {
	name string
	run  func(t *testing.T, st store.Store)
}

// TestStoreContract runs every contract case against every store, each case
// on a store of its own.
func TestStoreContract(t *testing.T) {
	stores := []contractStore{
		{name: "sqlite", open: openSQLiteContractStore},
		{name: "postgres", open: openPostgresContractStore},
	}

	var cases []contractCase
	cases = append(cases, versionContract...)
	cases = append(cases, leaseContract...)
	cases = append(cases, commentContract...)
	cases = append(cases, relationContract...)
	cases = append(cases, issueContract...)
	cases = append(cases, methodGroupContract...)

	for _, s := range stores {
		t.Run(s.name, func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					c.run(t, s.open(t))
				})
			}
		})
	}
}

// openSQLiteContractStore opens the SQLite store on a file rather than
// :memory:, because the store's reader is a second connection and must see
// the writer's database.
func openSQLiteContractStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "issues.db"))
	testsupport.Must(t, err, "OpenSQLite: %v", err)
	t.Cleanup(func() { st.Close() })
	err = st.Initialize()
	testsupport.Must(t, err, "Initialize: %v", err)
	err = st.Migrate()
	testsupport.Must(t, err, "Migrate: %v", err)
	return st
}

// openPostgresContractStore opens the PostgreSQL store in a schema made for
// this case alone and dropped when it ends. It skips without
// DOCKET_TEST_POSTGRES_DSN.
func openPostgresContractStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	dsn, passwordFile := testsupport.PostgresDSN(t), testsupport.PostgresPasswordFile()

	admin, err := pgstore.Open(ctx, dsn, passwordFile)
	testsupport.Must(t, err, "pgstore.Open: %v", err)
	t.Cleanup(func() { admin.Close() })
	suffix := make([]byte, 8)
	_, err = rand.Read(suffix)
	testsupport.Must(t, err, "rand.Read: %v", err)
	schema := "docket_contract_" + hex.EncodeToString(suffix)
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	testsupport.Must(t, err, "creating schema %s: %v", schema, err)
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Errorf("dropping schema %s: %v", schema, err)
		}
	})

	st, err := pgstore.OpenStore(ctx, dsn, passwordFile, schema)
	testsupport.Must(t, err, "pgstore.OpenStore: %v", err)
	// Registered after the schema drop, so it runs first and the drop waits
	// on no open connection.
	t.Cleanup(func() { st.Close() })
	err = st.Initialize()
	testsupport.Must(t, err, "Initialize: %v", err)
	err = st.Migrate()
	testsupport.Must(t, err, "Migrate: %v", err)
	return st
}

// createContractIssue creates a todo task and returns its id.
func createContractIssue(t *testing.T, st store.Store, title string) int {
	t.Helper()
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title:    title,
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, nil, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	return id
}

// methodGroupContract pins one representative call from each Store method
// group whose helper leans on engine-specific behavior: snapshot isolation,
// identity allocation after an import, integer-stored booleans, JSON
// functions, conflict-error classification, and conflict-skipping updates.
var methodGroupContract = []contractCase{
	{"ListProjectsIncludesAnEnsuredProject", contractListProjectsIncludesAnEnsuredProject},
	{"ReadSnapshotDoesNotSeeLaterCommits", contractReadSnapshotDoesNotSeeLaterCommits},
	{"CreateAfterInsertWithIDGetsAFreshID", contractCreateAfterInsertWithIDGetsAFreshID},
	{"CreateProposalKeepsBooleanPolicy", contractCreateProposalKeepsBooleanPolicy},
	{"ListProposalsFiltersByDomainTag", contractListProposalsFiltersByDomainTag},
	{"CastVoteDuplicateVoterIsConflict", contractCastVoteDuplicateVoterIsConflict},
	{"LinkProposalIssueTwiceIsConflict", contractLinkProposalIssueTwiceIsConflict},
	{"MoveIssueProjectRemapsLabels", contractMoveIssueProjectRemapsLabels},
	{"CountEventsByProjectReads", contractCountEventsByProjectReads},
}

// createContractProposal opens a proposal needing two voters.
func createContractProposal(t *testing.T, st store.Store, p model.Proposal) int {
	t.Helper()
	p.Criticality = model.CriticalityMedium
	p.Status = model.ProposalStatusOpen
	p.RequiredVoters = 2
	p.Threshold = 0.5
	id, err := st.CreateProposalIdempotent(&p, "")
	testsupport.Must(t, err, "CreateProposalIdempotent: %v", err)
	return id
}

func contractListProjectsIncludesAnEnsuredProject(t *testing.T, st store.Store) {
	id, err := st.EnsureProject("/repo/contract", "contract", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	projects, err := st.ListProjects()
	testsupport.Must(t, err, "ListProjects: %v", err)
	if !slices.ContainsFunc(projects, func(p *model.Project) bool { return p.ID == id }) {
		t.Errorf("ListProjects() has no project %d: %v", id, projects)
	}
}

func contractReadSnapshotDoesNotSeeLaterCommits(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "snapshot")
	snap, err := st.BeginReadSnapshot()
	testsupport.Must(t, err, "BeginReadSnapshot: %v", err)
	defer snap.Rollback()
	before, err := snap.GetIssueFilesTx(id)
	testsupport.Must(t, err, "GetIssueFilesTx: %v", err)

	err = st.AttachFiles(id, []string{"a.go"}, "tester")
	testsupport.Must(t, err, "AttachFiles: %v", err)

	after, err := snap.GetIssueFilesTx(id)
	testsupport.Must(t, err, "GetIssueFilesTx: %v", err)
	if !slices.Equal(before, after) {
		t.Errorf("snapshot read %v, then %v after a later commit", before, after)
	}
	committed, err := st.GetIssueFiles(id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if !slices.Equal(committed, []string{"a.go"}) {
		t.Errorf("GetIssueFiles = %v, want [a.go]", committed)
	}
}

func contractCreateAfterInsertWithIDGetsAFreshID(t *testing.T, st store.Store) {
	const importedID = 500
	now := time.Now().UTC().Truncate(time.Second)
	tx, err := st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	inserted, err := tx.InsertIssueWithID(&model.Issue{
		ID:        importedID,
		Title:     "imported",
		Status:    model.StatusTodo,
		Priority:  model.PriorityMedium,
		Kind:      model.IssueKindTask,
		CreatedAt: now,
		UpdatedAt: now,
	})
	testsupport.Must(t, err, "InsertIssueWithID: %v", err)
	if !inserted {
		t.Fatal("InsertIssueWithID inserted nothing into an empty store")
	}
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	if id := createContractIssue(t, st, "created after the import"); id <= importedID {
		t.Errorf("created issue got id %d, want one above the imported %d", id, importedID)
	}
}

func contractCreateProposalKeepsBooleanPolicy(t *testing.T, st store.Store) {
	id := createContractProposal(t, st, model.Proposal{Description: "sealed", Sealed: true, HoldOnDissent: true})
	got, err := st.GetProposal(id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if !got.Sealed || !got.HoldOnDissent {
		t.Errorf("GetProposal: Sealed=%v HoldOnDissent=%v, want both true", got.Sealed, got.HoldOnDissent)
	}
}

func contractListProposalsFiltersByDomainTag(t *testing.T, st store.Store) {
	storage := createContractProposal(t, st, model.Proposal{Description: "storage", DomainTags: []string{"db", "storage"}})
	createContractProposal(t, st, model.Proposal{Description: "ui", DomainTags: []string{"ui"}})

	got, total, err := st.ListProposals(0, "", "", "storage", 0)
	testsupport.Must(t, err, "ListProposals: %v", err)
	if total != 1 || len(got) != 1 || got[0].ID != storage {
		t.Errorf("ListProposals(domain storage) = %d rows, total %d; want only proposal %d", len(got), total, storage)
	}
}

func contractCastVoteDuplicateVoterIsConflict(t *testing.T, st store.Store) {
	id := createContractProposal(t, st, model.Proposal{Description: "duplicate voter"})
	vote := model.Vote{ProposalID: id, VoterName: "voter-1", Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9}
	first := vote
	_, err := st.CastVote(&first)
	testsupport.Must(t, err, "CastVote: %v", err)
	again := vote
	if _, err := st.CastVote(&again); !errors.Is(err, db.ErrConflict) {
		t.Errorf("second CastVote by the same voter: got %v, want ErrConflict", err)
	}
}

func contractLinkProposalIssueTwiceIsConflict(t *testing.T, st store.Store) {
	proposal := createContractProposal(t, st, model.Proposal{Description: "link"})
	issue := createContractIssue(t, st, "linked")
	err := st.LinkProposalIssue(proposal, issue)
	testsupport.Must(t, err, "LinkProposalIssue: %v", err)
	if err := st.LinkProposalIssue(proposal, issue); !errors.Is(err, db.ErrConflict) {
		t.Errorf("second LinkProposalIssue: got %v, want ErrConflict", err)
	}
}

func contractMoveIssueProjectRemapsLabels(t *testing.T, st store.Store) {
	from, err := st.EnsureProject("/repo/from", "from", 1)
	testsupport.Must(t, err, "EnsureProject(from): %v", err)
	to, err := st.EnsureProject("/repo/to", "to", 2)
	testsupport.Must(t, err, "EnsureProject(to): %v", err)
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: from,
		Title:     "mis-filed",
		Status:    model.StatusTodo,
		Priority:  model.PriorityMedium,
		Kind:      model.IssueKindTask,
	}, []string{"engine"}, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)

	moved, err := st.MoveIssueProject(id, to, "tester", 1000)
	testsupport.Must(t, err, "MoveIssueProject: %v", err)
	if !slices.Equal(moved, []int{id}) {
		t.Errorf("MoveIssueProject moved %v, want [%d]", moved, id)
	}
	label, err := st.GetLabelByName(to, "engine")
	testsupport.Must(t, err, "GetLabelByName(to, engine): %v", err)
	if label.IssueCount != 1 {
		t.Errorf("label engine in the target project carries %d issues, want 1", label.IssueCount)
	}
	names, err := st.GetIssueLabels(id)
	testsupport.Must(t, err, "GetIssueLabels: %v", err)
	if !slices.Equal(names, []string{"engine"}) {
		t.Errorf("GetIssueLabels after the move = %v, want [engine]", names)
	}
}

func contractCountEventsByProjectReads(t *testing.T, st store.Store) {
	snap, err := st.BeginReadSnapshot()
	testsupport.Must(t, err, "BeginReadSnapshot: %v", err)
	defer snap.Rollback()
	n, err := snap.CountEventsTx(db.EventFilter{ProjectID: 1})
	testsupport.Must(t, err, "CountEventsTx: %v", err)
	if n != 0 {
		t.Errorf("CountEventsTx on a fresh store = %d, want 0", n)
	}
}
