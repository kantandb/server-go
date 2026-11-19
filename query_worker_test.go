package main

import (
	"net/http"
	"testing"
)

func TestQueryWorkerMemory(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	checkResponse(t, sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json"), http.StatusCreated, `{"name":"users"}`)
	postQueryDoc(t, server, `{}`)

	const grow = `reduce range(0; 300) as $i ([]; . + [("x" * 1048576) + ($i | tostring)]) | length`
	res := sendRequest(t, server, queryMethod, "/db/users", grow, queryMediaType)
	checkResponse(t, res, http.StatusServiceUnavailable, `{"error":{"code":"query_limit","message":"Query exceeded its memory limit"}}`)

	res = sendNDJSONQuery(t, server, "/db/users", `1, (`+grow+`)`)
	checkResponse(t, res, http.StatusOK, "1\n")
	if got := res.Trailer.Get(queryStatus); got != "error" {
		t.Fatalf("Query-Status = %q, want error", got)
	}

	// The same server can still execute jq after both workers exit.
	res = sendRequest(t, server, queryMethod, "/db/users", "42", queryMediaType)
	checkResponse(t, res, http.StatusOK, "[42]")
}
