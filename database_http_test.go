package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDatabaseLifecycleHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)

	res := sendRequest(t, server, http.MethodGet, "/db", "", "")
	checkResponse(t, res, http.StatusOK, `{"databases":[]}`)

	res = sendRequest(t, server, http.MethodPost, "/db", `{"name":"beta"}`, "application/json; charset=utf-8")
	if got := res.Header.Get("Location"); got != "/db/beta" {
		t.Errorf("Location = %q, want %q", got, "/db/beta")
	}
	checkResponse(t, res, http.StatusCreated, `{"name":"beta"}`)

	res = sendRequest(t, server, http.MethodPost, "/db", `{"name":"alpha"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"alpha"}`)

	res = sendRequest(t, server, http.MethodGet, "/db", "", "")
	checkResponse(t, res, http.StatusOK, `{"databases":["alpha","beta"]}`)

	res = sendRequest(t, server, http.MethodPost, "/db", `{"name":"alpha"}`, "application/json")
	checkResponse(t, res, http.StatusConflict, `{"error":{"code":"database_exists","message":"Database already exists"}}`)

	res = sendRequest(t, server, http.MethodDelete, "/db/alpha", "", "")
	checkResponse(t, res, http.StatusNoContent, "")

	res = sendRequest(t, server, http.MethodGet, "/db", "", "")
	checkResponse(t, res, http.StatusOK, `{"databases":["beta"]}`)

	res = sendRequest(t, server, http.MethodDelete, "/db/alpha", "", "")
	checkResponse(t, res, http.StatusNotFound, `{"error":{"code":"database_not_found","message":"Database does not exist"}}`)
}

func TestCreateDatabaseIndexesHTTP(t *testing.T) {
	t.Parallel()

	store, err := openStore(t.TempDir(), testMasterKey)
	if err != nil {
		t.Fatalf("openStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Errorf("store.close() error = %v", err)
		}
	})

	server := httptest.NewServer(newHandler(store, defaultMaxBodyBytes))
	t.Cleanup(server.Close)

	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"empty","indices":[]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"empty","indices":[]}`)

	body := `{"name":"users","indices":[{"name":"email","path":"/email"},{"name":"active","path":"/active"}]}`
	res = sendRequest(t, server, http.MethodPost, "/db", body, "application/json")
	checkResponse(t, res, http.StatusCreated, body)

	defs, err := store.indexes("users")
	if err != nil {
		t.Fatalf("indexes() error = %v", err)
	}
	if len(defs) != 2 || defs[0] != (indexDef{name: "active", path: "/active"}) || defs[1] != (indexDef{name: "email", path: "/email"}) {
		t.Errorf("indexes() = %v, want active and email definitions", defs)
	}
}

func TestListDatabasesPaginationHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	for _, name := range []string{"gamma", "alpha", "beta"} {
		res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"`+name+`"}`, "application/json")
		checkResponse(t, res, http.StatusCreated, `{"name":"`+name+`"}`)
	}

	res := sendRequest(t, server, http.MethodGet, "/db?limit=2", "", "")
	checkResponse(t, res, http.StatusOK, `{"databases":["alpha","beta"]}`)

	res = sendRequest(t, server, http.MethodGet, "/db?limit=1&cursor=beta", "", "")
	checkResponse(t, res, http.StatusOK, `{"databases":["gamma"]}`)

	res = sendRequest(t, server, http.MethodGet, "/db?cursor=gamma", "", "")
	checkResponse(t, res, http.StatusOK, `{"databases":[]}`)
}

func TestListDatabasesValidationHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	tests := []struct {
		path string
		code string
	}{
		{path: "/db?limit=", code: "invalid_limit"},
		{path: "/db?limit=0", code: "invalid_limit"},
		{path: "/db?limit=1001", code: "invalid_limit"},
		{path: "/db?limit=none", code: "invalid_limit"},
		{path: "/db?cursor=Bad", code: "invalid_cursor"},
	}
	for _, tt := range tests {
		res := sendRequest(t, server, http.MethodGet, tt.path, "", "")
		body := readResponse(t, res)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want %d; body = %s", tt.path, res.StatusCode, http.StatusBadRequest, body)
		}
		if !strings.Contains(body, `"code":"`+tt.code+`"`) {
			t.Errorf("GET %s body = %s, want code %q", tt.path, body, tt.code)
		}
	}
}

