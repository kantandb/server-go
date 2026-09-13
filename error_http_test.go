package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
)

func TestRoutingErrorsUseEnvelope(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)

	res := sendRequest(t, server, http.MethodGet, "/db/missing/route/extra", "", "")
	checkResponse(t, res, http.StatusNotFound, `{"error":{"code":"route_not_found","message":"Route does not exist"}}`)

	res = sendRequest(t, server, http.MethodPost, "/", "", "")
	checkResponse(t, res, http.StatusMethodNotAllowed, `{"error":{"code":"method_not_allowed","message":"Method is not allowed"}}`)

	res = sendRequest(t, server, http.MethodGet, "/users", "", "")
	checkResponse(t, res, http.StatusNotFound, `{"error":{"code":"route_not_found","message":"Route does not exist"}}`)
}

func TestRoutingMethodAndPathEdges(t *testing.T) {
	t.Parallel()

	handler := newHandler(testStore(t), defaultMaxBodyBytes)
	tests := []struct {
		name   string
		method string
		path   string
		status int
		body   string
	}{
		{name: "HEAD is unsupported", method: http.MethodHead, path: "/healthz", status: http.StatusMethodNotAllowed, body: `{"error":{"code":"method_not_allowed","message":"Method is not allowed"}}`},
		{name: "wrong collection method", method: http.MethodPut, path: "/db", status: http.StatusMethodNotAllowed, body: `{"error":{"code":"method_not_allowed","message":"Method is not allowed"}}`},
		{name: "wrong document method", method: queryMethod, path: "/db/dbname/01950000-0000-7000-8000-000000000001", status: http.StatusMethodNotAllowed, body: `{"error":{"code":"method_not_allowed","message":"Method is not allowed"}}`},
		{name: "extra segment", method: http.MethodGet, path: "/db/dbname/id/extra", status: http.StatusNotFound, body: `{"error":{"code":"route_not_found","message":"Route does not exist"}}`},
		{name: "repeated slash", method: http.MethodGet, path: "/healthz//", status: http.StatusNotFound, body: `{"error":{"code":"route_not_found","message":"Route does not exist"}}`},
		{name: "dot segment", method: http.MethodGet, path: "/./healthz", status: http.StatusNotFound, body: `{"error":{"code":"route_not_found","message":"Route does not exist"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)

			if res.Code != tt.status {
				t.Errorf("status = %d, want %d", res.Code, tt.status)
			}
			if got := res.Body.String(); got != tt.body {
				t.Errorf("body = %q, want %q", got, tt.body)
			}
		})
	}
}

func TestRecoveryResponse(t *testing.T) {
	t.Parallel()

	a := newAPI(nil, defaultMaxBodyBytes, slog.New(slog.DiscardHandler))
	tests := []struct {
		name   string
		write  bool
		status int
		body   string
	}{
		{name: "before write", status: http.StatusInternalServerError, body: `{"error":{"code":"internal_error","message":"Internal server error"}}`},
		{name: "after write", write: true, status: http.StatusAccepted, body: "partial"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panicHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.write {
					w.WriteHeader(http.StatusAccepted)
					if _, err := w.Write([]byte("partial")); err != nil {
						t.Fatalf("Write() error = %v", err)
					}
				}
				panic("boom")
			})

			req := httptest.NewRequest(http.MethodGet, "/panic", nil)
			res := httptest.NewRecorder()
			a.recoverHTTP(panicHandler).ServeHTTP(res, req)

			if res.Code != tt.status {
				t.Errorf("status = %d, want %d", res.Code, tt.status)
			}
			if got := res.Body.String(); got != tt.body {
				t.Errorf("body = %q, want %q", got, tt.body)
			}
		})
	}
}

func TestTrailingSlashRedirects(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"dbname"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"dbname"}`)

	client := *server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	tests := []struct {
		method   string
		path     string
		body     string
		status   int
		location string
	}{
		{method: http.MethodGet, path: "/healthz/", status: http.StatusMovedPermanently, location: "/healthz"},
		{method: http.MethodGet, path: "/db/", status: http.StatusMovedPermanently, location: "/db"},
		{method: http.MethodGet, path: "/db/dbname/", status: http.StatusMovedPermanently, location: "/db/dbname"},
		{method: http.MethodPost, path: "/db/dbname/", body: `{}`, status: http.StatusTemporaryRedirect, location: "/db/dbname"},
	}
	for _, tt := range tests {
		req, err := http.NewRequestWithContext(t.Context(), tt.method, server.URL+tt.path, strings.NewReader(tt.body))
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		if tt.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}

		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		if res.StatusCode != tt.status {
			t.Errorf("%s %s status = %d, want %d", tt.method, tt.path, res.StatusCode, tt.status)
		}
		if got := res.Header.Get("Location"); got != tt.location {
			t.Errorf("%s %s Location = %q, want %q", tt.method, tt.path, got, tt.location)
		}
		_ = readResponse(t, res)
	}

	res = sendRequest(t, server, http.MethodPost, "/db/dbname/", `{"redirected":true}`, "application/json")
	if res.StatusCode != http.StatusCreated {
		t.Errorf("redirected POST status = %d, want %d", res.StatusCode, http.StatusCreated)
	}
	_ = readResponse(t, res)
}

