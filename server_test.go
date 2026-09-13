package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWelcome(t *testing.T) {
	t.Parallel()

	store, err := openStore(t.TempDir(), testMasterKey)
	if err != nil {
		t.Fatalf("openStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Errorf("store.close() error = %v", err)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	newHandler(store, defaultMaxBodyBytes).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", res.Code, http.StatusOK)
	}
	want := fmt.Sprintf(`{"name":"KantanDB","version":%q}`, buildVersion)
	if got := res.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestResponseWriters(t *testing.T) {
	t.Parallel()

	t.Run("JSON", func(t *testing.T) {
		res := httptest.NewRecorder()
		writeJSON(res, http.StatusCreated, serviceInfo{Name: "KantanDB", Version: "test"})

		if res.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d", res.Code, http.StatusCreated)
		}
		if got := res.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Errorf("Content-Type = %q, want application/json; charset=utf-8", got)
		}
		if got := res.Body.String(); got != `{"name":"KantanDB","version":"test"}` {
			t.Errorf("body = %q", got)
		}
	})

	t.Run("data", func(t *testing.T) {
		res := httptest.NewRecorder()
		writeData(res, http.StatusOK, "application/json", []byte(`{"ok":true}`))

		if res.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", res.Code, http.StatusOK)
		}
		if got := res.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := res.Body.String(); got != `{"ok":true}` {
			t.Errorf("body = %q", got)
		}
	})
}

func TestBulkSlots(t *testing.T) {
	t.Parallel()

	a := newAPIWithBulk(nil, defaultMaxBodyBytes, bulkConfig{concurrency: 1}, slog.New(slog.DiscardHandler))
	if !a.takeBulkSlot() {
		t.Fatal("first bulk slot was unavailable")
	}
	if a.takeBulkSlot() {
		t.Fatal("bulk concurrency limit was not enforced")
	}

	a.freeBulkSlot()
	if !a.takeBulkSlot() {
		t.Fatal("released bulk slot remained unavailable")
	}
	a.freeBulkSlot()
}

func TestHealth(t *testing.T) {
	t.Parallel()

	store, err := openStore(t.TempDir(), testMasterKey)
	if err != nil {
		t.Fatalf("openStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Errorf("store.close() error = %v", err)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	res := httptest.NewRecorder()
	newHandler(store, defaultMaxBodyBytes).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if got := res.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	if got := res.Body.String(); got != "{\"status\":\"ok\"}" {
		t.Errorf("body = %q, want %q", got, "{\"status\":\"ok\"}")
	}
}
