package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

func TestQueryThroughReverseProxy(t *testing.T) {
	t.Parallel()

	upstream := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, upstream, http.MethodPost, "/db", `{"name":"users"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"users"}`)
	postQueryDoc(t, upstream, `{"active":true}`)

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

	res = sendRequest(t, server, queryMethod, "/db/users", `select(.active)`, "text/plain")
	checkQueryResponse(t, res, http.StatusOK, `[{"active":true}]`)
}