func TestCreateDatabaseValidationHTTP(t *testing.T) {
	t.Parallel()

	indexes := strings.TrimSuffix(strings.Repeat(`{"name":"index","path":"/value"},`, maxIndexes+1), ",")
	tooManyIndexes := `{"name":"db","indices":[` + indexes + `]}`
	tests := []struct {
		name        string
		body        string
		contentType string
		maxBytes    int64
		status      int
		code        string
	}{
		{name: "missing content type", body: `{"name":"db"}`, maxBytes: 100, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "wrong content type", body: `{"name":"db"}`, contentType: "text/plain", maxBytes: 100, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "malformed media type", body: `{"name":"db"}`, contentType: "application/json; bad", maxBytes: 100, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "malformed JSON", body: `{"name":`, contentType: "application/json", maxBytes: 100, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "non-object JSON", body: `[]`, contentType: "application/json", maxBytes: 100, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "unknown field", body: `{"name":"db","other":true}`, contentType: "application/json", maxBytes: 100, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "unknown index field", body: `{"name":"db","indices":[{"name":"email","path":"/email","other":true}]}`, contentType: "application/json", maxBytes: 200, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "non-array indexes", body: `{"name":"db","indices":{}}`, contentType: "application/json", maxBytes: 100, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "null indexes", body: `{"name":"db","indices":null}`, contentType: "application/json", maxBytes: 100, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "duplicate indexes", body: `{"name":"db","indices":[{"name":"email","path":"/email"},{"name":"email","path":"/other"}]}`, contentType: "application/json", maxBytes: 200, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "invalid index name", body: `{"name":"db","indices":[{"name":"Bad","path":"/email"}]}`, contentType: "application/json", maxBytes: 100, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "invalid index path", body: `{"name":"db","indices":[{"name":"email","path":"email"}]}`, contentType: "application/json", maxBytes: 100, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "too many indexes", body: tooManyIndexes, contentType: "application/json", maxBytes: 2000, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "trailing value", body: `{"name":"db"}{}`, contentType: "application/json", maxBytes: 100, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "invalid name", body: `{"name":"Bad"}`, contentType: "application/json", maxBytes: 100, status: http.StatusBadRequest, code: "invalid_name"},
		{name: "too large", body: strings.Repeat("x", 17), contentType: "application/json", maxBytes: 16, status: http.StatusRequestEntityTooLarge, code: "content_too_large"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newTestServer(t, tt.maxBytes)
			res := sendRequest(t, server, http.MethodPost, "/db", tt.body, tt.contentType)
			body := readResponse(t, res)

			if res.StatusCode != tt.status {
				t.Errorf("status = %d, want %d; body = %s", res.StatusCode, tt.status, body)
			}
			if !strings.Contains(body, `"code":"`+tt.code+`"`) {
				t.Errorf("body = %s, want code %q", body, tt.code)
			}
		})
	}
}

func TestDeleteDatabaseValidatesNameHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodDelete, "/db/Bad", "", "")
	checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_name","message":"Database name is invalid"}}`)
}

func newTestServer(t *testing.T, maxBodyBytes int64) *httptest.Server {
	t.Helper()

	store, err := openStore(t.TempDir(), testMasterKey)
	if err != nil {
		t.Fatalf("openStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Errorf("store.close() error = %v", err)
		}
	})

	server := httptest.NewServer(newHandler(store, maxBodyBytes))
	t.Cleanup(server.Close)

	return server
}

func sendRequest(t *testing.T, server *httptest.Server, method, path, body, contentType string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("Client.Do() error = %v", err)
	}

	return res
}

func checkResponse(t *testing.T, res *http.Response, status int, body string) {
	t.Helper()

	gotBody := readResponse(t, res)
	if res.StatusCode != status {
		t.Errorf("status = %d, want %d; body = %s", res.StatusCode, status, gotBody)
	}
	if gotBody != body {
		t.Errorf("body = %q, want %q", gotBody, body)
	}
}

func readResponse(t *testing.T, res *http.Response) string {
	t.Helper()
	defer func() {
		if err := res.Body.Close(); err != nil {
			t.Errorf("Response.Body.Close() error = %v", err)
		}
	}()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}

	return string(body)
}
