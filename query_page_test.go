package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/itchyny/gojq"
)

func TestPageQuery(t *testing.T) {
	t.Parallel()

	for _, program := range []string{
		".", ".name", ".nested.name", "select(.age >= 18)",
		"select(.age >= 18) | .name", "select(.active)",
		"[.name, .age]", "{name: .name, age: .age}", "{}", "[]",
		"null", "-1", "(.name)", `select(.name | startswith("A"))`,
		`select(.name | test("a"; "i"))`, ".name | ascii_downcase",
	} {
		q, err := gojq.Parse(program)
		if err != nil || !pageQuery(q, 128) {
			t.Errorf("rejected %s: %v", program, err)
		}
	}
	for _, program := range []string{
		".name, .age", ".skills[]", "range(100)", "input", "inputs",
		"[., inputs]", "[., inputs] | sort_by(.age)",
		"def read: input; read", "def f: .; f", "now", "..", ".foo?",
		"[.skills[]]", "{value: inputs}", `"\(input)"`,
		"select(input)", "[input]", "limit(1; inputs)",
	} {
		q, err := gojq.Parse(program)
		if err != nil || pageQuery(q, 128) {
			t.Errorf("accepted %s or parse failed: %v", program, err)
		}
	}
}

func TestQueryPagesHTTP(t *testing.T) {
	t.Parallel()

	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprint(indexed), func(t *testing.T) {
			s, server := pageTestServer(t, indexed)
			for i := range 7 {
				id := fmt.Sprintf("01950000-0000-7000-8000-%012d", i)
				body := fmt.Appendf(nil, `{"age":%d,"name":"name%d"}`, (6-i)/2, i)
				if _, err := s.createDocWithID("users", id, body); err != nil {
					t.Fatal(err)
				}
			}
			for _, program := range []string{
				".", ".name", "[.name, .age]", "{name: .name, age: .age}",
				"select(.age >= 1) | .name", "select(.age == 1)",
				"select(.age < 0)", "select(.age >= 3)",
			} {
				res := sendRequest(t, server, queryMethod, "/db/users", program, queryMediaType)
				want := pageRows(t, readResponse(t, res), false)
				for _, stream := range []bool{false, true} {
					var got []json.RawMessage
					cursor := ""
					for n := 0; ; n++ {
						if n > 10 {
							t.Fatal("pagination did not finish")
						}
						res := sendQueryPage(t, server, program, "2", cursor, stream)
						body := readResponse(t, res)
						if res.StatusCode != http.StatusOK {
							t.Fatalf("%s: status %d: %s", program, res.StatusCode, body)
						}
						rows := pageRows(t, body, stream)
						if len(rows) > 2 {
							t.Fatalf("page contains %d results", len(rows))
						}
						got = append(got, rows...)
						cursor = res.Header.Get("Cursor")
						if stream {
							if cursor != "" || res.Trailer.Get(queryStatus) != "ok" {
								t.Fatalf("invalid streaming metadata: %v %v", res.Header, res.Trailer)
							}
							cursor = res.Trailer.Get("Cursor")
						}
						if cursor == "" {
							break
						}
						if len(rows) != 2 {
							t.Fatal("incomplete page has a continuation cursor")
						}
					}
					if !slices.EqualFunc(got, want, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }) {
						t.Fatalf("%s: got %s, want %s", program, got, want)
					}
				}
			}
		})
	}
}

func TestPageSeekHTTP(t *testing.T) {
	t.Parallel()

	for _, indexed := range []bool{false, true} {
		for _, deleted := range []bool{false, true} {
			t.Run(fmt.Sprintf("index=%t/delete=%t", indexed, deleted), func(t *testing.T) {
				s, server := pageTestServer(t, indexed)
				ids := []string{indexTestIDA, indexTestIDB, indexTestIDC, indexTestIDZ}
				for _, id := range ids {
					if _, err := s.createDocWithID("users", id, []byte(`{"age":25}`)); err != nil {
						t.Fatal(err)
					}
				}
				program := "."
				if indexed {
					program = "select(.age >= 18) | .age"
				}
				res := sendQueryPage(t, server, program, "1", "", false)
				readResponse(t, res)
				cursor := res.Header.Get("Cursor")
				if res.StatusCode != http.StatusOK || cursor == "" {
					t.Fatal("first page failed")
				}
				if deleted {
					if err := s.deleteDoc("users", ids[0], matchCond{}); err != nil {
						t.Fatal(err)
					}
				} else if err := s.db.Set(docKey("users", ids[0]), []byte("bad record"), pebble.Sync); err != nil {
					t.Fatal(err)
				}
				// Neither prior pages nor entries beyond the lookahead can be read.
				if err := s.db.Set(docKey("users", ids[3]), []byte("bad record"), pebble.Sync); err != nil {
					t.Fatal(err)
				}
				res = sendQueryPage(t, server, program, "1", cursor, false)
				body := readResponse(t, res)
				if res.StatusCode != http.StatusOK || !strings.Contains(body, ids[1]) || res.Header.Get("Cursor") == "" {
					t.Fatalf("resumed page = %d %s", res.StatusCode, body)
				}
			})
		}
	}
}

