package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
)

const (
	indexTestIDA = "01950000-0000-7000-8000-000000000001"
	indexTestIDB = "01950000-0000-7000-8000-000000000002"
	indexTestIDC = "01950000-0000-7000-8000-000000000003"
	indexTestIDZ = "01950000-0000-7000-8000-000000000004"
	indexTestIDE = "01950000-0000-7000-8000-000000000005"
	indexTestIDF = "01950000-0000-7000-8000-000000000006"
	indexTestIDG = "01950000-0000-7000-8000-000000000007"
	indexTestIDH = "01950000-0000-7000-8000-000000000008"
)

func TestStoreOptionsEnableBloom(t *testing.T) {
	t.Parallel()

	options := storeOptions().EnsureDefaults()
	if got := options.Levels[0].FilterPolicy.Name(); got != "rocksdb.BuiltinBloomFilter" {
		t.Errorf("FilterPolicy.Name() = %q, want Bloom filter", got)
	}
}

func TestValidateIndexes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		defs []indexDef
	}{
		{name: "invalid name", defs: []indexDef{{name: "Bad", path: "/name"}}},
		{name: "empty path", defs: []indexDef{{name: "name", path: ""}}},
		{name: "relative path", defs: []indexDef{{name: "name", path: "name"}}},
		{name: "invalid escape", defs: []indexDef{{name: "name", path: "/a~2b"}}},
		{name: "duplicate", defs: []indexDef{{name: "name", path: "/a"}, {name: "name", path: "/b"}}},
		{name: "long path", defs: []indexDef{{name: "name", path: "/" + strings.Repeat("a", maxIndexPath)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := validateIndexes(tt.defs); !errors.Is(err, errInvalidIndex) {
				t.Errorf("validateIndexes() error = %v, want %v", err, errInvalidIndex)
			}
		})
	}

	tooMany := make([]indexDef, maxIndexes+1)
	if err := validateIndexes(tooMany); !errors.Is(err, errInvalidIndex) {
		t.Errorf("validateIndexes(too many) error = %v, want %v", err, errInvalidIndex)
	}
}

func TestIndexValueEncoding(t *testing.T) {
	t.Parallel()

	numbers := []json.Number{"1", "1.0", "1e0"}
	first, err := encodeIndexValue(numbers[0])
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	for _, number := range numbers[1:] {
		got, err := encodeIndexValue(number)
		if err != nil {
			t.Fatalf("encodeIndexValue(%q) error = %v", number, err)
		}
		if !bytes.Equal(got, first) {
			t.Errorf("encodeIndexValue(%q) = %x, want %x", number, got, first)
		}
	}
	integer, err := encodeIndexValue(1)
	if err != nil {
		t.Fatalf("encodeIndexValue(int) error = %v", err)
	}
	if !bytes.Equal(integer, first) {
		t.Errorf("encodeIndexValue(int) = %x, want %x", integer, first)
	}

	truth, err := encodeIndexValue(true)
	if err != nil {
		t.Fatalf("encodeIndexValue(true) error = %v", err)
	}
	text, err := encodeIndexValue("true")
	if err != nil {
		t.Fatalf("encodeIndexValue(string) error = %v", err)
	}
	if bytes.Equal(truth, text) {
		t.Error("boolean and string encodings collide")
	}

	for _, number := range []json.Number{"", "01", "+1", ".1", "1/2"} {
		if _, err := encodeIndexValue(number); !errors.Is(err, errInvalidIndexValue) {
			t.Errorf("encodeIndexValue(%q) error = %v, want %v", number, err, errInvalidIndexValue)
		}
	}
	if _, err := encodeIndexValue([]any{1}); !errors.Is(err, errInvalidIndexValue) {
		t.Errorf("encodeIndexValue(array) error = %v, want %v", err, errInvalidIndexValue)
	}
	if _, err := encodeIndexValue(strings.Repeat("x", maxIndexValue)); !errors.Is(err, errInvalidIndexValue) {
		t.Errorf("encodeIndexValue(large) error = %v, want %v", err, errInvalidIndexValue)
	}

	const scale = 6000
	coefficient := new(big.Int).Exp(big.NewInt(5), big.NewInt(scale), nil).String()
	finiteDecimal := "0." + strings.Repeat("0", scale-len(coefficient)) + coefficient
	if _, err := encodeIndexValue(json.Number(finiteDecimal)); !errors.Is(err, errInvalidIndexValue) {
		t.Errorf("encodeIndexValue(large finite decimal) error = %v, want %v", err, errInvalidIndexValue)
	}
}

