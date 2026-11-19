package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJQHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	checkResponse(t, sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json"), http.StatusCreated, `{"name":"users"}`)
	checkResponse(t, sendRequest(t, server, queryMethod, "/db/users", ".", "text/plain"), http.StatusOK, "[]")
	createQueryDoc(t, server, "/db/users", `{"age":25,"name":"Alice"}`)
	createQueryDoc(t, server, "/db/users", `{"age":17,"name":"Bob"}`)

	tests := []struct{ program, want string }{
		{`select(.age >= 18) | .name`, `["Alice"]`},
		{`select(.age >= 18) | [.name]`, `[["Alice"]]`},
		{`[., inputs] | map(.age) | add`, `[42]`},
		{`null, false, empty, 1`, `[null,false,1,null,false,1]`},
		{`1, halt, 2`, `[1]`},
		{`env, $ENV`, `[{},{},{},{}]`},
	}
	for _, tt := range tests {
		res := sendRequest(t, server, queryMethod, "/db/users", tt.program, "text/plain; charset=UTF-8")
		if res.Header.Get("Accept-Query") != "text/plain" || res.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("query headers = %v", res.Header)
		}
		checkResponse(t, res, http.StatusOK, tt.want)
	}

	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		res := sendRequest(t, server, method, "/db/users", "", "")
		if res.Header.Get("Accept-Query") != "text/plain" {
			t.Errorf("%s headers = %v", method, res.Header)
		}
		readResponse(t, res)
	}
}

func TestJQAcceptHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	checkResponse(t, sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json"), http.StatusCreated, `{"name":"users"}`)
	postQueryDoc(t, server, `{"age":32,"name":"Alice","skills":["Go","Python"]}`)

	tests := []struct {
		accept, program, contentType, body, trailer string
		status                                      int
	}{
		{queryJSON, `.skills[]`, queryJSON, `["Go","Python"]`, "", 200},
		{queryNDJSON, `.skills[]`, queryNDJSON, "\"Go\"\n\"Python\"\n", "ok", 200},
		{queryNDJSON, `.skills | map(ascii_downcase)`, queryNDJSON, "[\"go\",\"python\"]\n", "ok", 200},
		{queryNDJSON, `null, false, "a\nb"`, queryNDJSON, "null\nfalse\n\"a\\nb\"\n", "ok", 200},
		{queryNDJSON, "empty", queryNDJSON, "", "ok", 200},
		{queryJSON, "empty", queryJSON, "[]", "", 200},
		{"application/json;q=.2, application/x-ndjson;q=.8", "1", queryNDJSON, "1\n", "ok", 200},
		{"application/json;q=0, */*", "1", queryNDJSON, "1\n", "ok", 200},
		{"application/*", "1", queryJSON, "[1]", "", 200},
		{"text/html", ".", queryJSON, `{"error":{"code":"not_acceptable","message":"Accept must allow application/json or application/x-ndjson"}}`, "", 406},
		{queryNDJSON, `error("test")`, queryJSON, `{"error":{"code":"query_runtime","message":"Query execution failed"}}`, "", 400},
		{queryNDJSON, `1, error("test")`, queryNDJSON, "1\n", "error", 200},
	}
	for _, tt := range tests {
		req, err := http.NewRequest(queryMethod, server.URL+"/db/users", strings.NewReader(tt.program))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", queryMediaType)
		req.Header.Set("Accept", tt.accept)
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if got, _, _ := strings.Cut(res.Header.Get("Content-Type"), ";"); got != tt.contentType {
			t.Errorf("Content-Type for %q = %q, want %q", tt.accept, got, tt.contentType)
		}
		if got := res.Header.Get("Vary"); got != "Accept" {
			t.Errorf("Vary = %q, want Accept", got)
		}
		checkResponse(t, res, tt.status, tt.body)
		if got := res.Trailer.Get(queryStatus); got != tt.trailer {
			t.Errorf("Query-Status for %q = %q, want %q", tt.program, got, tt.trailer)
		}
	}
}

