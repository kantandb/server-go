package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestObjectDecoder(t *testing.T) {
	t.Parallel()

	deep := `{"x":` + strings.Repeat("[", 350) + `9007199254740993` + strings.Repeat("]", 350) + `}`
	cases := []string{
		`{"dup":null,"dup":true,"a":[{},[],false,null,"a\t\n\\b",-0,1E+400,18446744073709551616000000001]}`,
		`{"\u0061":"\uD800","a":"\uD800\u0041","pair":"\uD83D\uDE00","x":"\u003c"}`,
		deep, `{"x":01}`, `{"x":"\q"}`, "{\"x\":\"\x01\"}", `{} {}`, `null`, `[]`,
	}
	var decoder objectDecoder
	for _, body := range cases {
		var want map[string]any
		std := json.NewDecoder(strings.NewReader(body))
		std.UseNumber()
		wantErr := std.Decode(&want) != nil || want == nil || jsonEnd(std) != nil
		got, err := decoder.decode([]byte(body), false)
		if (err != nil) != wantErr || !wantErr && !reflect.DeepEqual(got, want) {
			t.Fatalf("decode(%q) = %v, %v; want %v, error=%v", body, got, err, want, wantErr)
		}
		if wantErr {
			continue
		}

		canonical, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := decoder.decode(canonical, true)
		clear(canonical)
		if err != nil || !reflect.DeepEqual(stored, want) || !reflect.DeepEqual(got, want) {
			t.Fatalf("stored = %v, %v; want %v; previous decode = %v", stored, err, want, got)
		}
	}
}

func TestDeepDocumentQuery(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, defaultMaxBodyBytes)
	res := sendRequest(t, server, http.MethodPost, "/db", `{"name":"users"}`, "application/json")
	checkResponse(t, res, http.StatusCreated, `{"name":"users"}`)

	const levels = 350
	body := `{"x":` + strings.Repeat("[", levels) + "9007199254740993" + strings.Repeat("]", levels) + "}"
	postQueryDoc(t, server, body)

	res = sendNDJSONQuery(t, server, "/db/users", ".x"+strings.Repeat("[0]", levels))
	checkQueryResponse(t, res, http.StatusOK, "9007199254740993\n")
	if got := res.Trailer.Get(queryStatus); got != "ok" {
		t.Fatalf("trailer = %q, want ok", got)
	}
}

func TestDBRequestDecoder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body string
		want string
	}{
		{`{"name":"db","indices":[]}`, `{"name":"db","indices":[]}`},
		{`{"Name":"db","INDICES":[{"Name":"idx","Path":"/x"}]}`, `{"name":"db","indices":[{"name":"idx","path":"/x"}]}`},
		{`{"name":"db","name":null}`, `{"name":"db"}`},
		{`{"name":"db","indices":[{"name":"idx","name":null,"path":"/x"}]}`, `{"name":"db","indices":[{"name":"idx","path":"/x"}]}`},
		{`{"name":"db","indices":null}`, ""},
		{`{"name":"db","indices":[{"name":"idx","path":"/x","extra":true}]}`, ""},
		{`{"name":"db","other":true}`, ""},
	}
	for _, tt := range cases {
		request, err := decodeDBRequest([]byte(tt.body))
		if tt.want == "" {
			if err == nil {
				t.Errorf("decodeDBRequest(%s) succeeded", tt.body)
			}

			continue
		}
		if err != nil {
			t.Fatalf("decodeDBRequest(%s): %v", tt.body, err)
		}
		body, err := json.Marshal(request)
		if err != nil || !bytes.Equal(body, []byte(tt.want)) {
			t.Errorf("decodeDBRequest(%s) = %s, %v; want %s", tt.body, body, err, tt.want)
		}
	}
}
