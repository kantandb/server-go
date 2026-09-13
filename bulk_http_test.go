package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBulkRoundTripHTTP(t *testing.T) {
	t.Parallel()

	source, sourceServer := newBulkTestServer(t, defaultMaxBodyBytes, defaultBulkConfig())
	if err := source.createDB("source"); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	for _, document := range []struct {
		id   string
		body string
	}{
		{id: "01950000-0000-7000-8000-000000000002", body: `{"name":"second"}`},
		{id: "01950000-0000-7000-8000-000000000001", body: `{"id":"ordinary","name":"first"}`},
	} {
		if _, err := source.createDocWithID("source", document.id, []byte(document.body)); err != nil {
			t.Fatalf("createDocWithID() error = %v", err)
		}
	}

	exported := sendRequest(t, sourceServer, http.MethodGet, "/bulk/source", "", "")
	if exported.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", exported.StatusCode)
	}
	if got := exported.Header.Get("Content-Type"); got != bulkMediaType {
		t.Errorf("Content-Type = %q, want %q", got, bulkMediaType)
	}
	if got := exported.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	body := readResponse(t, exported)
	want := "{\"id\":\"ordinary\",\"name\":\"first\"}\n{\"name\":\"second\"}\n"
	if body != want {
		t.Fatalf("export body = %q, want %q", body, want)
	}

	target, targetServer := newBulkTestServer(t, defaultMaxBodyBytes, defaultBulkConfig())
	if err := target.createDB("target", indexDef{name: "name", path: "/name"}); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	imported := sendRequest(t, targetServer, http.MethodPost, "/bulk/target", body, bulkMediaType+"; charset=utf-8")
	checkResponse(t, imported, http.StatusOK, `{"success":true,"error":{}}`)

	ids, _, err := target.listDocs("target", 10, "")
	if err != nil {
		t.Fatalf("listDocs() error = %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("import stored %d documents, want 2", len(ids))
	}
	assertQuery(t, target, "target", "name", "first", idsForValue(t, target, ids, `{"id":"ordinary","name":"first"}`))
}

func TestBulkExportEmptyHTTP(t *testing.T) {
	t.Parallel()

	store, server := newBulkTestServer(t, defaultMaxBodyBytes, defaultBulkConfig())
	if err := store.createDB("db"); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}

	res := sendRequest(t, server, http.MethodGet, "/bulk/db", "", "")
	if res.StatusCode != http.StatusOK || readResponse(t, res) != "" {
		t.Errorf("empty export status = %d", res.StatusCode)
	}
}

func TestBulkRequestValidationHTTP(t *testing.T) {
	t.Parallel()

	store, server := newBulkTestServer(t, defaultMaxBodyBytes, defaultBulkConfig())
	if err := store.createDB("db"); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}

	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
		status      int
		code        string
	}{
		{name: "query export", method: http.MethodGet, path: "/bulk/db?x=1", status: 400, code: "invalid_query"},
		{name: "query import", method: http.MethodPost, path: "/bulk/db?x=1", body: `{}`, contentType: bulkMediaType, status: 400, code: "invalid_query"},
		{name: "invalid name", method: http.MethodGet, path: "/bulk/Bad", status: 400, code: "invalid_name"},
		{name: "media type", method: http.MethodPost, path: "/bulk/db", body: `{}`, contentType: "application/json", status: 415, code: "unsupported_media_type"},
		{name: "empty import", method: http.MethodPost, path: "/bulk/db", contentType: bulkMediaType, status: 400, code: "invalid_document"},
		{name: "missing export", method: http.MethodGet, path: "/bulk/missing", status: 404, code: "database_not_found"},
		{name: "missing import", method: http.MethodPost, path: "/bulk/missing", body: `{}`, contentType: bulkMediaType, status: 404, code: "database_not_found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := sendRequest(t, server, tt.method, tt.path, tt.body, tt.contentType)
			body := readResponse(t, res)
			if res.StatusCode != tt.status || !strings.Contains(body, `"code":"`+tt.code+`"`) {
				t.Errorf("response = %d %s, want %d %s", res.StatusCode, body, tt.status, tt.code)
			}
		})
	}

	res := sendRequest(t, server, http.MethodPut, "/bulk/db", "", "")
	checkResponse(t, res, http.StatusMethodNotAllowed, `{"error":{"code":"method_not_allowed","message":"Method is not allowed"}}`)
}

func TestBulkImportErrorsHTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		maxBody int64
		bulk    bulkConfig
		status  int
		line    int
	}{
		{name: "invalid second line", body: "{}\n[]\n", maxBody: 100, bulk: bulkConfig{maxBytes: 100, maxDocuments: 2, maxBatchBytes: 1000, timeout: time.Second, concurrency: 1}, status: 400, line: 2},
		{name: "line limit", body: "{\"value\":1}\n", maxBody: 5, bulk: bulkConfig{maxBytes: 100, maxDocuments: 2, maxBatchBytes: 1000, timeout: time.Second, concurrency: 1}, status: 413, line: 1},
		{name: "total limit", body: "{}\n{}\n", maxBody: 100, bulk: bulkConfig{maxBytes: 5, maxDocuments: 2, maxBatchBytes: 1000, timeout: time.Second, concurrency: 1}, status: 413},
		{name: "count limit", body: "{}\n{}\n", maxBody: 100, bulk: bulkConfig{maxBytes: 100, maxDocuments: 1, maxBatchBytes: 1000, timeout: time.Second, concurrency: 1}, status: 413, line: 2},
		{name: "batch limit", body: "{}\n", maxBody: 100, bulk: bulkConfig{maxBytes: 100, maxDocuments: 1, maxBatchBytes: 1, timeout: time.Second, concurrency: 1}, status: 413, line: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, server := newBulkTestServer(t, tt.maxBody, tt.bulk)
			if err := store.createDB("db"); err != nil {
				t.Fatalf("createDB() error = %v", err)
			}

			res := sendRequest(t, server, http.MethodPost, "/bulk/db", tt.body, bulkMediaType)
			body := readResponse(t, res)
			if res.StatusCode != tt.status || tt.line != 0 && !strings.Contains(body, `"line":`+strconv.Itoa(tt.line)) {
				t.Errorf("response = %d %s, want status %d line %d", res.StatusCode, body, tt.status, tt.line)
			}

			ids, _, err := store.listDocs("db", 10, "")
			if err != nil || len(ids) != 0 {
				t.Errorf("failed import stored %v, error %v", ids, err)
			}
		})
	}
}

func TestBulkLimitsHTTP(t *testing.T) {
	t.Parallel()

	t.Run("concurrency", func(t *testing.T) {
		store := testStore(t)
		if err := store.createDB("db"); err != nil {
			t.Fatalf("createDB() error = %v", err)
		}
		api := newAPIWithBulk(store, defaultMaxBodyBytes, defaultBulkConfig(), discardLogger())
		server := httptest.NewServer(api.handler())
		t.Cleanup(server.Close)
		if !api.takeBulkSlot() {
			t.Fatal("could not reserve bulk slot")
		}
		defer api.freeBulkSlot()

		res := sendRequest(t, server, http.MethodGet, "/bulk/db", "", "")
		body := readResponse(t, res)
		if res.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, `"code":"bulk_limit"`) {
			t.Errorf("response = %d %s", res.StatusCode, body)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		bulk := defaultBulkConfig()
		bulk.timeout = time.Nanosecond
		store, server := newBulkTestServer(t, defaultMaxBodyBytes, bulk)
		if err := store.createDB("db"); err != nil {
			t.Fatalf("createDB() error = %v", err)
		}

		res := sendRequest(t, server, http.MethodPost, "/bulk/db", `{}`, bulkMediaType)
		body := readResponse(t, res)
		if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"bulk_timeout"`) {
			t.Errorf("response = %d %s", res.StatusCode, body)
		}
	})
}

func newBulkTestServer(t *testing.T, maxBodyBytes int64, bulk bulkConfig) (*store, *httptest.Server) {
	t.Helper()

	store := testStore(t)
	server := httptest.NewServer(newAPIWithBulk(store, maxBodyBytes, bulk, discardLogger()).handler())
	t.Cleanup(server.Close)

	return store, server
}

func idsForValue(t *testing.T, store *store, ids []string, value string) []string {
	t.Helper()

	for _, id := range ids {
		doc, err := store.getDoc("target", id)
		if err != nil {
			t.Fatalf("getDoc() error = %v", err)
		}
		if string(doc.json) == value {
			return []string{id}
		}
	}

	t.Fatal("imported document not found")

	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