func TestSortableNumberOrder(t *testing.T) {
	t.Parallel()

	literals := []string{
		"-1e1000", "-123456789012345678901234567890.5", "-2", "-1.01", "-1", "-0.1",
		"0", "0.0001", "0.1", "1", "1.01", "2", "123456789012345678901234567890.5", "1e1000",
	}
	random := rand.New(rand.NewSource(1))
	for range 1000 {
		coefficient := random.Int63n(1<<61) - 1<<60
		exponent := random.Intn(81) - 40
		literals = append(literals, strconv.FormatInt(coefficient, 10)+"e"+strconv.Itoa(exponent))
	}

	for _, leftLiteral := range literals {
		left := json.Number(leftLiteral)
		leftNumber, ok := new(big.Rat).SetString(leftLiteral)
		if !ok {
			t.Fatalf("SetString(%q) failed", leftLiteral)
		}
		leftEncoded, err := encodeIndexValue(left)
		if err != nil {
			t.Fatalf("encodeIndexValue(%q) error = %v", left, err)
		}

		for _, rightLiteral := range literals {
			rightNumber, ok := new(big.Rat).SetString(rightLiteral)
			if !ok {
				t.Fatalf("SetString(%q) failed", rightLiteral)
			}
			rightEncoded, err := encodeIndexValue(json.Number(rightLiteral))
			if err != nil {
				t.Fatalf("encodeIndexValue(%q) error = %v", rightLiteral, err)
			}
			if got, want := bytes.Compare(leftEncoded, rightEncoded), leftNumber.Cmp(rightNumber); sign(got) != sign(want) {
				t.Fatalf("bytes.Compare(%q, %q) = %d, numeric comparison = %d", leftLiteral, rightLiteral, got, want)
			}
		}
	}
}

func TestSortableStringOrder(t *testing.T) {
	t.Parallel()

	values := []string{"", "a", "a\x00", "a\x00b", "aa", "é"}
	for _, left := range values {
		leftEncoded, err := encodeIndexValue(left)
		if err != nil {
			t.Fatalf("encodeIndexValue(%q) error = %v", left, err)
		}
		for _, right := range values {
			rightEncoded, err := encodeIndexValue(right)
			if err != nil {
				t.Fatalf("encodeIndexValue(%q) error = %v", right, err)
			}
			if got, want := bytes.Compare(leftEncoded, rightEncoded), strings.Compare(left, right); sign(got) != sign(want) {
				t.Errorf("bytes.Compare(%q, %q) = %d, string comparison = %d", left, right, got, want)
			}
		}
	}

	value := strings.Repeat("\x00", maxIndexValue-1)
	if _, err := encodeIndexValue(value); err != nil {
		t.Errorf("encodeIndexValue(maximum escaped string) error = %v", err)
	}
}

func sign(value int) int {
	switch {
	case value < 0:
		return -1
	case value > 0:
		return 1
	default:
		return 0
	}
}

func TestIndexKeyParts(t *testing.T) {
	t.Parallel()

	parts := [][]byte{[]byte("db"), []byte("a\x00/b"), []byte("value")}
	key := []byte{0x04}
	for _, part := range parts {
		key = appendPart(key, part)
	}

	rest := key[1:]
	for _, want := range parts {
		got, next, ok := readPart(rest)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("readPart() = %q, %t, want %q, true", got, ok, want)
		}
		rest = next
	}
	if len(rest) != 0 {
		t.Errorf("readPart() remainder = %x, want empty", rest)
	}
}

