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

func TestDocCreateWritesIntoTheResolvedProjectAsTheEnvAuthor(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")
	other := ensureProject(t, st, "/repos/other", "other")
	env := projectEnv(st, "/repos/other", true)
	env.Author = "alice"

	res, err := verb.DocCreate(context.Background(), env, verb.DocCreateInput{
		Title: "plan", Body: "the body", Type: "spec", Status: "draft",
	})
	if err != nil {
		t.Fatalf("DocCreate: %v", err)
	}
	doc := res.Doc
	if doc == nil || doc.Title != "plan" || doc.Body != "the body" || doc.Type != "spec" || doc.Status != "draft" {
		t.Fatalf("DocCreate doc = %#v, want the doc as given", doc)
	}
	if doc.Author != "alice" {
		t.Errorf("DocCreate doc author %q, want alice", doc.Author)
	}
	requireProjectDocs(t, st, other, doc.ID)
	if res.Registered != nil {
		t.Errorf("Registered = %+v, want nil for an already registered project", res.Registered)
	}
}

func TestDocCreateRegistersAnAnchoredProject(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")

	res, err := verb.DocCreate(context.Background(), projectEnv(st, "/repos/fresh", true),
		verb.DocCreateInput{Title: "first doc"})
	if err != nil {
		t.Fatalf("DocCreate: %v", err)
	}
	id, found, err := st.LookupProject("/repos/fresh")
	if err != nil || !found {
		t.Fatalf("LookupProject = (%d, %v, %v), want a registered row", id, found, err)
	}
	if reg := res.Registered; reg == nil || reg.ID != id || reg.Identity != "/repos/fresh" {
		t.Errorf("Registered = %+v, want the new row %d", reg, id)
	}
	requireProjectDocs(t, st, id, res.Doc.ID)
}

// requireProjectDocs asserts project holds exactly the doc docID.
func requireProjectDocs(t *testing.T, st store.Store, project, docID int) {
	t.Helper()
	docs, total, err := st.ListDocsWithCounts(store.DocListOptions{ProjectID: project})
	if err != nil {
		t.Fatalf("ListDocsWithCounts: %v", err)
	}
	if total != 1 || len(docs) != 1 || docs[0].Doc.ID != docID {
		t.Errorf("project %d holds %d docs, want only doc %d", project, total, docID)
	}
}

