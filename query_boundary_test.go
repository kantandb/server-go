package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"testing"
)

func TestPathQuerySemanticsHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"values"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"values"}`)

	ids := make(map[string]string)
	for name, body := range map[string]string{
		"negative": `{"value":-1}`,
		"one":      `{"value":1}`,
		"decimal":  `{"value":1.0}`,
		"two":      `{"value":2}`,
		"string":   `{"value":"z"}`,
		"null":     `{"value":null}`,
		"missing":  `{}`,
		"object":   `{"value":{"nested":1}}`,
		"array":    `{"value":[1,2]}`,
		"escaped":  `{"a/b":{"~x":"match"}}`,
	} {
		ids[name] = createQueryDoc(t, server, "/db/values", body)
	}

	tests := []struct {
		name  string
		path  string
		op    string
		value any
		want  []string
	}{
		{name: "numeric equivalence", path: `$.value`, value: json.Number("1e0"), want: []string{ids["one"], ids["decimal"]}},
		{name: "numeric boundary", path: `$.value`, op: "ge", value: json.Number("1"), want: []string{ids["one"], ids["decimal"], ids["two"]}},
		{name: "type mismatch", path: `$.value`, value: "1"},
		{name: "null", path: `$.value`, value: nil, want: []string{ids["null"]}},
		{name: "wildcard", path: `$.value[*]`, value: json.Number("2"), want: []string{ids["array"]}},
		{name: "escaped names", path: `$['a/b']['~x']`, value: "match", want: []string{ids["escaped"]}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"path": tt.path, "op": tt.op, "value": tt.value})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if tt.op == "" {
				body, err = json.Marshal(map[string]any{"path": tt.path, "value": tt.value})
				if err != nil {
					t.Fatalf("Marshal() error = %v", err)
				}
			}

			res := sendRequest(t, server, queryMethod, "/db/values", string(body), "application/json")
			var got idPage
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if err := res.Body.Close(); err != nil {
				t.Errorf("Response.Body.Close() error = %v", err)
			}
			slices.Sort(tt.want)
			if !slices.Equal(got.Documents, tt.want) || got.Cursor != "" {
				t.Errorf("page = %+v, want documents %v", got, tt.want)
			}
		})
	}
}

func TestPathQueryIsSafeHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"users"}`)
	id := createQueryDoc(t, server, "/db/users", `{"active":true}`)

	body := `{"path":"$.active","value":true}`
	for range 2 {
		res = sendRequest(t, server, queryMethod, "/db/users", body, "application/json")
		checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+id+`"],"cursor":""}`)
	}

	res = sendRequest(t, server, http.MethodGet, "/db/users/"+id, "", "")
	checkResponse(t, res, http.StatusOK, `{"active":true}`)
}

func TestQueryThroughReverseProxy(t *testing.T) {
	t.Parallel()

	upstream := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, upstream, http.MethodPost, "/db", `{"name":"users"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"users"}`)
	id := createQueryDoc(t, upstream, "/db/users", `{"active":true}`)

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	corsProxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") == queryMethod {
			w.Header().Set("Access-Control-Allow-Origin", "https://client.example")
			w.Header().Set("Access-Control-Allow-Methods", queryMethod)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		proxy.ServeHTTP(w, r)
	})
	server := httptest.NewServer(corsProxy)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodOptions, server.URL+"/db/users", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", queryMethod)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Allow") != "GET, POST, QUERY, DELETE, OPTIONS" || res.Header.Get("Access-Control-Allow-Methods") != queryMethod {
		t.Errorf("preflight status = %d, headers = %v", res.StatusCode, res.Header)
	}
	if err := res.Body.Close(); err != nil {
		t.Errorf("Response.Body.Close() error = %v", err)
	}

	res = sendRequest(t, server, queryMethod, "/db/users", `{"path":"$.active","value":true}`, "application/json")
	checkQueryIDs(t, res, http.StatusOK, `{"documents":["`+id+`"],"cursor":""}`)
}