func TestSortableIndexKeyOrder(t *testing.T) {
	t.Parallel()

	values := []any{json.Number("-2"), json.Number("1"), json.Number("1.5"), json.Number("10")}
	var previous []byte
	for _, value := range values {
		encoded, err := encodeIndexValue(value)
		if err != nil {
			t.Fatalf("encodeIndexValue(%v) error = %v", value, err)
		}
		key := indexKey("db", "value", encoded, indexTestIDA)
		if previous != nil && bytes.Compare(previous, key) >= 0 {
			t.Errorf("index key for %v does not follow previous value", value)
		}
		if !bytes.HasPrefix(key, indexValuePrefix("db", "value", encoded)) {
			t.Errorf("index key for %v lacks complete value prefix", value)
		}
		previous = key
	}

	encoded, err := encodeIndexValue("same")
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	if bytes.Compare(indexKey("db", "value", encoded, indexTestIDA), indexKey("db", "value", encoded, indexTestIDB)) >= 0 {
		t.Error("document IDs do not break equal-value ties")
	}
}

func TestStoreIndexesDocuments(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	defs := []indexDef{
		{name: "email", path: "/email"},
		{name: "active", path: "/active"},
		{name: "empty", path: "/empty"},
		{name: "escaped", path: "/profile/a~1b~0c"},
		{name: "item", path: "/items/0/sku"},
		{name: "object", path: "/profile"},
		{name: "array", path: "/items"},
	}
	if err := store.createDB("users", defs...); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	if err := store.createDB("other", defs...); err != nil {
		t.Fatalf("createDB(other) error = %v", err)
	}

	documents := map[string]string{
		indexTestIDC: `{"email":"other@example.com","active":false}`,
		indexTestIDA: `{"email":"alice@example.com","active":true,"empty":null,"profile":{"a/b~c":"yes"},"items":[{"sku":"first"}]}`,
		indexTestIDB: `{"email":"alice@example.com","active":true,"profile":{"a/b~c":"yes"}}`,
	}
	for id, document := range documents {
		if _, err := store.createDocWithID("users", id, []byte(document)); err != nil {
			t.Fatalf("createDoc(%q) error = %v", id, err)
		}
	}
	if _, err := store.createDocWithID("other", indexTestIDZ, []byte(`{"email":"alice@example.com"}`)); err != nil {
		t.Fatalf("createDoc(other) error = %v", err)
	}

	assertQuery(t, store, "users", "email", "alice@example.com", []string{indexTestIDA, indexTestIDB})
	assertQuery(t, store, "users", "active", true, []string{indexTestIDA, indexTestIDB})
	assertQuery(t, store, "users", "empty", nil, []string{indexTestIDA})
	assertQuery(t, store, "users", "escaped", "yes", []string{indexTestIDA, indexTestIDB})
	assertQuery(t, store, "users", "item", "first", []string{indexTestIDA})
	assertQuery(t, store, "users", "object", "ignored", nil)
	assertQuery(t, store, "users", "array", "ignored", nil)

	emailValue, err := encodeIndexValue("alice@example.com")
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	ids, more, err := store.queryDocs("users", "email", emailValue, 1, "")
	if err != nil {
		t.Fatalf("queryDocs() error = %v", err)
	}
	if !more {
		t.Error("queryDocs() more = false, want true")
	}
	if want := []string{indexTestIDA}; !slices.Equal(ids, want) {
		t.Errorf("queryDocs() = %v, want %v", ids, want)
	}

	ids, more, err = store.queryDocs("users", "email", emailValue, 1, indexTestIDA)
	if err != nil {
		t.Fatalf("queryDocs(cursor) error = %v", err)
	}
	if more {
		t.Error("queryDocs(cursor) more = true, want false")
	}
	if want := []string{indexTestIDB}; !slices.Equal(ids, want) {
		t.Errorf("queryDocs(cursor) = %v, want %v", ids, want)
	}

	if _, _, err := store.queryDocs("users", "missing", nil, 10, ""); !errors.Is(err, errIndexNotFound) {
		t.Errorf("queryDocs(missing index) error = %v, want %v", err, errIndexNotFound)
	}
	if _, _, err := store.queryDocs("missing", "email", nil, 10, ""); !errors.Is(err, errDBNotFound) {
		t.Errorf("queryDocs(missing database) error = %v, want %v", err, errDBNotFound)
	}
}

