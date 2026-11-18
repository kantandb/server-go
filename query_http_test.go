package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble"
)

func TestQueryDocumentsHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"users","indices":[{"name":"active","path":"/active"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"users","indices":[{"name":"active","path":"/active"}]}`)

	var active []string
	for _, body := range []string{`{"active":true}`, `{"active":false}`, `{"active":true}`, `{"active":true}`} {
		res = sendRequest(t, server, http.MethodPost, "/db/users", body, "application/json")
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
		if body == `{"active":true}` {
			active = append(active, created.ID)
		}
	}
	slices.Sort(active)

	res = sendRequest(t, server, queryMethod, "/db/users", `{"index":"active","value":true,"op":"eq","limit":3}`, "application/json")
	var exact idPage
	if err := json.NewDecoder(res.Body).Decode(&exact); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
	if !slices.Equal(exact.Documents, active) {
		t.Errorf("exact-limit documents = %v, want %v", exact.Documents, active)
	}
	if exact.Cursor != "" {
		t.Errorf("exact-limit cursor = %q, want empty", exact.Cursor)
	}

	res = sendRequest(t, server, queryMethod, "/db/users", `{"index":"active","value":true,"limit":1}`, "application/json")
	var page idPage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
	if !slices.Equal(page.Documents, active[:1]) {
		t.Errorf("documents = %v, want %v", page.Documents, active[:1])
	}
	if page.Cursor == "" {
		t.Error("cursor is empty on first page")
	}
	if page.Cursor == active[0] {
		t.Error("query cursor exposes the document ID")
	}

	cursor := page.Cursor
	res = sendRequest(t, server, queryMethod, "/db/users", `{"index":"active","value":false,"cursor":"`+cursor+`"}`, "application/json")
	checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_cursor","message":"Cursor is invalid"}}`)

	res = sendRequest(t, server, http.MethodPost, "/db", `{"name":"others","indices":[{"name":"active","path":"/active"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"others","indices":[{"name":"active","path":"/active"}]}`)
	res = sendRequest(t, server, queryMethod, "/db/others", `{"index":"active","value":true,"cursor":"`+cursor+`"}`, "application/json")
	checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_cursor","message":"Cursor is invalid"}}`)

	res = sendRequest(t, server, http.MethodDelete, "/db/users/"+active[0], "", "")
	checkResponse(t, res, http.StatusNoContent, "")

	body := `{"index":"active","value":true,"limit":1,"cursor":"` + cursor + `"}`
	res = sendRequest(t, server, queryMethod, "/db/users", body, "application/json")
	page = idPage{}
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
	if !slices.Equal(page.Documents, active[1:2]) {
		t.Errorf("middle documents = %v, want %v", page.Documents, active[1:2])
	}
	if page.Cursor == "" {
		t.Error("cursor is empty on middle page")
	}

	body = `{"index":"active","value":true,"limit":1,"cursor":"` + page.Cursor + `"}`
	res = sendRequest(t, server, queryMethod, "/db/users", body, "application/json")
	page = idPage{}
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
	if !slices.Equal(page.Documents, active[2:]) {
		t.Errorf("final documents = %v, want %v", page.Documents, active[2:])
	}
	if page.Cursor != "" {
		t.Errorf("final cursor = %q, want empty", page.Cursor)
	}
}

func TestQueryScalarValuesHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"values","indices":[{"name":"value","path":"/value"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"values","indices":[{"name":"value","path":"/value"}]}`)

	ids := make(map[string]string)
	for name, body := range map[string]string{
		"escaped": `{"value":"a b&c"}`,
		"empty":   `{"value":""}`,
		"one":     `{"value":1}`,
		"decimal": `{"value":1.0}`,
		"boolean": `{"value":true}`,
		"string":  `{"value":"true"}`,
		"null":    `{"value":null}`,
		"missing": `{}`,
		"object":  `{"value":{"nested":true}}`,
		"array":   `{"value":[1]}`,
	} {
		ids[name] = createQueryDoc(t, server, "/db/values", body)
	}

	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "escaped string", raw: `"a b&c"`, want: []string{ids["escaped"]}},
		{name: "empty string", raw: `""`, want: []string{ids["empty"]}},
		{name: "normalized number", raw: `1e0`, want: []string{ids["one"], ids["decimal"]}},
		{name: "boolean", raw: `true`, want: []string{ids["boolean"]}},
		{name: "string type", raw: `"true"`, want: []string{ids["string"]}},
		{name: "null", raw: `null`, want: []string{ids["null"]}},
		{name: "missing and compound values skipped", raw: `"ignored"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"index":"value","value":` + tt.raw + `}`
			res := sendRequest(t, server, queryMethod, "/db/values", body, "application/json")

			var page idPage
			if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if err := res.Body.Close(); err != nil {
				t.Errorf("Response.Body.Close() error = %v", err)
			}
			slices.Sort(tt.want)
			if !slices.Equal(page.Documents, tt.want) {
				t.Errorf("documents = %v, want %v", page.Documents, tt.want)
			}
			if page.Cursor != "" {
				t.Errorf("cursor = %q, want empty", page.Cursor)
			}
		})
	}

	body := `{"index":"value","value":"ignored"}`
	res = sendRequest(t, server, queryMethod, "/db/values", body, "application/json")
	checkResponse(t, res, http.StatusOK, `{"documents":[],"cursor":""}`)
}

func TestRangeQueryHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"scores","indices":[{"name":"score","path":"/score"},{"name":"rank","path":"/score"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"scores","indices":[{"name":"score","path":"/score"},{"name":"rank","path":"/score"}]}`)

	createQueryDoc(t, server, "/db/scores", `{"score":0}`)
	firstID := createQueryDoc(t, server, "/db/scores", `{"score":10}`)
	createQueryDoc(t, server, "/db/scores", `{}`)
	want := []string{
		firstID,
		createQueryDoc(t, server, "/db/scores", `{"score":20}`),
	}
	slices.Sort(want)

	res = sendRequest(t, server, queryMethod, "/db/scores", `{"index":"score","op":"ge","value":10,"limit":1}`, "application/json")
	var first idPage
	if err := json.NewDecoder(res.Body).Decode(&first); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
	if !slices.Equal(first.Documents, want[:1]) || first.Cursor == "" {
		t.Errorf("first page = %+v, want documents %v and cursor", first, want[:1])
	}

	cursor := first.Cursor
	res = sendRequest(t, server, queryMethod, "/db/scores", `{"index":"score","op":"ge","value":10,"limit":1,"cursor":"`+cursor+`"}`, "application/json")
	var last idPage
	if err := json.NewDecoder(res.Body).Decode(&last); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}
	if !slices.Equal(last.Documents, want[1:]) || last.Cursor != "" {
		t.Errorf("last page = %+v, want documents %v and no cursor", last, want[1:])
	}

	for _, body := range []string{
		`{"index":"score","op":"gt","value":10,"limit":1,"cursor":"` + cursor + `"}`,
		`{"index":"score","op":"ge","value":11,"limit":1,"cursor":"` + cursor + `"}`,
		`{"index":"rank","op":"ge","value":10,"limit":1,"cursor":"` + cursor + `"}`,
	} {
		res = sendRequest(t, server, queryMethod, "/db/scores", body, "application/json")
		checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_cursor","message":"Cursor is invalid"}}`)
	}

	res = sendRequest(t, server, http.MethodPost, "/db", `{"name":"other","indices":[{"name":"score","path":"/score"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"other","indices":[{"name":"score","path":"/score"}]}`)
	res = sendRequest(t, server, queryMethod, "/db/other", `{"index":"score","op":"ge","value":10,"limit":1,"cursor":"`+cursor+`"}`, "application/json")
	checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_cursor","message":"Cursor is invalid"}}`)
}

func TestRangeSemanticsHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"values","indices":[{"name":"value","path":"/value"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"values","indices":[{"name":"value","path":"/value"}]}`)

	ids := make(map[string]string)
	for _, document := range []struct {
		name string
		body string
	}{
		{name: "negative", body: `{"value":-2.5}`},
		{name: "one", body: `{"value":1}`},
		{name: "decimal", body: `{"value":1.0}`},
		{name: "exponent", body: `{"value":1e0}`},
		{name: "large", body: `{"value":123456789012345678901234567890}`},
		{name: "a", body: `{"value":"a"}`},
		{name: "z", body: `{"value":"z"}`},
		{name: "unicode", body: `{"value":"é"}`},
		{name: "boolean", body: `{"value":true}`},
		{name: "null", body: `{"value":null}`},
		{name: "missing", body: `{}`},
		{name: "object", body: `{"value":{}}`},
		{name: "array", body: `{"value":[]}`},
	} {
		ids[document.name] = createQueryDoc(t, server, "/db/values", document.body)
	}

	tests := []struct {
		name  string
		op    string
		value string
		want  []string
	}{
		{name: "number lt", op: "lt", value: "1", want: []string{ids["negative"]}},
		{name: "number le", op: "le", value: "1e0", want: []string{ids["negative"], ids["one"], ids["decimal"], ids["exponent"]}},
		{name: "number gt", op: "gt", value: "1.0", want: []string{ids["large"]}},
		{name: "number ge", op: "ge", value: "1", want: []string{ids["one"], ids["decimal"], ids["exponent"], ids["large"]}},
		{name: "string lt", op: "lt", value: `"z"`, want: []string{ids["a"]}},
		{name: "string le", op: "le", value: `"z"`, want: []string{ids["a"], ids["z"]}},
		{name: "string gt", op: "gt", value: `"z"`, want: []string{ids["unicode"]}},
		{name: "string ge", op: "ge", value: `"z"`, want: []string{ids["z"], ids["unicode"]}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"index":"value","op":"` + tt.op + `","value":` + tt.value + `}`
			res := sendRequest(t, server, queryMethod, "/db/values", body, "application/json")

			var page idPage
			if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if err := res.Body.Close(); err != nil {
				t.Errorf("Response.Body.Close() error = %v", err)
			}
			slices.Sort(tt.want)
			if !slices.Equal(page.Documents, tt.want) {
				t.Errorf("documents = %v, want %v", page.Documents, tt.want)
			}
		})
	}
}

func TestRangeValueOrderHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"values","indices":[{"name":"value","path":"/value"}]}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"values","indices":[{"name":"value","path":"/value"}]}`)

	twenty := createQueryDoc(t, server, "/db/values", `{"value":20}`)
	tenA := createQueryDoc(t, server, "/db/values", `{"value":10}`)
	tenB := createQueryDoc(t, server, "/db/values", `{"value":10.0}`)
	want := []string{tenA, tenB, twenty}

	var documents []string
	cursor := ""
	for range want {
		body := `{"index":"value","op":"ge","value":10,"limit":1,"cursor":"` + cursor + `"}`
		res = sendRequest(t, server, queryMethod, "/db/values", body, "application/json")

		var page idPage
		if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		if err := res.Body.Close(); err != nil {
			t.Errorf("Response.Body.Close() error = %v", err)
		}
		documents = append(documents, page.Documents...)
		cursor = page.Cursor
	}
	if !slices.Equal(documents, want) || cursor != "" {
		t.Errorf("documents = %v, cursor = %q, want value and ID order %v with no cursor", documents, cursor, want)
	}
}

func TestParseCmpOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want cmpOp
		ok   bool
	}{
		{raw: "eq", want: cmpEq, ok: true},
		{raw: "lt", want: cmpLT, ok: true},
		{raw: "le", want: cmpLE, ok: true},
		{raw: "gt", want: cmpGT, ok: true},
		{raw: "ge", want: cmpGE, ok: true},
		{raw: ""},
		{raw: "bad"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := parseCmpOp(tt.raw)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parseCmpOp(%q) = (%d, %t), want (%d, %t)", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestGetRejectsFilteringHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	for _, params := range []string{
		"index=active", "value=true", "op=eq",
		"index=active&value=true", "index=active&value=true&op=eq",
		"index=missing&value=true", "index=&value=",
		"limit=10&index=active&value=true",
	} {
		res := sendRequest(t, server, http.MethodGet, "/db/users?"+params, "", "")
		checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_query","message":"URI query parameters are not supported"}}`)
	}
}

func TestNamedQueryValidationHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	for _, body := range []string{
		`{"index":"active","value":true,"op":"bad"}`,
		`{"index":"active","value":true,"op":"eq","op":"eq"}`,
		`{"index":"active","value":true,"op":"lt"}`,
		`{"index":"active","value":false,"op":"le"}`,
		`{"index":"active","value":null,"op":"gt"}`,
		`{"index":"active","value":null,"op":"ge"}`,
		`{"index":"Bad","value":true}`,
		`{"index":"active","value":[]}`,
		`{"index":"active","value":{}}`,
		`{"index":"active","index":"active","value":true}`,
		`{"index":"active","value":true,"value":false}`,
		`{"index":"active","value":true} false`,
	} {
		res := sendRequest(t, server, queryMethod, "/db/users", body, "application/json")
		checkResponse(t, res, http.StatusBadRequest, `{"error":{"code":"invalid_query","message":"Query body is invalid"}}`)
	}
}

func TestQueryCorruptIndexHTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
	}{
		{name: "malformed ID", id: "bad"},
		{name: "missing document", id: testCursorID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := testStore(t)
			if err := store.createDB("dbname", indexDef{name: "name", path: "/name"}); err != nil {
				t.Fatalf("createDB() error = %v", err)
			}
			value, err := encodeIndexValue("value")
			if err != nil {
				t.Fatalf("encodeIndexValue() error = %v", err)
			}
			if err := store.db.Set(indexKey("dbname", "name", value, tt.id), nil, pebble.Sync); err != nil {
				t.Fatalf("Set() error = %v", err)
			}

			server := httptest.NewServer(newHandler(store, defaultMaxBodyBytes))
			t.Cleanup(server.Close)
			body := `{"index":"name","value":"value"}`
			res := sendRequest(t, server, queryMethod, "/db/dbname", body, "application/json")
			checkResponse(t, res, http.StatusInternalServerError, `{"error":{"code":"corrupt_data","message":"Stored data is corrupt"}}`)
		})
	}
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
