package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkBulkExport(b *testing.B) {
	const database, count = "export", 1000
	s, err := openStore(b.TempDir(), testMasterKey)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.close(); err != nil {
			b.Error(err)
		}
	})
	if err := s.createDB(database); err != nil {
		b.Fatal(err)
	}

	doc := []byte(`{"category":3,"payload":"` + strings.Repeat("x", 1024) + `"}`)
	docs := make([][]byte, count)
	for i := range docs {
		docs[i] = doc
	}
	if _, err := s.importDocs(b.Context(), database, docs, defaultBulkConfig().maxBatchBytes); err != nil {
		b.Fatal(err)
	}
	want := bytes.Repeat(append(bytes.Clone(doc), '\n'), count)

	a := newAPI(s, defaultMaxBodyBytes, discardLogger())
	for _, tc := range []struct{ name, program string }{{"jq", "(.)"}, {"direct", "."}} {
		b.Run(tc.name, func(b *testing.B) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				a.streamQuery(w, r, database, []byte(tc.program), nil)
			}))
			b.Cleanup(server.Close)

			// Compare response bytes before timing either path.
			var body bytes.Buffer
			readExport(b, server, &body, int64(len(want)))
			if !bytes.Equal(body.Bytes(), want) {
				b.Fatal("export did not match the stored documents")
			}

			b.SetBytes(int64(len(want)))
			b.ReportAllocs()
			for b.Loop() {
				readExport(b, server, io.Discard, int64(len(want)))
			}
		})
	}
}

func readExport(b *testing.B, server *httptest.Server, dst io.Writer, size int64) {
	b.Helper()

	req, err := http.NewRequestWithContext(b.Context(), queryMethod, server.URL, nil)
	if err != nil {
		b.Fatal(err)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		b.Fatal(err)
	}

	n, err := io.Copy(dst, res.Body)
	closeErr := res.Body.Close()
	if err != nil || closeErr != nil {
		b.Fatalf("read export: %v; close: %v", err, closeErr)
	}
	if status := res.Trailer.Get(queryStatus); res.StatusCode != http.StatusOK || status != "ok" || n != size {
		b.Fatalf("export status = %d, trailer = %q, bytes = %d; want 200, ok, %d", res.StatusCode, status, n, size)
	}
}
