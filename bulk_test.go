package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestIsNDJSON(t *testing.T) {
	t.Parallel()

	for _, value := range []string{bulkMediaType, bulkMediaType + "; charset=utf-8"} {
		if !isNDJSON(value) {
			t.Errorf("isNDJSON(%q) = false", value)
		}
	}
	for _, value := range []string{"", "application/json", bulkMediaType + "; bad"} {
		if isNDJSON(value) {
			t.Errorf("isNDJSON(%q) = true", value)
		}
	}
}

func TestReadBulk(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		maxLine   int64
		maxBytes  int64
		maxDocs   int
		want      [][]byte
		wantError error
		wantLine  int
	}{
		{name: "LF", body: "{\"b\":2,\"a\":1}\n{\"ok\":true}\n", maxLine: 100, maxBytes: 100, maxDocs: 2, want: byteRows(`{"a":1,"b":2}`, `{"ok":true}`)},
		{name: "CRLF", body: "{\"a\":1}\r\n{\"b\":2}\r\n", maxLine: 7, maxBytes: 20, maxDocs: 2, want: byteRows(`{"a":1}`, `{"b":2}`)},
		{name: "final line", body: "{\"a\":1}", maxLine: 7, maxBytes: 7, maxDocs: 1, want: byteRows(`{"a":1}`)},
		{name: "empty", maxLine: 10, maxBytes: 10, maxDocs: 1, wantError: errBulkEmpty},
		{name: "blank", body: "\n", maxLine: 10, maxBytes: 10, maxDocs: 1, wantError: errInvalidDoc, wantLine: 1},
		{name: "blank second", body: "{}\n\n", maxLine: 10, maxBytes: 10, maxDocs: 2, wantError: errInvalidDoc, wantLine: 2},
		{name: "malformed", body: "{}\n{\"a\":\n", maxLine: 20, maxBytes: 20, maxDocs: 2, wantError: errInvalidDoc, wantLine: 2},
		{name: "array", body: "[]", maxLine: 10, maxBytes: 10, maxDocs: 1, wantError: errInvalidDoc, wantLine: 1},
		{name: "trailing value", body: "{} {}", maxLine: 10, maxBytes: 10, maxDocs: 1, wantError: errInvalidDoc, wantLine: 1},
		{name: "raw line limit", body: "{\"a\":1}\n", maxLine: 6, maxBytes: 20, maxDocs: 1, wantError: errBulkLimit, wantLine: 1},
		{name: "canonical limit", body: "{\"x\":\"<\"}", maxLine: 10, maxBytes: 20, maxDocs: 1, wantError: errBodyTooLarge, wantLine: 1},
		{name: "total limit", body: "{}\n{}\n", maxLine: 10, maxBytes: 5, maxDocs: 2, wantError: errBulkLimit},
		{name: "record limit", body: "{}\n{}\n", maxLine: 10, maxBytes: 10, maxDocs: 1, wantError: errBulkLimit, wantLine: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := readBulk(strings.NewReader(tt.body), tt.maxLine, tt.maxBytes, tt.maxDocs)
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("readBulk() error = %v, want %v", err, tt.wantError)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("readBulk() = %q, want %q", got, tt.want)
			}

			var lineErr *bulkLineError
			if errors.As(err, &lineErr) && lineErr.line != tt.wantLine {
				t.Errorf("error line = %d, want %d", lineErr.line, tt.wantLine)
			}
			if tt.wantLine != 0 && !errors.As(err, &lineErr) {
				t.Errorf("error has no line, want %d", tt.wantLine)
			}
		})
	}
}

func TestReadBulkLongLine(t *testing.T) {
	t.Parallel()

	body := `{"value":"` + strings.Repeat("a", 64<<10) + `"}`
	got, err := readBulk(strings.NewReader(body), int64(len(body)), int64(len(body)), 1)
	if err != nil {
		t.Fatalf("readBulk() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("readBulk() returned %d documents, want 1", len(got))
	}
}

func TestReadBulkReadError(t *testing.T) {
	t.Parallel()

	_, err := readBulk(io.MultiReader(strings.NewReader("{}\n"), errorReader{err: errors.New("read failed")}), 10, 10, 2)
	if err == nil || errors.Is(err, errInvalidDoc) {
		t.Fatalf("readBulk() error = %v, want read error", err)
	}
}

func TestBulkResponseWriters(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		res := httptest.NewRecorder()
		writeBulkSuccess(res)

		if res.Code != http.StatusOK || res.Body.String() != `{"success":true,"error":{}}` {
			t.Errorf("response = %d %s", res.Code, res.Body.String())
		}
	})

	t.Run("error", func(t *testing.T) {
		res := httptest.NewRecorder()
		writeBulkError(res, http.StatusBadRequest, "invalid_document", "Document is invalid", 2)

		want := `{"success":false,"error":{"code":"invalid_document","line":2,"message":"Document is invalid"}}`
		if res.Code != http.StatusBadRequest || res.Body.String() != want {
			t.Errorf("response = %d %s, want %s", res.Code, res.Body.String(), want)
		}
	})
}

func byteRows(values ...string) [][]byte {
	rows := make([][]byte, len(values))
	for i, value := range values {
		rows[i] = []byte(value)
	}

	return rows
}
