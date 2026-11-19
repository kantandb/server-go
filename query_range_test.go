package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/itchyny/gojq"
)

func TestDenseRange(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rows []string
		used bool
		want string
	}{
		{"dense", []string{`"z"`, "100", "101", "102", "103"}, true, `[1,2,3]`},
		{"sparse", []string{"0", "0", "100", "0", "101", "0", "102"}, false, `[2,4,6]`},
		{"decimal", []string{"100.5", "100", "101", "102"}, false, `[0,1,2]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const database = "ranges"
			const program = `limit(3; (., inputs) | (select(.category >= 100) | .id)), halt`
			s := testStore(t)
			if err := s.createDB(database, indexDef{name: "category", path: "/category"}); err != nil {
				t.Fatal(err)
			}
			for i, value := range tc.rows {
				id := fmt.Sprintf("01950000-0000-7000-8000-%012d", i)
				body := fmt.Appendf(nil, `{"category":%s,"id":%d}`, value, i)
				if _, err := s.createDocWithID(database, id, body); err != nil {
					t.Fatal(err)
				}
			}

			query, err := gojq.Parse(program)
			if err != nil {
				t.Fatal(err)
			}
			plan := planQuery(query)
			if len(plan) != 1 || plan[0].probe == nil {
				t.Fatal("expected a range probe")
			}
			if err := s.withDocSnapshot(database, func(snapshot *pebble.Snapshot, key []byte) error {
				used, err := scanDenseRange(t.Context(), snapshot, database, key, plan[0].probe, func(func(context.Context) ([]byte, error)) error {
					return nil
				})
				if used != tc.used {
					t.Errorf("used primary prefix = %t, want %t", used, tc.used)
				}

				return err
			}); err != nil {
				t.Fatal(err)
			}

			got, err := s.queryArray(t.Context(), database, []byte(program))
			if err != nil || string(got) != tc.want {
				t.Fatalf("query = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}
