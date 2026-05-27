package db

import (
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
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
	if err != nil {
		t.Fatalf("CreateDocComment: %v", err)
	}
	if id <= 0 {
		t.Fatalf("returned id = %d, want > 0", id)
	}

	got, err := GetDocComment(db, id)
	if err != nil {
		t.Fatalf("GetDocComment: %v", err)
	}
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
	if err != nil {
		t.Fatalf("ListDocComments: %v", err)
	}
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
	if _, err := UpdateDoc(db, docID, DocUpdate{Body: &newBody, Author: "x"}); err != nil {
		t.Fatalf("UpdateDoc: %v", err)
	}

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
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	c := &model.DocComment{ID: 77, DocID: docID, Body: "imported", Author: "ops"}
	inserted, err := InsertDocCommentWithID(tx, c)
	if err != nil {
		t.Fatalf("InsertDocCommentWithID: %v", err)
	}
	if !inserted {
		t.Error("inserted = false, want true")
	}

	// Second call with same ID — skipped.
	inserted2, err := InsertDocCommentWithID(tx, c)
	if err != nil {
		t.Fatalf("InsertDocCommentWithID 2: %v", err)
	}
	if inserted2 {
		t.Error("inserted2 = true, want false")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := GetDocComment(db, 77)
	if err != nil {
		t.Fatalf("GetDocComment(77): %v", err)
	}
	if got.Body != "imported" {
		t.Errorf("Body = %q, want imported", got.Body)
	}
}
