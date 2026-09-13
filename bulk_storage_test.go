package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
)

func TestExportDocs(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db"); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}

	documents := []struct {
		id   string
		body string
	}{
		{id: "01950000-0000-7000-8000-000000000002", body: `{"name":"second"}`},
		{id: "01950000-0000-7000-8000-000000000001", body: `{"name":"first"}`},
	}
	for _, document := range documents {
		if _, err := store.createDocWithID("db", document.id, []byte(document.body)); err != nil {
			t.Fatalf("createDocWithID() error = %v", err)
		}
	}

	var got []string
	err := store.exportDocs(t.Context(), "db", func(document []byte) error {
		got = append(got, string(document))

		return nil
	})
	if err != nil {
		t.Fatalf("exportDocs() error = %v", err)
	}
	if want := []string{documents[1].body, documents[0].body}; !slices.Equal(got, want) {
		t.Errorf("exportDocs() = %v, want %v", got, want)
	}
}

func TestExportDocsSnapshot(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db"); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	if _, err := store.createDocWithID("db", "01950000-0000-7000-8000-000000000001", []byte(`{"value":1}`)); err != nil {
		t.Fatalf("createDocWithID() error = %v", err)
	}

	var got []string
	err := store.exportDocs(t.Context(), "db", func(document []byte) error {
		got = append(got, string(document))
		if len(got) == 1 {
			_, err := store.createDocWithID("db", "01950000-0000-7000-8000-000000000002", []byte(`{"value":2}`))

			return err
		}

		return nil
	})
	if err != nil {
		t.Fatalf("exportDocs() error = %v", err)
	}
	if want := []string{`{"value":1}`}; !slices.Equal(got, want) {
		t.Errorf("exportDocs() = %v, want %v", got, want)
	}
}

