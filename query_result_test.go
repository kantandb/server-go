package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// idPage projects records for existing match/order tests and checks their metadata.
type idPage struct {
	Documents []string `json:"documents"`
	Cursor    string   `json:"cursor"`
}

func (p *idPage) UnmarshalJSON(body []byte) error {
	var page docList
	if err := json.Unmarshal(body, &page); err != nil {
		return err
	}
	if page.Documents == nil {
		return errors.New("query response is missing documents")
	}
	p.Documents = make([]string, 0, len(page.Documents))
	p.Cursor = page.Cursor
	for _, doc := range page.Documents {
		rev, err := hex.DecodeString(doc.Rev)
		if err != nil || len(rev) != 16 || validateID(doc.ID) != nil || !json.Valid(doc.Doc) || len(doc.Doc) == 0 || doc.Doc[0] != '{' {
			return fmt.Errorf("invalid result record: %+v", doc)
		}
		p.Documents = append(p.Documents, doc.ID)
	}

	return nil
}

func checkQueryIDs(t *testing.T, res *http.Response, status int, want string) {
	t.Helper()

	body := readResponse(t, res)
	if res.StatusCode != status {
		t.Fatalf("status = %d, want %d; body = %s", res.StatusCode, status, body)
	}
	var page idPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("ID page = %s, want %s", got, want)
	}
}

func TestQueryFullDocumentsHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"stock","indices":[{"name":"age","path":"/age"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"stock","indices":[{"name":"age","path":"/age"}]}`)
	id := createQueryDoc(t, server, "/db/stock", `{"id":"app-id","rev":"app-rev","age":123456789012345678901234567890}`)
	res = sendRequest(t, server, http.MethodGet, "/db/stock/"+id, "", "")
	etag := res.Header.Get("ETag")
	stored := readResponse(t, res)

	for _, body := range []string{
		`{}`, `{"limit":1}`,
		`{"index":"age","value":123456789012345678901234567890}`,
		`{"path":"$.age","op":"ge","value":1}`,
		`{"path":"$.*","value":"app-id"}`,
	} {
		res = sendRequest(t, server, queryMethod, "/db/stock", body, "application/json")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("QUERY %s: status = %d, body = %s", body, res.StatusCode, readResponse(t, res))
		}
		var page docList
		if err := json.Unmarshal([]byte(readResponse(t, res)), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Documents) != 1 || page.Cursor != "" {
			t.Fatalf("page = %+v", page)
		}
		doc := page.Documents[0]
		if doc.ID != id || `"`+doc.Rev+`"` != etag || string(doc.Doc) != stored {
			t.Errorf("record = %+v, want id %s, ETag %s, body %s", doc, id, etag, stored)
		}
	}
}

func TestDatabaseInfoHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"empty"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"empty"}`)
	res = sendRequest(t, server, http.MethodGet, "/db/empty", "", "")
	checkResponse(t, res, http.StatusOK, `{"name":"empty","indices":[],"doc_count":0}`)
	res = sendRequest(t, server, queryMethod, "/db/empty", `{}`, "application/json")
	checkResponse(t, res, http.StatusOK, `{"documents":[],"cursor":""}`)

	res = sendRequest(t, server, http.MethodPost, "/db", `{"name":"stock","indices":[{"name":"sku","path":"/sku"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"stock","indices":[{"name":"sku","path":"/sku"}]}`)
	id := createQueryDoc(t, server, "/db/stock", `{"sku":"a"}`)
	createQueryDoc(t, server, "/db/stock", `{"sku":"a"}`)
	res = sendRequest(t, server, http.MethodGet, "/db/stock", "", "")
	checkResponse(t, res, http.StatusOK, `{"name":"stock","indices":[{"name":"sku","path":"/sku"}],"doc_count":2}`)
	res = sendRequest(t, server, http.MethodDelete, "/db/stock/"+id, "", "")
	checkResponse(t, res, http.StatusNoContent, "")
	res = sendRequest(t, server, http.MethodGet, "/db/stock", "", "")
	checkResponse(t, res, http.StatusOK, `{"name":"stock","indices":[{"name":"sku","path":"/sku"}],"doc_count":1}`)
}

func TestResultSnapshotAndBytes(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("users", indexDef{name: "age", path: "/age"}); err != nil {
		t.Fatal(err)
	}
	first, _, err := store.createDoc("users", []byte(`{"age":10}`))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.createDoc("users", []byte(`{"age":20}`))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.databaseKey("users")
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	snapshot := store.db.NewSnapshot()
	t.Cleanup(func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := store.replaceDoc("users", first, []byte(`{"age":99}`), matchCond{}); err != nil {
		t.Fatal(err)
	}
	if err := store.deleteDoc("users", second, matchCond{}); err != nil {
		t.Fatal(err)
	}

	for _, plan := range []pathPlan{{}, {index: "age"}} {
		query := pathQuery{op: cmpGE, value: 0, limit: 10}
		encoded := valueForCursor(t, 0)
		page, err := queryResultPage(context.Background(), snapshot, key, "users", plan, query, encoded, pathCursor{}, 1, maxScanDocs)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.docs) != 1 || page.docs[0].ID != first || !bytes.Equal(page.docs[0].Doc, []byte(`{"age":10}`)) || !page.more {
			t.Fatalf("first snapshot page = %+v", page)
		}
		after := pathCursor{id: page.lastID, lastValue: page.lastValue}
		page, err = queryResultPage(context.Background(), snapshot, key, "users", plan, query, encoded, after, 1, maxScanDocs)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.docs) != 1 || page.docs[0].ID != second || page.more {
			t.Fatalf("second snapshot page = %+v", page)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.executePathQuery(ctx, "users", pathPlan{}, pathQuery{limit: 10}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled query = %v", err)
	}
	if _, err := store.databaseInfo(ctx, "users"); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled metadata = %v", err)
	}
}