func TestStoppingReturnsUnavailable(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	a := newAPI(store, defaultMaxBodyBytes, slog.New(slog.DiscardHandler))
	handler := a.handler()
	a.stop()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
	if got := res.Body.String(); got != `{"error":{"code":"service_unavailable","message":"Service is unavailable"}}` {
		t.Errorf("body = %q", got)
	}
}

func TestCorruptionIsMappedAndLogged(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	if err := store.createDB("dbname"); err != nil {
		t.Fatalf("createDB() error = %v", err)
	}
	id := "01950000-0000-7000-8000-000000000001"
	if err := store.db.Set(docKey("dbname", id), []byte{0xff}, pebble.Sync); err != nil {
		t.Fatalf("DB.Set() error = %v", err)
	}

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	server := httptest.NewServer(newAPI(store, defaultMaxBodyBytes, log).handler())
	t.Cleanup(server.Close)

	res := sendRequest(t, server, http.MethodGet, "/db/dbname", "", "")
	checkResponse(t, res, http.StatusInternalServerError, `{"error":{"code":"corrupt_data","message":"Stored data is corrupt"}}`)
	if !strings.Contains(logs.String(), `"operation":"list documents"`) {
		t.Errorf("list log = %s", logs.String())
	}

	res = sendRequest(t, server, http.MethodGet, "/db/dbname/"+id, "", "")
	body := readResponse(t, res)
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusInternalServerError)
	}
	if body != `{"error":{"code":"corrupt_data","message":"Stored data is corrupt"}}` {
		t.Errorf("body = %q", body)
	}
	if strings.Contains(body, "invalid document record") {
		t.Errorf("response exposed internal error: %s", body)
	}
	if !strings.Contains(logs.String(), `"operation":"read document"`) || !strings.Contains(logs.String(), "corrupt stored data") {
		t.Errorf("log = %s", logs.String())
	}
}

func TestClosedStorageReturnsUnavailable(t *testing.T) {
	t.Parallel()

	store, err := openStore(t.TempDir(), testMasterKey)
	if err != nil {
		t.Fatalf("openStore() error = %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("store.close() error = %v", err)
	}

	server := httptest.NewServer(newHandler(store, defaultMaxBodyBytes))
	t.Cleanup(server.Close)
	id := "01950000-0000-7000-8000-000000000001"

	res := sendRequest(t, server, http.MethodGet, "/db/dbname", "", "")
	checkResponse(t, res, http.StatusServiceUnavailable, `{"error":{"code":"service_unavailable","message":"Service is unavailable"}}`)

	res = sendRequest(t, server, http.MethodGet, "/db/dbname/"+id, "", "")
	checkResponse(t, res, http.StatusServiceUnavailable, `{"error":{"code":"service_unavailable","message":"Service is unavailable"}}`)
}
