package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestQueryMetadataHTTP(t *testing.T) {
	t.Parallel()

	s, server := newBulkTestServer(t, defaultMaxBodyBytes, defaultBulkConfig())
	if err := s.createDB("users", indexDef{name: "age", path: "/age"}); err != nil {
		t.Fatal(err)
	}

	ids := []string{indexTestIDA, indexTestIDB}
	revisions := make([]string, len(ids))
	for i, id := range ids {
		rev, err := s.createDocWithID("users", id, []byte(`{"age":25}`))
		if err != nil {
			t.Fatal(err)
		}
		revisions[i] = formatETag(rev)
	}

	for _, program := range []string{".", ".age", "select(.age == 25)", "select(.age >= 25) | .age", "null, false", "[., inputs] | length"} {
		for _, stream := range []bool{false, true} {
			var res *http.Response
			if stream {
				res = sendNDJSONQuery(t, server, "/db/users", program)
			} else {
				res = sendRequest(t, server, queryMethod, "/db/users", program, queryMediaType)
			}
			body := readResponse(t, res)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("%s: %d %s", program, res.StatusCode, body)
			}
			var rows []json.RawMessage
			if stream {
				for line := range strings.SplitSeq(strings.TrimSuffix(body, "\n"), "\n") {
					rows = append(rows, json.RawMessage(line))
				}
			} else if err := json.Unmarshal([]byte(body), &rows); err != nil {
				t.Fatal(err)
			}
			wantCount := 2
			switch program {
			case "null, false":
				wantCount = 4
			case "[., inputs] | length":
				wantCount = 1
			}
			if len(rows) != wantCount {
				t.Fatalf("%s: got %d outputs, want %d", program, len(rows), wantCount)
			}
			for i, row := range rows {
				var result map[string]json.RawMessage
				if err := json.Unmarshal(row, &result); err != nil {
					t.Fatal(err)
				}
				index := i
				if program == "null, false" {
					index /= 2
				}
				wantID, err := json.Marshal(ids[index])
				if err != nil {
					t.Fatal(err)
				}
				wantRev, err := json.Marshal(revisions[index])
				if err != nil {
					t.Fatal(err)
				}
				if len(result) != 3 || string(result["id"]) != string(wantID) ||
					string(result["rev"]) != string(wantRev) || len(result["doc"]) == 0 {
					t.Fatalf("%s: wrong metadata: %s", program, row)
				}
			}
		}
	}
}

func queryValues(t *testing.T, body string, stream bool) string {
	t.Helper()

	var rows []json.RawMessage
	if stream {
		for line := range strings.SplitSeq(strings.TrimSuffix(body, "\n"), "\n") {
			if line != "" {
				rows = append(rows, json.RawMessage(line))
			}
		}
	} else if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}

	values := make([]string, 0, len(rows))
	for _, row := range rows {
		var result struct {
			ID  string          `json:"id"`
			Rev string          `json:"rev"`
			Doc json.RawMessage `json:"doc"`
		}
		if err := json.Unmarshal(row, &result); err != nil {
			t.Fatal(err)
		}
		if validateID(result.ID) != nil || len(result.Doc) == 0 {
			t.Fatalf("invalid query wrapper: %s", row)
		}
		if _, err := parseETag(result.Rev); err != nil {
			t.Fatalf("invalid query revision: %s", row)
		}
		values = append(values, string(result.Doc))
	}
	if stream {
		if len(values) == 0 {
			return ""
		}
		return strings.Join(values, "\n") + "\n"
	}

	return "[" + strings.Join(values, ",") + "]"
}

func checkQueryResponse(t *testing.T, res *http.Response, status int, want string) {
	t.Helper()

	if res.StatusCode == http.StatusOK {
		body := queryValues(t, readResponse(t, res), strings.HasPrefix(res.Header.Get("Content-Type"), queryNDJSON))
		res.Body = io.NopCloser(bytes.NewBufferString(body))
	}
	checkResponse(t, res, status, want)
}

func queryDocsForTest(t *testing.T, s *store, program string) ([]byte, error) {
	t.Helper()

	body, err := s.queryArray(t.Context(), "users", []byte(program))
	if err != nil {
		return nil, err
	}

	return []byte(queryValues(t, string(body), false)), nil
}
