package main

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestQueryCursor(t *testing.T) {
	t.Parallel()

	key := bytes.Repeat([]byte{1}, 32)
	box, err := pageCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	defs := []indexDef{{name: "age", path: "/age"}}
	binding, err := pageBinding("users", []byte(".age"), "age", defs)
	if err != nil {
		t.Fatal(err)
	}
	position := []byte("index entry with a private value")
	token, err := encodeQueryCursor(box, binding, position)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeQueryCursor(box, binding, token)
	if err != nil || !bytes.Equal(got, position) {
		t.Fatalf("cursor round trip = %q, %v", got, err)
	}
	for _, tc := range []struct {
		database, program, index string
		defs                     []indexDef
	}{
		{"other", ".age", "age", defs},
		{"users", ".name", "age", defs},
		{"users", ".age", "", defs},
		{"users", ".age", "age", []indexDef{{name: "age", path: "/other"}}},
	} {
		other, err := pageBinding(tc.database, []byte(tc.program), tc.index, tc.defs)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeQueryCursor(box, other, token); !errors.Is(err, errQueryCursor) {
			t.Fatalf("accepted mismatched cursor: %+v, %v", tc, err)
		}
	}
	otherBox, err := pageCipher(bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeQueryCursor(otherBox, binding, token); !errors.Is(err, errQueryCursor) {
		t.Fatalf("accepted cursor from another database generation: %v", err)
	}
	for _, invalid := range []string{"", "bad", token[:len(token)-4], "!" + token[1:], strings.Repeat("a", maxQueryCursor+1)} {
		if _, err := decodeQueryCursor(box, binding, invalid); !errors.Is(err, errQueryCursor) {
			t.Fatalf("accepted invalid cursor: %v", err)
		}
	}
}

func TestParseQueryPage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		limits, cursors []string
		wantLimit       int
		wantErr         error
	}{
		{wantLimit: 0},
		{limits: []string{"1"}, wantLimit: 1},
		{limits: []string{"1000"}, wantLimit: 1000},
		{cursors: []string{"token"}, wantLimit: 100},
		{limits: []string{""}, wantErr: errPageLimit},
		{limits: []string{"1", "2"}, wantErr: errPageLimit},
		{limits: []string{"1, 2"}, wantErr: errPageLimit},
		{cursors: []string{""}, wantErr: errQueryCursor},
		{cursors: []string{"a", "b"}, wantErr: errQueryCursor},
		{cursors: []string{strings.Repeat("a", maxQueryCursor+1)}, wantErr: errQueryCursor},
	} {
		header := make(http.Header)
		for _, value := range tc.limits {
			header.Add("Limit", value)
		}
		for _, value := range tc.cursors {
			header.Add("Cursor", value)
		}
		page, err := parseQueryPage(header)
		if !errors.Is(err, tc.wantErr) {
			t.Fatalf("parseQueryPage(%v): %v", header, err)
		}
		if err != nil {
			continue
		}
		if tc.wantLimit == 0 {
			if page != nil {
				t.Fatal("pagination enabled without headers")
			}
		} else if page == nil || page.limit != tc.wantLimit {
			t.Fatalf("parseQueryPage(%v) = %+v, want limit %d", header, page, tc.wantLimit)
		}
	}
}
