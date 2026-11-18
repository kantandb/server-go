package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestPathQueryHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"users"}`)

	first := createQueryDoc(t, server, "/db/users", `{"profile":{"age":29},"tags":["staff"]}`)
	second := createQueryDoc(t, server, "/db/users", `{"profile":{"age":30},"tags":["admin","staff"]}`)
	third := createQueryDoc(t, server, "/db/users", `{"profile":{"age":31}}`)

	body := `{"path":"$.profile.age","op":"ge","value":30,"limit":1}`
	res = sendRequest(t, server, queryMethod, "/db/users", body, "application/json; charset=utf-8")
	if got := res.Header.Get("Accept-Query"); got != "application/json" {
		t.Errorf("Accept-Query = %q, want application/json", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	var page idPage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
	if len(page.Documents) != 1 || page.Documents[0] != second || page.Cursor == "" || page.Cursor == second {
		t.Fatalf("first page = %+v, want %s and opaque cursor", page, second)
	}

	body = `{"path":"$.profile.age","op":"ge","value":30,"limit":1,"cursor":"` + page.Cursor + `"}`
	res = sendRequest(t, server, queryMethod, "/db/users", body, "application/json")
	checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+third+`"],"cursor":""}`)

	body = `{"path":"$.tags[*]","value":"staff"}`
	res = sendRequest(t, server, queryMethod, "/db/users", body, "application/json")
	checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+first+`","`+second+`"],"cursor":""}`)

	res = sendRequest(t, server, http.MethodGet, "/db/users", "", "")
	if got := res.Header.Get("Accept-Query"); got != "application/json" {
		t.Errorf("GET Accept-Query = %q, want application/json", got)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
}

func TestPathQueryUsesIndexHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"scores","indices":[{"name":"score","path":"/score"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"scores","indices":[{"name":"score","path":"/score"}]}`)

	twenty := createQueryDoc(t, server, "/db/scores", `{"score":20}`)
	ten := createQueryDoc(t, server, "/db/scores", `{"score":10}`)

	body := `{"path":"$.score","op":"ge","value":10,"limit":1}`
	res = sendRequest(t, server, queryMethod, "/db/scores", body, "application/json")
	var first idPage
	if err := json.NewDecoder(res.Body).Decode(&first); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
	if len(first.Documents) != 1 || first.Documents[0] != ten || first.Cursor == "" || first.Cursor == ten {
		t.Fatalf("first page = %+v, want indexed value order and opaque cursor", first)
	}

	for _, body := range []string{
		`{"path":"$.other","op":"ge","value":10,"limit":1,"cursor":"` + first.Cursor + `"}`,
		`{"path":"$.score","op":"gt","value":10,"limit":1,"cursor":"` + first.Cursor + `"}`,
		`{"path":"$.score","op":"ge","value":11,"limit":1,"cursor":"` + first.Cursor + `"}`,
	} {
		res = sendRequest(t, server, queryMethod, "/db/scores", body, "application/json")
		checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_cursor","message":"Cursor is invalid"}}`)
	}

	res = sendRequest(t, server, http.MethodGet, "/db/scores?cursor="+url.QueryEscape(first.Cursor), "", "")
	checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_query","message":"URI query parameters are not supported"}}`)

	body = `{"path":"$['score']","op":"ge","value":10,"limit":1,"cursor":"` + first.Cursor + `"}`
	res = sendRequest(t, server, queryMethod, "/db/scores", body, "application/json")
	checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+twenty+`"],"cursor":""}`)
}

func TestPathQueryNumericTokenHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"stock","indices":[{"name":"sku","path":"/items/0/sku"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"stock","indices":[{"name":"sku","path":"/items/0/sku"}]}`)

	arrayID := createQueryDoc(t, server, "/db/stock", `{"items":[{"sku":"match"}]}`)
	objectID := createQueryDoc(t, server, "/db/stock", `{"items":{"0":{"sku":"match"}}}`)

	res = sendRequest(t, server, queryMethod, "/db/stock", `{"path":"$.items[0].sku","value":"match"}`, "application/json")
	checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+arrayID+`"],"cursor":""}`)

	res = sendRequest(t, server, queryMethod, "/db/stock", `{"path":"$.items['0'].sku","value":"match"}`, "application/json")
	checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+objectID+`"],"cursor":""}`)
}

func TestPathQueryLongCursorHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"deep"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"deep"}`)

	names := make([]string, maxPathSegments)
	var value any = 1
	for i := len(names) - 1; i >= 0; i-- {
		names[i] = "aaaaa"
		value = map[string]any{names[i]: value}
	}
	doc, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	firstID := createQueryDoc(t, server, "/db/deep", string(doc))
	secondID := createQueryDoc(t, server, "/db/deep", string(doc))

	path := "$." + strings.Join(names, ".")
	body, err := json.Marshal(map[string]any{"path": path, "value": 1, "limit": 1})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	res = sendRequest(t, server, queryMethod, "/db/deep", string(body), "application/json")
	var page idPage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
	if len(page.Documents) != 1 || page.Documents[0] != firstID || page.Cursor == "" {
		t.Fatalf("first page = %+v, want %s and cursor", page, firstID)
	}

	body, err = json.Marshal(map[string]any{"path": path, "value": 1, "limit": 1, "cursor": page.Cursor})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	res = sendRequest(t, server, queryMethod, "/db/deep", string(body), "application/json")
	checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+secondID+`"],"cursor":""}`)
}

func TestPathQueryLimitsHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"users"}`)

	body := `{"path":"$` + strings.Repeat(".a", maxQueryPath) + `","value":1}`
	res = sendRequest(t, server, queryMethod, "/db/users", body, "application/json")
	checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_query","message":"Path is invalid"}}`)

	body = `{"path":"$","value":null}` + strings.Repeat(" ", maxQueryBody)
	res = sendRequest(t, server, queryMethod, "/db/users", body, "application/json")
	checkResponse(t, res, http.StatusRequestEntityTooLarge, `{"error":{"code":"content_too_large","message":"Request body exceeds the size limit"}}`)
}

func TestPathQueryContractHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, 256)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"users"}`)

	tests := []struct {
		name        string
		path        string
		body        string
		contentType string
		status      int
		code        string
	}{
		{name: "URI query", path: "/db/users?limit=1", body: `{"path":"$","value":null}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "missing content type", path: "/db/users", body: `{"path":"$","value":null}`, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "wrong content type", path: "/db/users", body: `{"path":"$","value":null}`, contentType: "text/plain", status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "empty body", path: "/db/users", contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "array body", path: "/db/users", body: `[]`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "missing path", path: "/db/users", body: `{"value":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "missing value", path: "/db/users", body: `{"path":"$.age"}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "unknown field", path: "/db/users", body: `{"path":"$.age","value":1,"extra":true}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "duplicate field", path: "/db/users", body: `{"path":"$.age","path":"$.name","value":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "trailing JSON", path: "/db/users", body: `{"path":"$.age","value":1}{}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "compound value", path: "/db/users", body: `{"path":"$.age","value":[]}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "bad operator", path: "/db/users", body: `{"path":"$.age","op":"ne","value":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "ordered boolean", path: "/db/users", body: `{"path":"$.age","op":"gt","value":true}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "bad limit", path: "/db/users", body: `{"path":"$.age","value":1,"limit":0}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "bad cursor", path: "/db/users", body: `{"path":"$.age","value":1,"cursor":"bad"}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_cursor"},
		{name: "invalid path", path: "/db/users", body: `{"path":"age","value":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "expanding path", path: "/db/users", body: `{"path":"$..*..*","value":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "union path", path: "/db/users", body: `{"path":"$['a','a']","value":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_query"},
		{name: "missing database", path: "/db/missing", body: `{"path":"$.age","value":1}`, contentType: "application/json", status: http.StatusNotFound, code: "database_not_found"},
		{name: "body too large", path: "/db/users", body: strings.Repeat(" ", 257), contentType: "application/json", status: http.StatusRequestEntityTooLarge, code: "content_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := sendRequest(t, server, queryMethod, tt.path, tt.body, tt.contentType)
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