func TestExportDocsErrors(t *testing.T) {
	t.Parallel()

	t.Run("empty database", func(t *testing.T) {
		t.Parallel()

		store := testStore(t)
		if err := store.createDB("db"); err != nil {
			t.Fatalf("createDB() error = %v", err)
		}
		if err := store.exportDocs(t.Context(), "db", func([]byte) error {
			t.Fatal("empty database yielded a document")

			return nil
		}); err != nil {
			t.Fatalf("exportDocs() error = %v", err)
		}
	})

	t.Run("missing database", func(t *testing.T) {
		t.Parallel()

		store := testStore(t)
		err := store.exportDocs(t.Context(), "missing", func([]byte) error { return nil })
		if !errors.Is(err, errDBNotFound) {
			t.Fatalf("exportDocs() error = %v, want %v", err, errDBNotFound)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		t.Parallel()

		store := testStore(t)
		if err := store.createDB("db"); err != nil {
			t.Fatalf("createDB() error = %v", err)
		}
		if _, err := store.createDocWithID("db", "01950000-0000-7000-8000-000000000001", []byte(`{}`)); err != nil {
			t.Fatalf("createDocWithID() error = %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := store.exportDocs(ctx, "db", func([]byte) error { return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("exportDocs() error = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		t.Parallel()

		store := testStore(t)
		if err := store.createDB("db"); err != nil {
			t.Fatalf("createDB() error = %v", err)
		}
		if _, err := store.createDocWithID("db", "bad", []byte(`{}`)); err != nil {
			t.Fatalf("createDocWithID() error = %v", err)
		}

		err := store.exportDocs(t.Context(), "db", func([]byte) error { return nil })
		if !errors.Is(err, errCorruptData) {
			t.Fatalf("exportDocs() error = %v, want %v", err, errCorruptData)
		}
	})

	t.Run("corrupt record", func(t *testing.T) {
		t.Parallel()

		store := testStore(t)
		if err := store.createDB("db"); err != nil {
			t.Fatalf("createDB() error = %v", err)
		}
		id := "01950000-0000-7000-8000-000000000001"
		if err := store.db.Set(docKey("db", id), []byte{0xff}, pebble.Sync); err != nil {
			t.Fatalf("Set() error = %v", err)
		}

		err := store.exportDocs(t.Context(), "db", func([]byte) error { return nil })
		if !errors.Is(err, errCorruptData) {
			t.Fatalf("exportDocs() error = %v, want %v", err, errCorruptData)
		}
	})
}

func TestExportBlocksDeletion(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db"); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	if _, err := store.createDocWithID("db", "01950000-0000-7000-8000-000000000001", []byte(`{}`)); err != nil {
		t.Fatalf("createDocWithID() error = %v", err)
	}

	deleted := make(chan error, 1)
	err := store.exportDocs(t.Context(), "db", func([]byte) error {
		go func() { deleted <- store.deleteDB("db") }()

		select {
		case err := <-deleted:
			t.Fatalf("deleteDB() completed during export: %v", err)
		case <-time.After(20 * time.Millisecond):
		}

		return nil
	})
	if err != nil {
		t.Fatalf("exportDocs() error = %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("deleteDB() error = %v", err)
	}
}

func TestImportDocs(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db", indexDef{name: "name", path: "/name"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}

	documents := byteRows(`{"name":"first"}`, `{"id":"data","name":"second"}`)
	ids, err := store.importDocs(t.Context(), "db", documents, defaultBulkBatchBytes)
	if err != nil {
		t.Fatalf("importDocs() error = %v", err)
	}
	if len(ids) != len(documents) || ids[0] == ids[1] {
		t.Fatalf("importDocs() IDs = %v", ids)
	}

	for i, id := range ids {
		if err := validateID(id); err != nil {
			t.Errorf("ID %q is invalid: %v", id, err)
		}

		doc, err := store.getDoc("db", id)
		if err != nil {
			t.Fatalf("getDoc(%q) error = %v", id, err)
		}
		if string(doc.json) != string(documents[i]) {
			t.Errorf("getDoc(%q) = %s, want %s", id, doc.json, documents[i])
		}
		if doc.revision == (revision{}) {
			t.Errorf("getDoc(%q) has zero revision", id)
		}
	}

	assertQuery(t, store, "db", "name", "first", ids[:1])
	assertQuery(t, store, "db", "name", "second", ids[1:])
}

func TestImportDocsIsAtomic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		docs     [][]byte
		maxBatch int
		ctx      func() context.Context
		wantErr  error
	}{
		{
			name:     "invalid index",
			docs:     byteRows(`{"value":"ok"}`, `{"value":"`+strings.Repeat("a", maxIndexValue+1)+`"}`),
			maxBatch: defaultBulkBatchBytes,
			ctx:      context.Background,
			wantErr:  errInvalidIndexValue,
		},
		{
			name:     "batch limit",
			docs:     byteRows(`{"value":"ok"}`),
			maxBatch: 1,
			ctx:      context.Background,
			wantErr:  errBulkLimit,
		},
		{
			name:     "cancellation",
			docs:     byteRows(`{"value":"ok"}`),
			maxBatch: defaultBulkBatchBytes,
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				return ctx
			},
			wantErr: context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := testStore(t)
			if err := store.createDB("db", indexDef{name: "value", path: "/value"}); err != nil {
				t.Fatalf("createDB() error = %v", err)
			}

			ids, err := store.importDocs(tt.ctx(), "db", tt.docs, tt.maxBatch)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("importDocs() error = %v, want %v", err, tt.wantErr)
			}
			if ids != nil {
				t.Errorf("importDocs() IDs = %v, want nil", ids)
			}

			stored, _, err := store.listDocs("db", 10, "")
			if err != nil {
				t.Fatalf("listDocs() error = %v", err)
			}
			if len(stored) != 0 {
				t.Errorf("failed import stored documents %v", stored)
			}
		})
	}
}

func TestImportDocsMissingDatabase(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if _, err := store.importDocs(t.Context(), "missing", byteRows(`{}`), defaultBulkBatchBytes); !errors.Is(err, errDBNotFound) {
		t.Fatalf("importDocs() error = %v, want %v", err, errDBNotFound)
	}
}