func TestStoreRangeQueries(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db", indexDef{name: "value", path: "/value"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}

	documents := map[string]string{
		indexTestIDA: `{"value":-2.5}`,
		indexTestIDB: `{"value":1.0}`,
		indexTestIDC: `{"value":123456789012345678901234567890}`,
		indexTestIDZ: `{"value":"a"}`,
		indexTestIDE: `{"value":"é"}`,
		indexTestIDF: `{}`,
		indexTestIDG: `{"value":{"nested":true}}`,
		indexTestIDH: `{"value":[1]}`,
	}
	for id, document := range documents {
		if _, err := store.createDocWithID("db", id, []byte(document)); err != nil {
			t.Fatalf("createDoc(%q) error = %v", id, err)
		}
	}

	tests := []struct {
		name  string
		op    cmpOp
		value any
		want  []string
	}{
		{name: "number less", op: cmpLT, value: json.Number("1"), want: []string{indexTestIDA}},
		{name: "number less or equal", op: cmpLE, value: json.Number("1e0"), want: []string{indexTestIDA, indexTestIDB}},
		{name: "number greater", op: cmpGT, value: json.Number("1.0"), want: []string{indexTestIDC}},
		{name: "number greater or equal", op: cmpGE, value: json.Number("1"), want: []string{indexTestIDB, indexTestIDC}},
		{name: "string less", op: cmpLT, value: "z", want: []string{indexTestIDZ}},
		{name: "string greater", op: cmpGT, value: "z", want: []string{indexTestIDE}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := encodeIndexValue(tt.value)
			if err != nil {
				t.Fatalf("encodeIndexValue() error = %v", err)
			}
			got, more, err := store.queryRangeDocs("db", "value", tt.op, encoded, 100, "")
			if err != nil {
				t.Fatalf("queryRangeDocs() error = %v", err)
			}
			if more {
				t.Error("queryRangeDocs() more = true, want false")
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("queryRangeDocs() = %v, want %v", got, tt.want)
			}
		})
	}

	one, err := encodeIndexValue(json.Number("1"))
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	ids, more, err := store.queryRangeDocs("db", "value", cmpGE, one, 1, "")
	if err != nil {
		t.Fatalf("queryRangeDocs(first page) error = %v", err)
	}
	if !more || !slices.Equal(ids, []string{indexTestIDB}) {
		t.Errorf("queryRangeDocs(first page) = %v, %t, want [%s], true", ids, more, indexTestIDB)
	}
	ids, more, err = store.queryRangeDocs("db", "value", cmpGE, one, 1, indexTestIDB)
	if err != nil {
		t.Fatalf("queryRangeDocs(cursor) error = %v", err)
	}
	if more || !slices.Equal(ids, []string{indexTestIDC}) {
		t.Errorf("queryRangeDocs(cursor) = %v, %t, want [%s], false", ids, more, indexTestIDC)
	}
	ids, more, err = store.queryRangeDocs("db", "value", cmpGE, one, 1, indexTestIDC)
	if err != nil {
		t.Fatalf("queryRangeDocs(final cursor) error = %v", err)
	}
	if more || len(ids) != 0 {
		t.Errorf("queryRangeDocs(final cursor) = %v, %t, want empty, false", ids, more)
	}

	if _, _, err := store.queryRangeDocs("db", "value", cmpLT, []byte{0x02}, 10, ""); !errors.Is(err, errInvalidIndexValue) {
		t.Errorf("queryRangeDocs(boolean) error = %v, want %v", err, errInvalidIndexValue)
	}

	page, err := store.queryRangePage(context.Background(), "db", "value", cmpGE, one, 1, nil, "")
	if err != nil {
		t.Fatalf("queryRangePage() error = %v", err)
	}
	if !page.more || !slices.Equal(page.ids, []string{indexTestIDB}) {
		t.Fatalf("queryRangePage() = %+v, want first matching document", page)
	}

	doc, err := store.getDoc("db", indexTestIDB)
	if err != nil {
		t.Fatalf("getDoc() error = %v", err)
	}
	if err := store.deleteDoc("db", indexTestIDB, matchCond{set: true, revision: doc.revision}); err != nil {
		t.Fatalf("deleteDoc() error = %v", err)
	}
	if _, err := store.createDocWithID("db", indexTestIDB, []byte(`{"value":2}`)); err != nil {
		t.Fatalf("createDoc(reused cursor ID) error = %v", err)
	}
	page, err = store.queryRangePage(context.Background(), "db", "value", cmpGE, one, 100, page.lastValue, indexTestIDB)
	if err != nil {
		t.Fatalf("queryRangePage(mutated cursor entry) error = %v", err)
	}
	if page.more || !slices.Equal(page.ids, []string{indexTestIDB, indexTestIDC}) {
		t.Errorf("queryRangePage(mutated cursor entry) = %+v, want [%s %s]", page, indexTestIDB, indexTestIDC)
	}
}