func TestDocCreateReplaysAnIdempotencyKeyAndRefusesItsReuse(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)

	first, err := verb.DocCreate(context.Background(), env, verb.DocCreateInput{Title: "once", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("DocCreate: %v", err)
	}
	again, err := verb.DocCreate(context.Background(), env, verb.DocCreateInput{Title: "once", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("DocCreate replay: %v", err)
	}
	if again.Doc.ID != first.Doc.ID {
		t.Errorf("replayed DocCreate = doc %d, want the original doc %d", again.Doc.ID, first.Doc.ID)
	}

	_, err = verb.DocCreate(context.Background(), env, verb.DocCreateInput{Title: "different", IdempotencyKey: "k1"})
	requireVerbError(t, err, output.ErrConflict, "")
}

// TestDocWritesRefuseAnUnanchoredIdentity: create, edit and delete write, so
// an unanchored identity with no project row is refused before any change and
// registers nothing.
func TestDocWritesRefuseAnUnanchoredIdentity(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")
	id := createDoc(t, st, store.DefaultProjectID, "kept")
	ref := model.FormatDocID(id)
	env := projectEnv(st, "/work/scratch", false)
	title := "changed"

	_, err := verb.DocCreate(context.Background(), env, verb.DocCreateInput{Title: "new"})
	requireVerbError(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	_, err = verb.DocEdit(context.Background(), env, verb.DocEditInput{Doc: ref, Title: &title})
	requireVerbError(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	_, err = verb.DocDelete(context.Background(), env, verb.DocDeleteInput{Doc: ref, Force: true})
	requireVerbError(t, err, output.ErrValidation, "cannot tell which project this belongs to")

	requireUnregistered(t, st, "/work/scratch")
	doc, err := st.GetDoc(id)
	if err != nil || doc.Title != "kept" {
		t.Errorf("GetDoc = (%#v, %v), want the doc untouched", doc, err)
	}
	if _, total, err := st.ListDocsWithCounts(store.DocListOptions{ProjectID: store.DefaultProjectID}); err != nil || total != 1 {
		t.Errorf("docs in the default project = (%d, %v), want only the kept doc", total, err)
	}
}

func TestDocEditAppliesTheGivenFieldsAsTheEnvAuthor(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "old title")
	env := projectEnv(st, "", false)
	env.Author = "bob"
	title, body := "new title", "new body"

	res, err := verb.DocEdit(context.Background(), env, verb.DocEditInput{
		Doc: model.FormatDocID(id), Title: &title, Body: &body,
	})
	if err != nil {
		t.Fatalf("DocEdit: %v", err)
	}
	if !res.Changed || res.Doc == nil || res.Doc.Title != "new title" || res.Doc.Body != "new body" {
		t.Fatalf("DocEdit = changed %v doc %#v, want the changed doc", res.Changed, res.Doc)
	}
	if res.Doc.Type != "note" || res.Doc.Status != "draft" {
		t.Errorf("DocEdit doc type %q status %q, want the untouched note/draft", res.Doc.Type, res.Doc.Status)
	}
	revs, err := st.ListDocRevisions(id)
	if err != nil {
		t.Fatalf("ListDocRevisions: %v", err)
	}
	if latest := revs[len(revs)-1]; latest.Author != "bob" {
		t.Errorf("latest revision author = %q, want bob", latest.Author)
	}
}

func TestDocEditWithoutAChangeReportsUnchanged(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "same")
	ref := model.FormatDocID(id)
	env := projectEnv(st, "", false)
	same := "same"

	for name, in := range map[string]verb.DocEditInput{
		"no fields":       {Doc: ref},
		"identical title": {Doc: ref, Title: &same},
	} {
		res, err := verb.DocEdit(context.Background(), env, in)
		if err != nil {
			t.Fatalf("DocEdit %s: %v", name, err)
		}
		if res.Changed || res.Doc == nil || res.Doc.ID != id {
			t.Errorf("DocEdit %s = changed %v doc %#v, want the unchanged doc %d", name, res.Changed, res.Doc, id)
		}
	}
}

func TestDocEditErrorCodes(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	title := "t"

	_, err := verb.DocEdit(context.Background(), env, verb.DocEditInput{Doc: "not-a-doc", Title: &title})
	requireVerbError(t, err, output.ErrValidation, "invalid doc ID: ")

	_, err = verb.DocEdit(context.Background(), env, verb.DocEditInput{Doc: "999", Title: &title})
	requireVerbError(t, err, output.ErrNotFound, "doc DOC-999 not found")
}

func TestDocDeleteWithoutForceDeletesNothingAndReturnsTheDoc(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "keep me")
	ref := model.FormatDocID(id)

	res, err := verb.DocDelete(context.Background(), projectEnv(st, "", false), verb.DocDeleteInput{Doc: ref})
	requireVerbError(t, err, output.ErrValidation, "deleting "+ref+" is destructive; pass --force to confirm")
	if res.Doc == nil || res.Doc.ID != id || res.Doc.Title != "keep me" {
		t.Errorf("unconfirmed DocDelete doc = %#v, want the doc it would delete", res.Doc)
	}
	if _, err := st.GetDoc(id); err != nil {
		t.Errorf("the unconfirmed delete removed %s: %v", ref, err)
	}
}

func TestDocDeleteWithForceDeletesTheDoc(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "gone")

	res, err := verb.DocDelete(context.Background(), projectEnv(st, "", false),
		verb.DocDeleteInput{Doc: model.FormatDocID(id), Force: true})
	if err != nil {
		t.Fatalf("DocDelete: %v", err)
	}
	if res.Doc == nil || res.Doc.ID != id || res.Doc.Title != "gone" {
		t.Errorf("DocDelete doc = %#v, want the deleted doc", res.Doc)
	}
	if _, err := st.GetDoc(id); err == nil {
		t.Errorf("DocDelete --force left %s in place", model.FormatDocID(id))
	}
}

