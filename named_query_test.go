package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestNamedQueryHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"stock","indices":[{"name":"sku","path":"/items/0/sku"},{"name":"other","path":"/items/0/sku"}]}`, "application/json")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create database status = %d", res.StatusCode)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}

	// Reverse value order proves named queries use the index, not an ID scan.
	objectID := createQueryDoc(t, server, "/db/stock", `{"items":{"0":{"sku":"z"}}}`)
	arrayID := createQueryDoc(t, server, "/db/stock", `{"items":[{"sku":"a"}]}`)
	res = sendRequest(t, server, queryMethod, "/db/stock", `{"index":"sku","op":"ge","value":"a","limit":1}`, "application/json")
	var page idPage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || len(page.Documents) != 1 || page.Documents[0] != arrayID || page.Cursor == "" {
		t.Fatalf("first page status = %d, page = %+v", res.StatusCode, page)
	}

	body, err := json.Marshal(map[string]any{"index": "sku", "op": "ge", "value": "a", "limit": 1, "cursor": page.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	res = sendRequest(t, server, queryMethod, "/db/stock", string(body), "application/json")
	checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+objectID+`"],"cursor":""}`)

	for _, body := range []string{
		`{"index":"sku","value":"a","cursor":"` + page.Cursor + `"}`,
		`{"index":"other","op":"ge","value":"a","cursor":"` + page.Cursor + `"}`,
		`{"path":"$.items[0].sku","op":"ge","value":"a","cursor":"` + page.Cursor + `"}`,
	} {
		res = sendRequest(t, server, queryMethod, "/db/stock", body, "application/json")
		checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_cursor","message":"Cursor is invalid"}}`)
	}

	res = sendRequest(t, server, queryMethod, "/db/stock", `{"index":"sku","value":"z"}`, "application/json")
	checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+objectID+`"],"cursor":""}`)
	res = sendRequest(t, server, queryMethod, "/db/stock", `{"index":"missing","value":"a"}`, "application/json")
	checkResponse(t, res, http.StatusNotFound, `{"error":{"code":"index_not_found","message":"Index does not exist"}}`)
	res = sendRequest(t, server, queryMethod, "/db/missing", `{"index":"sku","value":"a"}`, "application/json")
	checkResponse(t, res, http.StatusNotFound, `{"error":{"code":"database_not_found","message":"Database does not exist"}}`)
}

func TestDecodeNamedQuery(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{}`, `{"limit":1}`, `{"limit":2,"cursor":"opaque"}`,
		`{"index":"sku","value":null}`,
		`{"index":"sku","op":"ge","value":10}`,
		`{"path":"$.sku","value":"a"}`,
	} {
		if _, err := decodePathQuery([]byte(body)); err != nil {
			t.Errorf("decodePathQuery(%s): %v", body, err)
		}
	}
	for _, body := range []string{
		`{"value":1}`, `{"op":"eq"}`, `{"op":"eq","value":null}`, `{"path":null,"value":1}`,
		`{"index":"","value":1}`, `{"index":null,"value":1}`,
		`{"index":12,"value":1}`, `{"index":"BAD","value":1}`,
		`{"index":"sku","path":"$.sku","value":1}`,
		`{"index":"sku","path":null,"value":1}`, `{"index":"sku"}`,
		`{"index":"sku","index":"sku","value":1}`,
		`{"index":"sku","value":[]}`, `{"index":"sku","op":"gt","value":null}`,
	} {
		if _, err := decodePathQuery([]byte(body)); err == nil {
			t.Errorf("decodePathQuery(%s) accepted invalid query", body)
		}
	}
}