func TestStoreRangeQueryCorruption(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db", indexDef{name: "value", path: "/value"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	encoded, err := encodeIndexValue(json.Number("1"))
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}

	entry := indexKey("db", "value", encoded, "bad")
	if err := store.db.Set(entry, nil, pebble.Sync); err != nil {
		t.Fatalf("Set(index entry) error = %v", err)
	}
	if _, _, err := store.queryRangeDocs("db", "value", cmpGE, encoded, 10, ""); !errors.Is(err, errCorruptData) {
		t.Errorf("queryRangeDocs(index entry) error = %v, want %v", err, errCorruptData)
	}
	if err := store.db.Delete(entry, pebble.Sync); err != nil {
		t.Fatalf("Delete(index entry) error = %v", err)
	}

	entry = indexKey("db", "value", encoded, indexTestIDA)
	if err := store.db.Set(entry, nil, pebble.Sync); err != nil {
		t.Fatalf("Set(missing document entry) error = %v", err)
	}
	if _, _, err := store.queryRangeDocs("db", "value", cmpGE, encoded, 10, ""); !errors.Is(err, errCorruptData) {
		t.Errorf("queryRangeDocs(missing document) error = %v, want %v", err, errCorruptData)
	}

	if err := store.db.Set(indexDefKey("db", "bad"), []byte{0xff}, pebble.Sync); err != nil {
		t.Fatalf("Set(index definition) error = %v", err)
	}
	if _, _, err := store.queryRangeDocs("db", "value", cmpGE, encoded, 10, ""); !errors.Is(err, errCorruptData) {
		t.Errorf("queryRangeDocs(index definition) error = %v, want %v", err, errCorruptData)
	}
}

func TestStoreQueryCancellation(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db", indexDef{name: "value", path: "/value"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	if _, err := store.createDocWithID("db", indexTestIDA, []byte(`{"value":1}`)); err != nil {
		t.Fatalf("createDoc() error = %v", err)
	}
	encoded, err := encodeIndexValue(json.Number("0"))
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store.queryRangePage(ctx, "db", "value", cmpGT, encoded, 10, nil, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("queryRangePage() error = %v, want %v", err, context.Canceled)
	}
	exact, err := encodeIndexValue(json.Number("1"))
	if err != nil {
		t.Fatalf("encodeIndexValue(exact) error = %v", err)
	}
	if _, _, err := store.queryDocsCtx(ctx, "db", "value", exact, 10, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("queryDocsCtx() error = %v, want %v", err, context.Canceled)
	}
}

func TestStoreIgnoresOldIndexLayout(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db", indexDef{name: "value", path: "/value"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	encoded, err := encodeIndexValue(json.Number("1"))
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	oldKey := appendPart(indexDataPrefix("db"), []byte("value"))
	oldKey = appendPart(oldKey, encoded)
	oldKey = append(oldKey, indexTestIDA...)
	if err := store.db.Set(oldKey, nil, pebble.Sync); err != nil {
		t.Fatalf("Set(old index entry) error = %v", err)
	}

	ids, more, err := store.queryDocs("db", "value", encoded, 10, "")
	if err != nil {
		t.Fatalf("queryDocs() error = %v", err)
	}
	if more || len(ids) != 0 {
		t.Errorf("queryDocs() = %v, %t, want empty, false", ids, more)
	}
}

