package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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