func TestPageErrorsHTTP(t *testing.T) {
	t.Parallel()

	_, server := pageTestServer(t, true)
	for _, tc := range []struct{ program, limit, cursor, code string }{
		{".", "0", "", "invalid_limit"},
		{".", "-1", "", "invalid_limit"},
		{".", "1001", "", "invalid_limit"},
		{".", "x", "", "invalid_limit"},
		{".", "1", "bad", "invalid_cursor"},
		{".age, .name", "1", "", "query_not_pageable"},
		{"inputs", "1", "", "query_not_pageable"},
		{"[., inputs]", "1", "", "query_not_pageable"},
		{"def read: input; read", "1", "", "query_not_pageable"},
		{".skills[]", "1", "", "query_not_pageable"},
		{"select(.age == 1 or .age == 2)", "1", "", "query_not_pageable"},
		{"select(.age | IN(1, 2))", "1", "", "query_not_pageable"},
		{"[", "1", "", "invalid_query"},
	} {
		for _, stream := range []bool{false, true} {
			res := sendQueryPage(t, server, tc.program, tc.limit, tc.cursor, stream)
			body := readResponse(t, res)
			if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"`+tc.code+`"`) {
				t.Errorf("%s: %d %s; want %s", tc.program, res.StatusCode, body, tc.code)
			}
			if res.Header.Get("Cursor") != "" || res.Trailer.Get("Cursor") != "" {
				t.Fatal("failed page returned a cursor")
			}
		}
	}
}

func TestPageDefaultAndBindingHTTP(t *testing.T) {
	t.Parallel()

	s, server := pageTestServer(t, false)
	for range 103 {
		if _, _, err := s.createDoc("users", []byte(`{"name":"Alice"}`)); err != nil {
			t.Fatal(err)
		}
	}
	res := sendQueryPage(t, server, ".", "", "", false)
	if rows := pageRows(t, readResponse(t, res), false); len(rows) != 103 || res.Header.Get("Cursor") != "" {
		t.Fatal("unpaginated query changed")
	}
	res = sendQueryPage(t, server, ".", "1", "", false)
	readResponse(t, res)
	cursor := res.Header.Get("Cursor")
	if cursor == "" {
		t.Fatal("missing first cursor")
	}
	res = sendQueryPage(t, server, ".name", "1", cursor, false)
	body := readResponse(t, res)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"invalid_cursor"`) {
		t.Fatalf("accepted cursor for another query: %d %s", res.StatusCode, body)
	}
	res = sendQueryPage(t, server, ".", "", cursor, true)
	if rows := pageRows(t, readResponse(t, res), true); len(rows) != 100 || res.Trailer.Get("Cursor") == "" || res.Trailer.Get(queryStatus) != "ok" {
		t.Fatalf("cursor-only request did not use default limit: %d results", len(rows))
	}
	res = sendQueryPage(t, server, ".", "3", res.Trailer.Get("Cursor"), false)
	if rows := pageRows(t, readResponse(t, res), false); len(rows) != 2 || res.Header.Get("Cursor") != "" {
		t.Fatalf("last page: %d results", len(rows))
	}
	if err := s.deleteDB("users"); err != nil {
		t.Fatal(err)
	}
	if err := s.createDB("users"); err != nil {
		t.Fatal(err)
	}
	res = sendQueryPage(t, server, ".", "1", cursor, false)
	body = readResponse(t, res)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"invalid_cursor"`) {
		t.Fatalf("accepted cursor after database recreation: %d %s", res.StatusCode, body)
	}
}

func TestPageLookaheadFailureHTTP(t *testing.T) {
	t.Parallel()

	s, server := pageTestServer(t, false)
	for id, body := range map[string]string{
		indexTestIDA: `{"name":"Alice"}`,
		indexTestIDB: `{"name":1}`,
	} {
		if _, err := s.createDocWithID("users", id, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, stream := range []bool{false, true} {
		res := sendQueryPage(t, server, `.name | startswith("A")`, "1", "", stream)
		body := readResponse(t, res)
		if stream {
			if res.StatusCode != http.StatusOK || res.Trailer.Get(queryStatus) != "error" || len(pageRows(t, body, true)) != 1 {
				t.Fatalf("streamed failure: %d %s %v", res.StatusCode, body, res.Trailer)
			}
		} else if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"query_runtime"`) {
			t.Fatalf("buffered failure: %d %s", res.StatusCode, body)
		}
		if res.Header.Get("Cursor") != "" || res.Trailer.Get("Cursor") != "" {
			t.Fatal("failed page returned a cursor")
		}
	}
}

func pageTestServer(t *testing.T, indexed bool) (*store, *httptest.Server) {
	t.Helper()

	s, server := newBulkTestServer(t, defaultMaxBodyBytes, defaultBulkConfig())
	var defs []indexDef
	if indexed {
		defs = []indexDef{{name: "age", path: "/age"}}
	}
	if err := s.createDB("users", defs...); err != nil {
		t.Fatal(err)
	}

	return s, server
}

func sendQueryPage(t *testing.T, server *httptest.Server, program, limit, cursor string, stream bool) *http.Response {
	t.Helper()

	req, err := http.NewRequest(queryMethod, server.URL+"/db/users", strings.NewReader(program))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", queryMediaType)
	if limit != "" {
		req.Header.Set("Limit", limit)
	}
	if cursor != "" {
		req.Header.Set("Cursor", cursor)
	}
	if stream {
		req.Header.Set("Accept", queryNDJSON)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	return res
}

func pageRows(t *testing.T, body string, stream bool) []json.RawMessage {
	t.Helper()

	if stream {
		body = "[" + strings.Join(strings.FieldsFunc(body, func(r rune) bool { return r == '\n' }), ",") + "]"
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}

	return rows
}