func TestStoreRejectsLargeIndexedValue(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db", indexDef{name: "value", path: "/value"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}

	body, err := json.Marshal(map[string]string{"value": strings.Repeat("x", maxIndexValue)})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if _, err := store.createDocWithID("db", "id", body); !errors.Is(err, errInvalidIndexValue) {
		t.Fatalf("createDoc() error = %v, want %v", err, errInvalidIndexValue)
	}
	if _, err := store.getDoc("db", "id"); !errors.Is(err, errDocNotFound) {
		t.Errorf("getDoc() error = %v, want %v", err, errDocNotFound)
	}
}

func TestStoreMaintainsIndexes(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db", indexDef{name: "name", path: "/name"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}

	rev, err := store.createDocWithID("db", indexTestIDA, []byte(`{"name":"first"}`))
	if err != nil {
		t.Fatalf("createDoc() error = %v", err)
	}
	if _, err := store.replaceDoc("db", indexTestIDA, []byte(`{"name":"blocked"}`), matchCond{set: true, revision: revision{1}}); !errors.Is(err, errPreconditionFailed) {
		t.Fatalf("replaceDoc(stale) error = %v, want %v", err, errPreconditionFailed)
	}
	assertQuery(t, store, "db", "name", "first", []string{indexTestIDA})
	assertIndexEntry(t, store, "db", "name", "first", indexTestIDA, true)

	newRev, err := store.replaceDoc("db", indexTestIDA, []byte(`{"name":"second"}`), matchCond{set: true, revision: rev})
	if err != nil {
		t.Fatalf("replaceDoc() error = %v", err)
	}
	assertQuery(t, store, "db", "name", "first", nil)
	assertQuery(t, store, "db", "name", "second", []string{indexTestIDA})
	assertIndexEntry(t, store, "db", "name", "first", indexTestIDA, false)
	assertIndexEntry(t, store, "db", "name", "second", indexTestIDA, true)

	doc, err := store.patchDoc("db", indexTestIDA, matchCond{set: true, revision: newRev}, func([]byte) ([]byte, error) {
		return []byte(`{"name":"third"}`), nil
	})
	if err != nil {
		t.Fatalf("patchDoc() error = %v", err)
	}
	assertQuery(t, store, "db", "name", "second", nil)
	assertQuery(t, store, "db", "name", "third", []string{indexTestIDA})
	assertIndexEntry(t, store, "db", "name", "second", indexTestIDA, false)
	assertIndexEntry(t, store, "db", "name", "third", indexTestIDA, true)

	if err := store.deleteDoc("db", indexTestIDA, matchCond{set: true, revision: doc.revision}); err != nil {
		t.Fatalf("deleteDoc() error = %v", err)
	}
	assertQuery(t, store, "db", "name", "third", nil)
	assertIndexEntry(t, store, "db", "name", "third", indexTestIDA, false)
}

