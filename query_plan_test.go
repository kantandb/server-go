package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/itchyny/gojq"
)

func TestPlanQuery(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		program, path string
		op            cmpOp
		value         any
	}{
		{`select(.salary > 100000) | .name`, "/salary", cmpGT, json.Number("100000")},
		{`select(.category == 42)`, "/category", cmpEq, json.Number("42")},
		{`select(."a/b~c" <= "yes") | .`, "/a~1b~0c", cmpLE, "yes"},
		{`select(.salary >= -1.5)`, "/salary", cmpGE, json.Number("-1.5")},
		{`select(.salary < 100)`, "/salary", cmpLT, json.Number("100")},
		{`select(.active == true)`, "/active", cmpEq, true},
		{program: `select(.salary == null)`},
		{program: `select(.salary > 1e1000000000)`},
		{program: `select(.salary > 100000) | input`},
		{program: `select(.salary > 100000) | [inputs]`},
		{program: `def select(f): .; select(.salary > 100000) | .name`},
		{program: `.job | select(.salary > 100000) | .name`},
		{program: `(select(.salary > 100000) | .name)`},
		{program: `select(.salary > 100000), .name`},
		{program: `select(.salary > 100000) as $doc | .name`},
	} {
		t.Run(tt.program, func(t *testing.T) {
			t.Parallel()

			query, err := gojq.Parse(tt.program)
			if err != nil {
				t.Fatal(err)
			}
			plan := planQuery(query)
			if tt.path == "" {
				if len(plan) != 0 {
					t.Fatalf("plan = %+v, want full scan", plan)
				}

				return
			}
			if len(plan) != 1 || plan[0].path != tt.path || plan[0].op != tt.op {
				t.Fatalf("plan = %+v, want path %q and op %v", plan, tt.path, tt.op)
			}
			want, err := encodeIndexValue(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(plan[0].encoded) != string(want) {
				t.Errorf("index key = %x, want %x", plan[0].encoded, want)
			}
		})
	}
}

func TestPlanPredicates(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		predicate string
		want      int
	}{
		{`.age >= 30 and .age <= 40`, 1},
		{`.age < 30 or .age > 40`, 2},
		{`.dept | IN("hr", "sales", "hr")`, 3},
		{`.name | startswith("A")`, 1},
		{`.name | endswith("e")`, 1},
		{`.name | contains("ar")`, 1},
		{`.name | test("^A.*e$"; "i")`, 1},
		{`.name | test("[")`, 0},
		{`.name | test("a"; "bad")`, 0},
		{`.name | test(input)`, 0},
		{`.name | contains(.other)`, 0},
		{`.name | startswith("A\(input)")`, 0},
		{`.dept | IN("hr", input)`, 0},
		{`.age < 30 or .active`, 0},
		{`.age >= 30 and .salary <= 40`, 0},
		{`.age >= 30 and .age <= "40"`, 0},
		{`.dept | IN(` + strings.Repeat(`"hr", `, maxPlanScans) + `"hr")`, 0},
	} {
		query, err := gojq.Parse("select(" + tt.predicate + ")")
		if err != nil {
			t.Fatal(err)
		}
		if plan := planQuery(query); len(plan) != tt.want {
			t.Errorf("plan for %s has %d scans, want %d", tt.predicate, len(plan), tt.want)
		}
	}
}

func TestIndexedPredicatesHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	const database = `{"name":"users","indices":[{"name":"age","path":"/age"},{"name":"dept","path":"/dept"},{"name":"name","path":"/name"}]}`
	checkResponse(t, sendRequest(t, server, http.MethodPost, "/db", database, "application/json"), http.StatusCreated, database)
	for _, body := range []string{
		`{"id":1,"age":40,"dept":"hr","name":"A\u0000na"}`,
		`{"id":2,"age":25,"dept":"it","name":"Béa"}`,
		`{"id":3,"age":30,"dept":"sales","name":"A"}`,
		`{"id":4,"age":41,"dept":"it","name":"Alex"}`,
		`{"id":5,"age":35,"dept":"hr","name":"Diego","active":true}`,
	} {
		postQueryDoc(t, server, body)
	}
	for _, tt := range []struct{ predicate, want string }{
		{`.age >= 30 and .age <= 40`, `[1,3,5]`},
		{`.age < 40 and .age > 30`, `[5]`},
		{`.age > 40 and .age <= 30`, `[]`},
		{`.age < 30 or .age > 40`, `[2,4]`},
		{`.age >= 40 or .dept == "hr"`, `[1,4,5]`},
		{`.age < 30 or .active == true`, `[2,5]`},
		{`.dept | IN("sales", "hr", "hr")`, `[1,3,5]`},
		{`.name | startswith("A")`, `[1,3,4]`},
		{`.name | startswith("A\u0000")`, `[1]`},
		{`.name | startswith("")`, `[1,2,3,4,5]`},
	} {
		program := "select(" + tt.predicate + ") | .id"
		checkResponse(t, sendRequest(t, server, queryMethod, "/db/users", program, queryMediaType), http.StatusOK, tt.want)
	}
}

func TestIndexStringFilters(t *testing.T) {
	t.Parallel()

	s := testStore(t)
	if err := s.createDB("users", indexDef{name: "name", path: "/name"}); err != nil {
		t.Fatal(err)
	}
	for id, body := range map[string]string{
		indexTestIDA: `{"name":"Anne","id":1}`,
		indexTestIDB: `{"name":"Bob","id":2}`,
		indexTestIDC: `{"name":"A\u0000rie","id":3}`,
	} {
		if _, err := s.createDocWithID("users", id, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	// Leave Bob's index entry intact. A plan that fetches every string document fails here.
	if err := s.db.Set(docKey("users", indexTestIDB), []byte("bad record"), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ program, want string }{
		{`select(.name | endswith("e")) | .id`, `[1,3]`},
		{`select(.name | contains("\u0000r")) | .id`, `[3]`},
		{`select(.name | test("^a.*e$"; "i")) | .id`, `[1,3]`},
		{`select((.name | endswith("e")) or .name == "Anne") | .id`, `[1,3]`},
		{`select(.name | test("^Z")) | .id`, `[]`},
	} {
		result, err := s.queryArray(t.Context(), "users", []byte(tt.program))
		if err != nil || string(result) != tt.want {
			t.Errorf("%s = %s, %v; want %s", tt.program, result, err, tt.want)
		}
	}
}

func TestIndexedQueryHTTP(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	const database = `{"name":"users","indices":[{"name":"pay","path":"/salary"}]}`
	checkResponse(t, sendRequest(t, server, http.MethodPost, "/db", database, "application/json"), http.StatusCreated, database)
	for _, body := range []string{
		`{"salary":140000,"name":"Ana"}`,
		`{"salary":90000,"name":"Ben"}`,
		`{"salary":120000,"name":"Cara"}`,
		`{"salary":"90000","name":"Dan"}`,
		`{"salary":100000,"name":"Eli"}`,
	} {
		postQueryDoc(t, server, body)
	}

	const query = `select(.salary > 100000) | .name`
	checkResponse(t, sendRequest(t, server, queryMethod, "/db/users", query, queryMediaType), http.StatusOK, `["Ana","Cara"]`)
	res := sendNDJSONQuery(t, server, "/db/users", query)
	checkResponse(t, res, http.StatusOK, "\"Ana\"\n\"Cara\"\n")
	if got := res.Trailer.Get(queryStatus); got != "ok" {
		t.Errorf("Query-Status = %q, want ok", got)
	}
	checkResponse(t, sendRequest(t, server, queryMethod, "/db/users", `(`+query+`)`, queryMediaType), http.StatusOK, `["Ana","Cara","Dan"]`)
	checkResponse(t, sendRequest(t, server, queryMethod, "/db/users", `select(.salary == 90000) | .name`, queryMediaType), http.StatusOK, `["Ben"]`)
	checkResponse(t, sendRequest(t, server, queryMethod, "/db/users", `select(.salary > 100000) | [.name, (input | .name)]`, queryMediaType), http.StatusOK, `[["Ana","Ben"],["Cara","Dan"]]`)
}
