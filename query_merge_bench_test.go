package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// BenchmarkLimitedRange measures the cost of returning the first 100 range matches.
func BenchmarkLimitedRange(b *testing.B) {
	for _, tc := range []struct {
		categories int
		widths     []int
	}{
		{100, []int{8, 32, 33, 64, 90, 100}},
		{1000, []int{128, 256, 900, 1000}},
	} {
		b.Run(fmt.Sprintf("categories=%d", tc.categories), func(b *testing.B) {
			const database = "range"
			s, docs, order := seedRangeBench(b, database, tc.categories)
			for _, width := range tc.widths {
				b.Run(fmt.Sprintf("width=%d", width), func(b *testing.B) {
					benchRange(b, s, database, docs, order, tc.categories, tc.categories-width)
				})
			}
		})
	}
}

func seedRangeBench(b *testing.B, database string, categories int) (*store, [][]byte, []int) {
	b.Helper()

	s, err := openStore(b.TempDir(), testMasterKey)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.close(); err != nil {
			b.Error(err)
		}
	})
	if err := s.createDB(database, indexDef{name: "category", path: "/category"}); err != nil {
		b.Fatal(err)
	}

	docs := make([][]byte, 10_000)
	random := rand.NewChaCha8([32]byte{})
	var raw [768]byte
	for i := range docs {
		if _, err := random.Read(raw[:]); err != nil {
			b.Fatal(err)
		}
		payload := base64.RawStdEncoding.EncodeToString(raw[:])
		docs[i] = fmt.Appendf(nil, `{"category":%d,"payload":%q,"seed":%d}`, i%categories, payload, i)
	}
	ids, err := s.importDocs(b.Context(), database, docs, defaultBulkBatchBytes)
	if err != nil {
		b.Fatal(err)
	}

	// Import does not promise that document IDs sort in input order.
	order := make([]int, len(ids))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return strings.Compare(ids[a], ids[b]) })

	return s, docs, order
}

func benchRange(b *testing.B, s *store, database string, docs [][]byte, order []int, categories, lower int) {
	b.Helper()

	const limit = 100
	program := fmt.Appendf(nil, `limit(%d; (., inputs) | (select(.category >= %d))), halt`, limit, lower)
	want := make([][]byte, 0, limit)
	for _, i := range order {
		if i%categories >= lower {
			want = append(want, docs[i])
			if len(want) == limit {
				break
			}
		}
	}
	if len(want) != limit {
		b.Fatal("not enough matching documents")
	}

	// Check the exact output against ID order before timing.
	n := 0
	if err := s.runQuery(b.Context(), database, program, true, func(doc []byte) error {
		if n >= len(want) || !bytes.Equal(doc, want[n]) {
			return fmt.Errorf("unexpected document at position %d", n)
		}
		n++

		return nil
	}); err != nil || n != limit {
		b.Fatalf("query returned %d results, error: %v", n, err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if err := s.runQuery(b.Context(), database, program, true, func([]byte) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}
