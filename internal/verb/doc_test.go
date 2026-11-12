package verb_test

import (
	"context"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func createDoc(t *testing.T, st store.Store, projectID int, title string) int {
	t.Helper()
	id, err := st.CreateDocIdempotent(&model.Doc{
		ProjectID: projectID, Type: "note", Status: "draft", Title: title, Body: "body of " + title, Author: "tester",
	}, "")
	if err != nil {
		t.Fatalf("CreateDocIdempotent(%q): %v", title, err)
	}
	return id
}

func addDocComment(t *testing.T, st store.Store, docID int, body string) {
	t.Helper()
	if _, err := st.CreateDocCommentIdempotent(&model.DocComment{DocID: docID, Body: body, Author: "tester"}, ""); err != nil {
		t.Fatalf("CreateDocCommentIdempotent: %v", err)
	}
}

func docTitles(res verb.DocListResult) []string {
	titles := make([]string, 0, len(res.Docs))
	for _, s := range res.Docs {
		titles = append(titles, s.Doc.Title)
	}
	return titles
}

func TestDocListListsOnlyTheResolvedProjectsDocs(t *testing.T) {
	st := openStore(t)
	// The first identity registered adopts the default project; the second
	// gets a row of its own.
	first, err := st.EnsureProject("/repos/first", "first", 1)
	if err != nil {
		t.Fatalf("EnsureProject(first): %v", err)
	}
	other, err := st.EnsureProject("/repos/other", "other", 1)
	if err != nil {
		t.Fatalf("EnsureProject(other): %v", err)
	}
	createDoc(t, st, first, "first doc")
	createDoc(t, st, other, "other doc")

	res, err := verb.DocList(context.Background(), projectEnv(st, "/repos/other", false), verb.DocListInput{})
	if err != nil {
		t.Fatalf("DocList: %v", err)
	}
	if got := docTitles(res); len(got) != 1 || got[0] != "other doc" || res.Total != 1 {
		t.Errorf("DocList in /repos/other = %v (total %d), want [other doc] (total 1)", got, res.Total)
	}
}

func TestDocListUnregisteredIdentityListsNothingAndRegistersNothing(t *testing.T) {
	st := openStore(t)
	createDoc(t, st, store.DefaultProjectID, "default doc")

	res, err := verb.DocList(context.Background(), projectEnv(st, "/repos/new", true), verb.DocListInput{})
	if err != nil {
		t.Fatalf("DocList: %v", err)
	}
	if len(res.Docs) != 0 || res.Total != 0 {
		t.Errorf("DocList for an unregistered identity = %v (total %d), want none", docTitles(res), res.Total)
	}
	requireUnregistered(t, st, "/repos/new")
}

func TestDocListAppliesSortDirectionAndLimit(t *testing.T) {
	st := openStore(t)
	createDoc(t, st, store.DefaultProjectID, "first")
	createDoc(t, st, store.DefaultProjectID, "second")
	createDoc(t, st, store.DefaultProjectID, "third")

	res, err := verb.DocList(context.Background(), projectEnv(st, "", false),
		verb.DocListInput{Sort: "id:desc", Limit: 2})
	if err != nil {
		t.Fatalf("DocList: %v", err)
	}
	got := docTitles(res)
	if len(got) != 2 || got[0] != "third" || got[1] != "second" || res.Total != 3 {
		t.Errorf("DocList --sort id:desc --limit 2 = %v (total %d), want [third second] (total 3)", got, res.Total)
	}
}

func TestDocShowReturnsTheDocWithRevisionsAndComments(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "shown")
	addDocComment(t, st, id, "looks right")

	res, err := verb.DocShow(context.Background(), projectEnv(st, "", false),
		verb.DocShowInput{Doc: model.FormatDocID(id)})
	if err != nil {
		t.Fatalf("DocShow: %v", err)
	}
	if res.Doc == nil || res.Doc.Title != "shown" {
		t.Fatalf("DocShow doc = %#v, want the doc titled shown", res.Doc)
	}
	if len(res.Revisions) != 1 || len(res.Comments) != 1 || res.Comments[0].Body != "looks right" {
		t.Errorf("DocShow = %d revisions, %d comments, want 1 create revision and the one comment",
			len(res.Revisions), len(res.Comments))
	}
	if res.Revision != nil {
		t.Errorf("DocShow without a revision returned Revision %#v, want nil", res.Revision)
	}
}

