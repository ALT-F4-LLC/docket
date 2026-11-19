package db

import (
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestCreateDocComment(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "body")

	c := &model.DocComment{
		DocID:  docID,
		Body:   "looks good",
		Author: "reviewer",
	}
	id, err := CreateDocComment(db, c)
	testsupport.Must(t, err, "CreateDocComment: %v", err)
	if id <= 0 {
		t.Fatalf("returned id = %d, want > 0", id)
	}

	got, err := GetDocComment(db, id)
	testsupport.Must(t, err, "GetDocComment: %v", err)
	if got.DocID != docID {
		t.Errorf("DocID = %d, want %d", got.DocID, docID)
	}
	if got.Body != "looks good" {
		t.Errorf("Body = %q", got.Body)
	}
	if got.Author != "reviewer" {
		t.Errorf("Author = %q", got.Author)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

func TestCreateDocComment_DocNotFound(t *testing.T) {
	db := mustInitAndMigrate(t)
	_, err := CreateDocComment(db, &model.DocComment{DocID: 999, Body: "x", Author: "a"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestCreateDocCommentIdempotent_KeyReuseWithDifferentContentConflicts: a key
// already used for one comment is rejected for a request that differs in its
// doc target, author or body, and the rejection names the key.
func TestCreateDocCommentIdempotent_KeyReuseWithDifferentContentConflicts(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "body")
	otherDocID := mustCreateDoc(t, db, "e", "tdd", "draft", "body")
	original := model.DocComment{DocID: docID, Body: "looks good", Author: "reviewer"}

	_, err := CreateDocCommentIdempotent(db, &original, "key-1")
	testsupport.Must(t, err, "first CreateDocCommentIdempotent: %v", err)

	for name, changed := range map[string]model.DocComment{
		"doc":    {DocID: otherDocID, Body: original.Body, Author: original.Author},
		"author": {DocID: original.DocID, Body: original.Body, Author: "someone-else"},
		"body":   {DocID: original.DocID, Body: "needs work", Author: original.Author},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CreateDocCommentIdempotent(db, &changed, "key-1")
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("reusing key-1 with a different %s returned %v, want an ErrConflict", name, err)
			}
			if !strings.Contains(err.Error(), "key-1") {
				t.Errorf("conflict %q does not name the key key-1", err)
			}
		})
	}

	comments, err := ListDocComments(db, docID)
	testsupport.Must(t, err, "ListDocComments: %v", err)
	if len(comments) != 1 {
		t.Errorf("doc has %d comments after the rejected reuses, want 1", len(comments))
	}
}

// TestCreateDocCommentIdempotent_KeyReuseWithSameContentReplays: repeating a
// keyed request with identical content returns the original comment and
// inserts nothing.
func TestCreateDocCommentIdempotent_KeyReuseWithSameContentReplays(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "body")

	first, err := CreateDocCommentIdempotent(db,
		&model.DocComment{DocID: docID, Body: "looks good", Author: "reviewer"}, "key-1")
	testsupport.Must(t, err, "first CreateDocCommentIdempotent: %v", err)

	replay, err := CreateDocCommentIdempotent(db,
		&model.DocComment{DocID: docID, Body: "looks good", Author: "reviewer"}, "key-1")
	testsupport.Must(t, err, "replayed CreateDocCommentIdempotent: %v", err)
	if replay != first {
		t.Errorf("replay returned comment %d, want the original %d", replay, first)
	}

	comments, err := ListDocComments(db, docID)
	testsupport.Must(t, err, "ListDocComments: %v", err)
	if len(comments) != 1 {
		t.Errorf("doc has %d comments after the replay, want 1", len(comments))
	}
}

func TestListDocComments(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")

	for i, body := range []string{"a", "b", "c"} {
		if _, err := CreateDocComment(db, &model.DocComment{
			DocID: docID, Body: body, Author: "u",
		}); err != nil {
			t.Fatalf("CreateDocComment %d: %v", i, err)
		}
	}

	got, err := ListDocComments(db, docID)
	testsupport.Must(t, err, "ListDocComments: %v", err)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	// Ascending order by created_at means insertion order is preserved.
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Body != want {
			t.Errorf("got[%d].Body = %q, want %q", i, got[i].Body, want)
		}
	}
}

func TestListDocComments_DocNotFound(t *testing.T) {
	db := mustInitAndMigrate(t)
	_, err := ListDocComments(db, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestListDocComments_EmptyIsNotNil pins ListDocComments' zero-result
// contract: a doc that exists but carries no comments returns a non-nil
// empty slice, not nil — distinct from the doc-not-found case above, which
// returns ErrNotFound and never reaches this path.
func TestListDocComments_EmptyIsNotNil(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "body")

	got, err := ListDocComments(db, docID)
	testsupport.Must(t, err, "ListDocComments: %v", err)
	if got == nil {
		t.Fatalf("ListDocComments returned nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

func TestGetDocComment_NotFound(t *testing.T) {
	db := mustInitAndMigrate(t)
	_, err := GetDocComment(db, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateDocComment_TouchesDocUpdatedAt(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "body")
	before, _ := GetDoc(db, docID)

	// Sleep is sufficient: RFC3339 has 1-second resolution. Use a body update
	// path instead to deterministically tick the timestamp.
	newBody := "v2"
	_, err := UpdateDoc(db, docID, DocUpdate{Body: &newBody, Author: "x"})
	testsupport.Must(t, err, "UpdateDoc: %v", err)

	if _, err := CreateDocComment(db, &model.DocComment{
		DocID: docID, Body: "c", Author: "u",
	}); err != nil {
		t.Fatalf("CreateDocComment: %v", err)
	}

	after, _ := GetDoc(db, docID)
	if !after.UpdatedAt.After(before.UpdatedAt) && !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("docs.updated_at went backwards: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestInsertDocCommentWithID_RoundTrip(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)

	c := &model.DocComment{ID: 77, DocID: docID, Body: "imported", Author: "ops"}
	inserted, err := InsertDocCommentWithID(tx, c)
	testsupport.Must(t, err, "InsertDocCommentWithID: %v", err)
	if !inserted {
		t.Error("inserted = false, want true")
	}

	// Second call with same ID — skipped.
	inserted2, err := InsertDocCommentWithID(tx, c)
	testsupport.Must(t, err, "InsertDocCommentWithID 2: %v", err)
	if inserted2 {
		t.Error("inserted2 = true, want false")
	}

	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	got, err := GetDocComment(db, 77)
	testsupport.Must(t, err, "GetDocComment(77): %v", err)
	if got.Body != "imported" {
		t.Errorf("Body = %q, want imported", got.Body)
	}
}