func TestStorePersistsIndexes(t *testing.T) {
	t.Parallel()

	path := t.TempDir()
	store, err := openStore(path, testMasterKey)
	if err != nil {
		t.Fatalf("openStore() error = %v", err)
	}
	if err := store.createDB("db", indexDef{name: "number", path: "/number"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	if _, err := store.createDocWithID("db", indexTestIDA, []byte(`{"number":1.0}`)); err != nil {
		t.Fatalf("createDoc() error = %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}

	store, err = openStore(path, testMasterKey)
	if err != nil {
		t.Fatalf("reopen store error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Errorf("close() error = %v", err)
		}
	})

	defs, err := store.indexes("db")
	if err != nil {
		t.Fatalf("indexes() error = %v", err)
	}
	if len(defs) != 1 || defs[0].name != "number" || defs[0].path != "/number" {
		t.Errorf("indexes() = %+v, want number definition", defs)
	}
	assertQuery(t, store, "db", "number", json.Number("1e0"), []string{indexTestIDA})
}

func TestDeleteDBDeletesIndexes(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db", indexDef{name: "name", path: "/name"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	if _, err := store.createDocWithID("db", "id", []byte(`{"name":"value"}`)); err != nil {
		t.Fatalf("createDoc() error = %v", err)
	}
	if err := store.deleteDB("db"); err != nil {
		t.Fatalf("deleteDB() error = %v", err)
	}

	for _, prefix := range [][]byte{indexDefPrefix("db"), indexDataPrefix("db")} {
		iter, err := store.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
		if err != nil {
			t.Fatalf("NewIter() error = %v", err)
		}
		if iter.First() {
			t.Errorf("index key remains after database deletion: %x", iter.Key())
		}
		if err := iter.Close(); err != nil {
			t.Fatalf("iterator close error = %v", err)
		}
	}
}

func TestStoreRejectsCorruptIndex(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("db", indexDef{name: "name", path: "/name"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	if err := store.db.Set(indexDefKey("db", "bad"), []byte{0xff}, pebble.Sync); err != nil {
		t.Fatalf("Set(definition) error = %v", err)
	}
	if _, err := store.indexes("db"); !errors.Is(err, errCorruptData) {
		t.Errorf("indexes() error = %v, want %v", err, errCorruptData)
	}

	if err := store.db.Delete(indexDefKey("db", "bad"), pebble.Sync); err != nil {
		t.Fatalf("Delete(definition) error = %v", err)
	}
	value, err := encodeIndexValue("value")
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	entry := indexKey("db", "name", value, indexTestIDA)
	if err := store.db.Set(entry, []byte{1}, pebble.Sync); err != nil {
		t.Fatalf("Set(entry) error = %v", err)
	}
	if _, _, err := store.queryDocs("db", "name", value, 10, ""); !errors.Is(err, errCorruptData) {
		t.Errorf("queryDocs(value) error = %v, want %v", err, errCorruptData)
	}
	if err := store.db.Delete(entry, pebble.Sync); err != nil {
		t.Fatalf("Delete(entry) error = %v", err)
	}

	entry = indexKey("db", "name", value, "bad")
	if err := store.db.Set(entry, nil, pebble.Sync); err != nil {
		t.Fatalf("Set(malformed ID) error = %v", err)
	}
	if _, _, err := store.queryDocs("db", "name", value, 10, ""); !errors.Is(err, errCorruptData) {
		t.Errorf("queryDocs(malformed ID) error = %v, want %v", err, errCorruptData)
	}
	if err := store.db.Delete(entry, pebble.Sync); err != nil {
		t.Fatalf("Delete(malformed ID) error = %v", err)
	}

	entry = indexKey("db", "name", value, indexTestIDA)
	if err := store.db.Set(entry, nil, pebble.Sync); err != nil {
		t.Fatalf("Set(missing document) error = %v", err)
	}
	if _, _, err := store.queryDocs("db", "name", value, 10, ""); !errors.Is(err, errCorruptData) {
		t.Errorf("queryDocs(missing document) error = %v, want %v", err, errCorruptData)
	}
}

func assertIndexEntry(t *testing.T, store *store, database, index string, value any, id string, want bool) {
	t.Helper()

	encoded, err := encodeIndexValue(value)
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	got, err := store.has(indexKey(database, index, encoded, id))
	if err != nil {
		t.Fatalf("has(index entry) error = %v", err)
	}
	if got != want {
		t.Errorf("index entry exists = %t, want %t", got, want)
	}
}

func assertQuery(t *testing.T, store *store, database, index string, value any, want []string) {
	t.Helper()

	encoded, err := encodeIndexValue(value)
	if err != nil {
		t.Fatalf("encodeIndexValue() error = %v", err)
	}
	got, more, err := store.queryDocs(database, index, encoded, 100, "")
	if err != nil {
		t.Fatalf("queryDocs(%q, %q) error = %v", database, index, err)
	}
	if more {
		t.Errorf("queryDocs(%q, %q) more = true, want false", database, index)
	}
	if !slices.Equal(got, want) {
		t.Errorf("queryDocs(%q, %q) = %v, want %v", database, index, got, want)
	}
}