func TestDocShowRevisionReturnsOnlyThatRevision(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "revised")
	rev := 1

	res, err := verb.DocShow(context.Background(), projectEnv(st, "", false),
		verb.DocShowInput{Doc: model.FormatDocID(id), Rev: &rev})
	if err != nil {
		t.Fatalf("DocShow --rev 1: %v", err)
	}
	if res.Revision == nil || res.Revision.DocID != id || res.Revision.RevisionNumber != 1 {
		t.Errorf("DocShow --rev 1 revision = %#v, want revision 1 of doc %d", res.Revision, id)
	}
	if res.Doc != nil {
		t.Errorf("DocShow --rev 1 returned Doc %#v, want only the revision", res.Doc)
	}
}

func TestDocShowErrorCodes(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "doc")
	ref := model.FormatDocID(id)
	env := projectEnv(st, "", false)
	negative, missing := -1, 999

	_, err := verb.DocShow(context.Background(), env, verb.DocShowInput{Doc: "not-a-doc"})
	requireVerbError(t, err, output.ErrValidation, "invalid doc ID: ")

	_, err = verb.DocShow(context.Background(), env, verb.DocShowInput{Doc: "DOC-999"})
	requireVerbError(t, err, output.ErrNotFound, "doc DOC-999 not found")

	_, err = verb.DocShow(context.Background(), env, verb.DocShowInput{Doc: ref, Rev: &negative})
	requireVerbError(t, err, output.ErrValidation, "")

	_, err = verb.DocShow(context.Background(), env, verb.DocShowInput{Doc: ref, Rev: &missing})
	requireVerbError(t, err, output.ErrNotFound, "doc "+ref+" revision 999 not found")
}

func TestDocCommentListReturnsTheDocsComments(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "commented")
	addDocComment(t, st, id, "one")
	addDocComment(t, st, id, "two")

	res, err := verb.DocCommentList(context.Background(), projectEnv(st, "", false),
		verb.DocCommentListInput{Doc: model.FormatDocID(id)})
	if err != nil {
		t.Fatalf("DocCommentList: %v", err)
	}
	if res.DocID != id || len(res.Comments) != 2 || res.Comments[0].Body != "one" || res.Comments[1].Body != "two" {
		t.Errorf("DocCommentList = doc %d, %d comments, want doc %d with [one two]", res.DocID, len(res.Comments), id)
	}
}

func TestDocCommentListErrorCodes(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)

	_, err := verb.DocCommentList(context.Background(), env, verb.DocCommentListInput{Doc: "not-a-doc"})
	requireVerbError(t, err, output.ErrValidation, "invalid doc ID: ")

	_, err = verb.DocCommentList(context.Background(), env, verb.DocCommentListInput{Doc: "DOC-999"})
	requireVerbError(t, err, output.ErrNotFound, "doc DOC-999 not found")
}

// TestDocReadsRegisterNoProject: all three verbs are reads, so an anchored
// identity with no project row stays unregistered.
func TestDocReadsRegisterNoProject(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "doc")
	ref := model.FormatDocID(id)
	env := projectEnv(st, "/repos/new", true)

	if _, err := verb.DocShow(context.Background(), env, verb.DocShowInput{Doc: ref}); err != nil {
		t.Fatalf("DocShow: %v", err)
	}
	if _, err := verb.DocCommentList(context.Background(), env, verb.DocCommentListInput{Doc: ref}); err != nil {
		t.Fatalf("DocCommentList: %v", err)
	}
	requireUnregistered(t, st, "/repos/new")
}
