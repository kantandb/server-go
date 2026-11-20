package main

import (
	"net/http"
	"testing"

	"github.com/itchyny/gojq"
)

func TestPlanDirect(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		program string
		want    *directPlan
	}{
		{`.`, &directPlan{}},
		{`.category`, &directPlan{field: "category", project: true}},
		{`.""`, &directPlan{project: true}},
		{`."a\u003cb"`, &directPlan{field: "a<b", project: true}},
		{`(.)`, nil},
		{`.items[]`, nil},
		{`.items.name`, nil},
		{`."\(.key)"`, nil},
		{`.category | length`, nil},
		{`select(.category == 3)`, nil},
	} {
		q, err := gojq.Parse(tc.program)
		if err != nil {
			t.Fatal(err)
		}
		got := planDirect(q)
		if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Errorf("planDirect(%q) = %+v, want %+v", tc.program, got, tc.want)
		}
	}
}

func TestDirectQueryHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	checkResponse(t, sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json"), http.StatusCreated, `{"name":"users"}`)
	const doc = `{"":false,"a\u003cb":[{"category":"nested"},"a\"b"],"category":0.10000000000000001}`
	postQueryDoc(t, server, doc)
	postQueryDoc(t, server, `{}`)

	for _, tc := range []struct{ program, first, second string }{
		{`.`, doc, `{}`},
		{`.category`, `0.10000000000000001`, `null`},
		{`."a<b"`, `[{"category":"nested"},"a\"b"]`, `null`},
		{`.""`, `false`, `null`},
		{`.missing`, `null`, `null`},
		{`(.category)`, `0.10000000000000001`, `null`},
	} {
		checkQueryResponse(t, sendRequest(t, server, queryMethod, "/db/users", tc.program, queryMediaType), http.StatusOK, "["+tc.first+","+tc.second+"]")
		res := sendNDJSONQuery(t, server, "/db/users", tc.program)
		checkQueryResponse(t, res, http.StatusOK, tc.first+"\n"+tc.second+"\n")
		if got := res.Trailer.Get(queryStatus); got != "ok" {
			t.Errorf("Query-Status for %q = %q, want ok", tc.program, got)
		}
	}
}