func TestDocDeleteErrorCodes(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)

	_, err := verb.DocDelete(context.Background(), env, verb.DocDeleteInput{Doc: "not-a-doc", Force: true})
	requireVerbError(t, err, output.ErrValidation, "invalid doc ID: ")

	// A missing doc is reported before the consent check.
	_, err = verb.DocDelete(context.Background(), env, verb.DocDeleteInput{Doc: "999"})
	requireVerbError(t, err, output.ErrNotFound, "doc DOC-999 not found")
}

func TestDocCommentAddStoresTheCommentAsTheEnvAuthor(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "commented")
	env := projectEnv(st, "", false)
	env.Author = "carol"

	res, err := verb.DocCommentAdd(context.Background(), env, verb.DocCommentAddInput{
		Doc: model.FormatDocID(id), Body: "looks good",
	})
	if err != nil {
		t.Fatalf("DocCommentAdd: %v", err)
	}
	if res.Doc == nil || res.Doc.ID != id || res.Doc.Title != "commented" {
		t.Errorf("DocCommentAdd doc = %#v, want doc %d titled commented", res.Doc, id)
	}
	stored, err := st.ListDocComments(id)
	if err != nil {
		t.Fatalf("ListDocComments: %v", err)
	}
	if len(stored) != 1 || stored[0].Body != "looks good" || stored[0].Author != "carol" {
		t.Fatalf("stored comments = %#v, want one by carol saying looks good", stored)
	}
	if c := res.Comment; c == nil || c.ID != stored[0].ID || c.Body != "looks good" || c.Author != "carol" {
		t.Errorf("DocCommentAdd comment = %#v, want the stored comment %d", c, stored[0].ID)
	}
}

