package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
)

func TestQueryReadsWholeSnapshot(t *testing.T) {
	t.Parallel()

	s := queryStore(t)
	key := docKey("users", indexTestIDB)
	if err := s.db.Set(key, []byte("bad record"), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	body, err := s.queryArray(t.Context(), "users", []byte("42"))
	if body != nil || !errors.Is(err, errCorruptData) {
		t.Fatalf("runQuery() = %s, %v; want corruption and no partial output", body, err)
	}
}

func TestQueryInputLimits(t *testing.T) {
	t.Parallel()

	for _, limit := range []bool{true, false} {
		input := &queryInput{
			ctx: t.Context(), docs: maxScanDocs, bytes: maxScanBytes, limit: limit,
			read: func(context.Context) (*queryDocument, error) {
				return &queryDocument{storedDoc: storedDoc{json: []byte(`{"age":25}`)}}, nil
			},
		}
		value, ok := input.Next()
		if !ok || limit && !errors.Is(input.err, errQueryLimit) || !limit && input.err != nil {
			t.Errorf("Next() with limit %t = %v, %t, err %v", limit, value, ok, input.err)
		}
	}
}

func TestQueryDeadline(t *testing.T) {
	t.Parallel()

	s := queryStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	body, err := s.queryArray(ctx, "users", []byte(`def loop: loop; loop`))
	if body != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runQuery() = %s, %v; want deadline exceeded", body, err)
	}
}

func queryStore(t *testing.T) *store {
	t.Helper()

	s := testStore(t)
	if err := s.createDB("users"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createDocWithID("users", indexTestIDA, []byte(`{"age":25}`)); err != nil {
		t.Fatal(err)
	}

	return s
}