func TestQueryFlushesEarly(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	checkResponse(t, sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json"), http.StatusCreated, `{"name":"users"}`)
	postQueryDoc(t, server, `{}`)

	ctx, cancel := context.WithTimeout(t.Context(), queryTimeout/2)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, queryMethod, server.URL+"/db/users", strings.NewReader(`1, (def loop: loop; loop)`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", queryMediaType)
	req.Header.Set("Accept", queryNDJSON)
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if res.StatusCode != http.StatusOK || line != "1\n" || err != nil {
		t.Fatalf("status = %d, first line = %q, err = %v", res.StatusCode, line, err)
	}
}

func TestNDJSONExceedsBuffer(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	checkResponse(t, sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json"), http.StatusCreated, `{"name":"users"}`)
	postQueryDoc(t, server, `{}`)

	res := sendNDJSONQuery(t, server, "/db/users", `range(80) | "x" * 131072`)
	body := readResponse(t, res)
	if res.StatusCode != http.StatusOK || len(body) <= maxQueryBytes || strings.Count(body, "\n") != 80 || res.Trailer.Get(queryStatus) != "ok" {
		t.Fatalf("status = %d, bytes = %d, trailer = %v", res.StatusCode, len(body), res.Trailer)
	}
}

func TestJQErrorsHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	checkResponse(t, sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json"), http.StatusCreated, `{"name":"users"}`)
	postQueryDoc(t, server, `{}`)

	tests := []struct {
		path, program, contentType, code string
		status                           int
	}{
		{"/db/users", `{"path":"$.age","value":18}`, "application/json", "unsupported_media_type", 415},
		{"/db/users", ".", "text/plain; charset=latin1", "unsupported_media_type", 415},
		{"/db/users?limit=1", ".", "text/plain", "invalid_query", 400},
		{"/db/users", " ", "text/plain", "invalid_query", 400},
		{"/db/users", "[", "text/plain", "invalid_query", 400},
		{"/db/users", `include "local"; .`, "text/plain", "invalid_query", 400},
		{"/db/users", `1, error("test")`, "text/plain", "query_runtime", 400},
		{"/db/users", strings.Repeat(" ", maxQueryBody+1), "text/plain", "content_too_large", 413},
		{"/db/users", `"a" * 8388608`, "text/plain", "query_limit", 503},
		{"/db/missing", ".", "text/plain", "database_not_found", 404},
	}
	for _, tt := range tests {
		res := sendRequest(t, server, queryMethod, tt.path, tt.program, tt.contentType)
		var envelope errorEnvelope
		body := readResponse(t, res)
		if err := json.Unmarshal([]byte(body), &envelope); err != nil || res.StatusCode != tt.status || envelope.Error.Code != tt.code {
			t.Errorf("status = %d, body = %s; want %d %s", res.StatusCode, body, tt.status, tt.code)
		}
	}
}

func TestJQBusyHTTP(t *testing.T) {
	t.Parallel()

	a := newAPI(testStore(t), defaultMaxBodyBytes, discardLogger())
	a.querySlots <- struct{}{}
	req := httptest.NewRequest(queryMethod, "/db/users", strings.NewReader("."))
	req.Header.Set("Content-Type", "text/plain")
	res := httptest.NewRecorder()
	a.handler().ServeHTTP(res, req)
	checkResponse(t, res.Result(), http.StatusTooManyRequests, `{"error":{"code":"query_busy","message":"Too many queries"}}`)
}

func sendNDJSONQuery(t *testing.T, server *httptest.Server, path, program string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), queryMethod, server.URL+path, strings.NewReader(program))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", queryMediaType)
	req.Header.Set("Accept", queryNDJSON)
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	return res
}

func postQueryDoc(t *testing.T, server *httptest.Server, body string) {
	t.Helper()

	createQueryDoc(t, server, "/db/users", body)
}

func createQueryDoc(t *testing.T, server *httptest.Server, path, body string) string {
	t.Helper()

	res := sendRequest(t, server, http.MethodPost, path, body, "application/json")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", res.StatusCode, http.StatusCreated, readResponse(t, res))
	}

	var created docResponse
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}

	return created.ID
}