func TestDocCommentAddReplaysAnIdempotencyKeyAndRefusesItsReuse(t *testing.T) {
	st := openStore(t)
	id := createDoc(t, st, store.DefaultProjectID, "doc")
	ref := model.FormatDocID(id)
	env := projectEnv(st, "", false)

	first, err := verb.DocCommentAdd(context.Background(), env, verb.DocCommentAddInput{Doc: ref, Body: "once", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("DocCommentAdd: %v", err)
	}
	again, err := verb.DocCommentAdd(context.Background(), env, verb.DocCommentAddInput{Doc: ref, Body: "once", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("DocCommentAdd replay: %v", err)
	}
	if again.Comment.ID != first.Comment.ID {
		t.Errorf("replayed DocCommentAdd = comment %d, want the original comment %d", again.Comment.ID, first.Comment.ID)
	}

	_, err = verb.DocCommentAdd(context.Background(), env, verb.DocCommentAddInput{Doc: ref, Body: "different", IdempotencyKey: "k1"})
	requireVerbError(t, err, output.ErrConflict, "")
	if stored, err := st.ListDocComments(id); err != nil || len(stored) != 1 {
		t.Errorf("comments after the refused reuse = (%d, %v), want 1", len(stored), err)
	}
}

func TestDocCommentAddErrorCodes(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)

	_, err := verb.DocCommentAdd(context.Background(), env, verb.DocCommentAddInput{Doc: "not-a-doc", Body: "b"})
	requireVerbError(t, err, output.ErrValidation, "invalid doc ID: ")

	_, err = verb.DocCommentAdd(context.Background(), env, verb.DocCommentAddInput{Doc: "DOC-999", Body: "b"})
	requireVerbError(t, err, output.ErrNotFound, "doc DOC-999 not found")
}

func TestDocLinkAddThenRemove(t *testing.T) {
	st := openStore(t)
	docID := createDoc(t, st, store.DefaultProjectID, "linked")
	issueID := createIssue(t, st)
	env := projectEnv(st, "", false)
	docRef, issueRef := model.FormatDocID(docID), model.FormatID(issueID)

	added, err := verb.DocLinkAdd(context.Background(), env, verb.DocLinkAddInput{Doc: docRef, Issue: issueRef})
	if err != nil {
		t.Fatalf("DocLinkAdd: %v", err)
	}
	if added.DocID != docID || added.IssueID != issueID {
		t.Errorf("DocLinkAdd = doc %d issue %d, want doc %d issue %d", added.DocID, added.IssueID, docID, issueID)
	}
	if linked := docLinkedIssues(t, st, docID); len(linked) != 1 || linked[0] != issueID {
		t.Fatalf("linked issues after add = %v, want [%d]", linked, issueID)
	}

	_, err = verb.DocLinkAdd(context.Background(), env, verb.DocLinkAddInput{Doc: docRef, Issue: issueRef})
	requireVerbError(t, err, output.ErrConflict, "link already exists")

	removed, err := verb.DocLinkRemove(context.Background(), env, verb.DocLinkRemoveInput{Doc: docRef, Issue: issueRef})
	if err != nil {
		t.Fatalf("DocLinkRemove: %v", err)
	}
	if removed.DocID != docID || removed.IssueID != issueID {
		t.Errorf("DocLinkRemove = doc %d issue %d, want doc %d issue %d", removed.DocID, removed.IssueID, docID, issueID)
	}
	if linked := docLinkedIssues(t, st, docID); len(linked) != 0 {
		t.Fatalf("linked issues after remove = %v, want none", linked)
	}

	_, err = verb.DocLinkRemove(context.Background(), env, verb.DocLinkRemoveInput{Doc: docRef, Issue: issueRef})
	requireVerbError(t, err, output.ErrNotFound, "link not found")
}

func docLinkedIssues(t *testing.T, st store.Store, docID int) []int {
	t.Helper()
	linked, err := st.HydrateLinkedIssues([]int{docID})
	if err != nil {
		t.Fatalf("HydrateLinkedIssues: %v", err)
	}
	var ids []int
	for _, ref := range linked[docID] {
		ids = append(ids, ref.ID)
	}
	return ids
}

func TestDocLinkErrorCodes(t *testing.T) {
	st := openStore(t)
	docRef := model.FormatDocID(createDoc(t, st, store.DefaultProjectID, "doc"))
	env := projectEnv(st, "", false)

	_, err := verb.DocLinkAdd(context.Background(), env, verb.DocLinkAddInput{Doc: "not-a-doc", Issue: "DKT-1"})
	requireVerbError(t, err, output.ErrValidation, "invalid doc ID: ")
	_, err = verb.DocLinkAdd(context.Background(), env, verb.DocLinkAddInput{Doc: docRef, Issue: "not-an-issue"})
	requireVerbError(t, err, output.ErrValidation, "invalid issue ID: ")
	_, err = verb.DocLinkAdd(context.Background(), env, verb.DocLinkAddInput{Doc: docRef, Issue: "DKT-999"})
	requireVerbError(t, err, output.ErrNotFound, "doc or issue not found")

	_, err = verb.DocLinkRemove(context.Background(), env, verb.DocLinkRemoveInput{Doc: "not-a-doc", Issue: "DKT-1"})
	requireVerbError(t, err, output.ErrValidation, "invalid doc ID: ")
	_, err = verb.DocLinkRemove(context.Background(), env, verb.DocLinkRemoveInput{Doc: docRef, Issue: "not-an-issue"})
	requireVerbError(t, err, output.ErrValidation, "invalid issue ID: ")
}

// TestDocCommentAndLinkWritesRefuseAnUnanchoredIdentity: comment add, link add
// and link remove write, so an unanchored identity with no project row is
// refused before any change and registers nothing.
func TestDocCommentAndLinkWritesRefuseAnUnanchoredIdentity(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")
	docID := createDoc(t, st, store.DefaultProjectID, "kept")
	issueID := createIssue(t, st)
	docRef, issueRef := model.FormatDocID(docID), model.FormatID(issueID)
	env := projectEnv(st, "/work/scratch", false)

	_, err := verb.DocCommentAdd(context.Background(), env, verb.DocCommentAddInput{Doc: docRef, Body: "b"})
	requireVerbError(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	_, err = verb.DocLinkAdd(context.Background(), env, verb.DocLinkAddInput{Doc: docRef, Issue: issueRef})
	requireVerbError(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	_, err = verb.DocLinkRemove(context.Background(), env, verb.DocLinkRemoveInput{Doc: docRef, Issue: issueRef})
	requireVerbError(t, err, output.ErrValidation, "cannot tell which project this belongs to")

	requireUnregistered(t, st, "/work/scratch")
	if stored, err := st.ListDocComments(docID); err != nil || len(stored) != 0 {
		t.Errorf("comments = (%d, %v), want none", len(stored), err)
	}
	if linked := docLinkedIssues(t, st, docID); len(linked) != 0 {
		t.Errorf("linked issues = %v, want none", linked)
	}
}

// TestDocCommentAndLinkWritesRegisterAnAnchoredProject: each write registers
// the caller's anchored identity on first contact and reports the new row.
func TestDocCommentAndLinkWritesRegisterAnAnchoredProject(t *testing.T) {
	ctx := context.Background()
	for name, write := range map[string]func(env verb.Env, docRef, issueRef string) (*model.Project, error){
		"comment add": func(env verb.Env, docRef, _ string) (*model.Project, error) {
			res, err := verb.DocCommentAdd(ctx, env, verb.DocCommentAddInput{Doc: docRef, Body: "b"})
			return res.Registered, err
		},
		"link add": func(env verb.Env, docRef, issueRef string) (*model.Project, error) {
			res, err := verb.DocLinkAdd(ctx, env, verb.DocLinkAddInput{Doc: docRef, Issue: issueRef})
			return res.Registered, err
		},
		"link remove": func(env verb.Env, docRef, issueRef string) (*model.Project, error) {
			if _, err := verb.DocLinkAdd(ctx, projectEnv(env.Store, "", false),
				verb.DocLinkAddInput{Doc: docRef, Issue: issueRef}); err != nil {
				return nil, err
			}
			res, err := verb.DocLinkRemove(ctx, env, verb.DocLinkRemoveInput{Doc: docRef, Issue: issueRef})
			return res.Registered, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			ensureProject(t, st, "/repos/first", "first")
			docRef := model.FormatDocID(createDoc(t, st, store.DefaultProjectID, "doc"))
			issueRef := model.FormatID(createIssue(t, st))

			reg, err := write(projectEnv(st, "/repos/fresh", true), docRef, issueRef)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			id, found, err := st.LookupProject("/repos/fresh")
			if err != nil || !found {
				t.Fatalf("LookupProject = (%d, %v, %v), want a registered row", id, found, err)
			}
			if reg == nil || reg.ID != id || reg.Identity != "/repos/fresh" {
				t.Errorf("Registered = %+v, want the new row %d", reg, id)
			}
		})
	}
}

// TestDocCommentAndLinkWritesReportTheProjectTheyRegisteredBeforeFailing:
// a write that registers the caller's anchored identity and then fails still
// reports the new row beside its error, so the caller can record the
// project-registered event for a project that exists either way.
func TestDocCommentAndLinkWritesReportTheProjectTheyRegisteredBeforeFailing(t *testing.T) {
	ctx := context.Background()
	type fixture struct{ docRef, issueRef string }
	for _, tc := range []struct {
		name   string
		code   output.ErrorCode
		prefix string
		write  func(env verb.Env, f fixture) (*model.Project, error)
	}{
		{"comment target, missing doc", output.ErrNotFound, "doc DOC-999 not found",
			func(env verb.Env, _ fixture) (*model.Project, error) {
				res, err := verb.DocCommentTarget(ctx, env, verb.DocCommentTargetInput{Doc: "DOC-999"})
				return res.Registered, err
			}},
		{"comment add, malformed doc ID", output.ErrValidation, "invalid doc ID: ",
			func(env verb.Env, _ fixture) (*model.Project, error) {
				res, err := verb.DocCommentAdd(ctx, env, verb.DocCommentAddInput{Doc: "not-a-doc", Body: "b"})
				return res.Registered, err
			}},
		{"comment add, missing doc", output.ErrNotFound, "doc DOC-999 not found",
			func(env verb.Env, _ fixture) (*model.Project, error) {
				res, err := verb.DocCommentAdd(ctx, env, verb.DocCommentAddInput{Doc: "DOC-999", Body: "b"})
				return res.Registered, err
			}},
		{"comment add, reused key", output.ErrConflict, "",
			func(env verb.Env, f fixture) (*model.Project, error) {
				if _, err := verb.DocCommentAdd(ctx, projectEnv(env.Store, "", false),
					verb.DocCommentAddInput{Doc: f.docRef, Body: "first", IdempotencyKey: "k"}); err != nil {
					return nil, err
				}
				res, err := verb.DocCommentAdd(ctx, env,
					verb.DocCommentAddInput{Doc: f.docRef, Body: "second", IdempotencyKey: "k"})
				return res.Registered, err
			}},
		{"link add, malformed doc ID", output.ErrValidation, "invalid doc ID: ",
			func(env verb.Env, f fixture) (*model.Project, error) {
				res, err := verb.DocLinkAdd(ctx, env, verb.DocLinkAddInput{Doc: "not-a-doc", Issue: f.issueRef})
				return res.Registered, err
			}},
		{"link add, missing issue", output.ErrNotFound, "doc or issue not found",
			func(env verb.Env, f fixture) (*model.Project, error) {
				res, err := verb.DocLinkAdd(ctx, env, verb.DocLinkAddInput{Doc: f.docRef, Issue: "DKT-999"})
				return res.Registered, err
			}},
		{"link add, existing link", output.ErrConflict, "link already exists",
			func(env verb.Env, f fixture) (*model.Project, error) {
				if _, err := verb.DocLinkAdd(ctx, projectEnv(env.Store, "", false),
					verb.DocLinkAddInput{Doc: f.docRef, Issue: f.issueRef}); err != nil {
					return nil, err
				}
				res, err := verb.DocLinkAdd(ctx, env, verb.DocLinkAddInput{Doc: f.docRef, Issue: f.issueRef})
				return res.Registered, err
			}},
		{"link remove, malformed issue ID", output.ErrValidation, "invalid issue ID: ",
			func(env verb.Env, f fixture) (*model.Project, error) {
				res, err := verb.DocLinkRemove(ctx, env, verb.DocLinkRemoveInput{Doc: f.docRef, Issue: "not-an-issue"})
				return res.Registered, err
			}},
		{"link remove, missing link", output.ErrNotFound, "link not found",
			func(env verb.Env, f fixture) (*model.Project, error) {
				res, err := verb.DocLinkRemove(ctx, env, verb.DocLinkRemoveInput{Doc: f.docRef, Issue: f.issueRef})
				return res.Registered, err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			ensureProject(t, st, "/repos/first", "first")
			f := fixture{
				docRef:   model.FormatDocID(createDoc(t, st, store.DefaultProjectID, "doc")),
				issueRef: model.FormatID(createIssue(t, st)),
			}

			reg, err := tc.write(projectEnv(st, "/repos/fresh", true), f)
			requireVerbError(t, err, tc.code, tc.prefix)
			id, found, lookupErr := st.LookupProject("/repos/fresh")
			if lookupErr != nil || !found {
				t.Fatalf("LookupProject = (%d, %v, %v), want a registered row", id, found, lookupErr)
			}
			if reg == nil || reg.ID != id || reg.Identity != "/repos/fresh" {
				t.Errorf("Registered = %+v beside %v, want the new row %d", reg, err, id)
			}
		})
	}
}

// TestDocCommentTargetNamesTheDocToComment: the target of a comment is the doc
// the reference names, and an unanchored caller is refused before any lookup.
func TestDocCommentTargetNamesTheDocToComment(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")
	docID := createDoc(t, st, store.DefaultProjectID, "subject")
	ref := model.FormatDocID(docID)

	target, err := verb.DocCommentTarget(ctx, projectEnv(st, "", false), verb.DocCommentTargetInput{Doc: ref})
	if err != nil {
		t.Fatalf("DocCommentTarget: %v", err)
	}
	if target.Doc == nil || target.Doc.ID != docID || target.Doc.Title != "subject" {
		t.Errorf("DocCommentTarget = %+v, want doc %d titled %q", target.Doc, docID, "subject")
	}

	_, err = verb.DocCommentTarget(ctx, projectEnv(st, "", false), verb.DocCommentTargetInput{Doc: "not-a-doc"})
	requireVerbError(t, err, output.ErrValidation, "invalid doc ID: ")

	_, err = verb.DocCommentTarget(ctx, projectEnv(st, "/work/scratch", false), verb.DocCommentTargetInput{Doc: "DOC-999"})
	requireVerbError(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	requireUnregistered(t, st, "/work/scratch")
}
