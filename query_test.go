package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/theory/jsonpath"
)

func TestJSONPathPointer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path    string
		pointer string
		ok      bool
	}{
		{path: `$.profile.age`, pointer: "/profile/age", ok: true},
		{path: `$['a/b']['~x']`, pointer: "/a~1b/~0x", ok: true},
		{path: `$`, ok: false},
		{path: `$.items[0]`, ok: false},
		{path: `$.items['0']`, ok: false},
		{path: `$.items[-1]`, ok: false},
		{path: `$.items[*]`, ok: false},
		{path: `$..name`, ok: false},
		{path: `$['a','b']`, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			path, err := jsonpath.Parse(tt.path)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			pointer, ok := jsonPathPointer(path)
			if pointer != tt.pointer || ok != tt.ok {
				t.Errorf("jsonPathPointer() = (%q, %t), want (%q, %t)", pointer, ok, tt.pointer, tt.ok)
			}
		})
	}
}

func TestValidatePathText(t *testing.T) {
	t.Parallel()

	if err := validatePathText(`$.items[?(@.age >= 18)]`); err != nil {
		t.Errorf("validatePathText() error = %v", err)
	}
	if err := validatePathText("$" + strings.Repeat("(", maxPathDepth+1)); !errors.Is(err, errInvalidPath) {
		t.Errorf("deep path error = %v, want invalid path", err)
	}
	if err := validatePathText("$" + strings.Repeat(".a", maxQueryPath)); !errors.Is(err, errInvalidPath) {
		t.Errorf("long path error = %v, want invalid path", err)
	}
}

func TestSafePath(t *testing.T) {
	t.Parallel()

	for _, path := range []string{`$..items[*].name`, `$.items[?(@.name == "a,b")]`, `$.items[?match(@.name, "a,b")]`} {
		if !safePath(path) {
			t.Errorf("safePath(%q) = false", path)
		}
	}
	for _, path := range []string{`$..*..*`, `$["a","a"]`, `$[?(@["a","a"])]`} {
		if safePath(path) {
			t.Errorf("safePath(%q) = true", path)
		}
	}
}

func TestPlanPathQuery(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("scores", indexDef{name: "first", path: "/score"}, indexDef{name: "second", path: "/score"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}

	plan, err := store.planPathQuery("scores", `$.score`)
	if err != nil {
		t.Fatalf("planPathQuery() error = %v", err)
	}
	if plan.text != `$["score"]` || plan.index != "first" {
		t.Errorf("plan = %+v, want canonical path and first matching index", plan)
	}

	plan, err = store.planPathQuery("scores", `$..score`)
	if err != nil {
		t.Fatalf("planPathQuery() error = %v", err)
	}
	if plan.index != "" {
		t.Errorf("descendant index = %q, want scan", plan.index)
	}

	store = testStore(t)
	if err := store.createDB("items", indexDef{name: "sku", path: "/items/0/sku"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	for _, path := range []string{`$.items[0].sku`, `$.items['0'].sku`} {
		plan, err := store.planPathQuery("items", path)
		if err != nil {
			t.Fatalf("planPathQuery(%q) error = %v", path, err)
		}
		if plan.index != "" {
			t.Errorf("planPathQuery(%q) index = %q, want scan", path, plan.index)
		}
	}
}

func TestPathMatchLimits(t *testing.T) {
	t.Parallel()

	path := jsonpath.MustParse(`$[*]`)
	expected, err := encodeIndexValue(1)
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	if _, err := pathMatches(context.Background(), path, []any{1, 2}, cmpEq, expected, 2); !errors.Is(err, errQueryLimit) {
		t.Errorf("pathMatches() error = %v, want query limit", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pathMatches(ctx, path, []any{1}, cmpEq, expected, 10); !errors.Is(err, context.Canceled) {
		t.Errorf("pathMatches() error = %v, want context canceled", err)
	}
}

func TestPathQueryScanLimits(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("users"); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	for _, id := range []string{indexTestIDA, indexTestIDB} {
		if _, err := store.createDocWithID("users", id, []byte(`{"age":10}`)); err != nil {
			t.Fatalf("createDoc() error = %v", err)
		}
	}
	path := jsonpath.MustParse(`$.missing`)

	page, err := store.queryPathDocs(context.Background(), "users", path, cmpEq, true, 10, 1, "")
	if err != nil {
		t.Fatalf("queryPathDocs() error = %v", err)
	}
	if len(page.ids) != 0 || !page.more || page.lastID != indexTestIDA {
		t.Errorf("page = %+v, want bounded scan cursor at first ID", page)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.queryPathDocs(ctx, "users", path, cmpEq, true, 10, 1, "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled query error = %v, want context canceled", err)
	}
}
