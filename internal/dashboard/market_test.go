package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFetchPluginMarket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"$meta":{"name":"test"},"echo":{"name":"echo","version":"1.0.0"}}`))
	}))
	defer srv.Close()

	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	data, err := s.fetchPluginMarket(srv.URL, false)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	m, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", data)
	}
	if _, ok := m["$meta"]; !ok {
		t.Errorf("missing $meta")
	}
	entry, ok := m["echo"].(map[string]interface{})
	if !ok || entry["version"] != "1.0.0" {
		t.Errorf("unexpected echo entry: %v", m["echo"])
	}
}

func TestFetchPluginMarketCaching(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":{"name":"a"}}`))
	}))
	defer srv.Close()

	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	if _, err := s.fetchPluginMarket(srv.URL, false); err != nil {
		t.Fatalf("first fetch failed: %v", err)
	}
	if _, err := s.fetchPluginMarket(srv.URL, false); err != nil {
		t.Fatalf("second fetch failed: %v", err)
	}
	mu.Lock()
	if hits != 1 {
		t.Errorf("expected 1 upstream hit due to cache, got %d", hits)
	}
	mu.Unlock()

	if _, err := s.fetchPluginMarket(srv.URL, true); err != nil {
		t.Fatalf("force refresh failed: %v", err)
	}
	mu.Lock()
	if hits != 2 {
		t.Errorf("expected 2 upstream hits after force refresh, got %d", hits)
	}
	mu.Unlock()
}

func TestFetchPluginMarketFallbackToCache(t *testing.T) {
	var mu sync.Mutex
	serve := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := serve
		mu.Unlock()
		if !ok {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"b":{"name":"b"}}`))
	}))
	defer srv.Close()

	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	if _, err := s.fetchPluginMarket(srv.URL, false); err != nil {
		t.Fatalf("first fetch failed: %v", err)
	}

	// Upstream now failing; must fall back to cached data.
	s.marketCache[srv.URL].fetchedAt = time.Now().Add(-time.Hour) // force refetch attempt
	mu.Lock()
	serve = false
	mu.Unlock()

	data, err := s.fetchPluginMarket(srv.URL, false)
	if err != nil {
		t.Fatalf("expected cached fallback, got error: %v", err)
	}
	if m, ok := data.(map[string]interface{}); !ok || m["b"] == nil {
		t.Errorf("expected cached data fallback, got: %v", data)
	}
}

func TestDecodeMarketBody(t *testing.T) {
	raw, err := decodeMarketBody(strings.NewReader(`{"$meta":{},"echo":{"name":"echo"}}`))
	if err != nil {
		t.Fatalf("plain object decode failed: %v", err)
	}
	if m, ok := raw.(map[string]interface{}); !ok || m["echo"] == nil {
		t.Errorf("expected plain object passthrough, got: %v", raw)
	}

	wrapped, err := decodeMarketBody(strings.NewReader(`{"timestamp":"x","data":{"echo":{"name":"echo"}}}`))
	if err != nil {
		t.Fatalf("wrapped decode failed: %v", err)
	}
	if m, ok := wrapped.(map[string]interface{}); !ok || m["echo"] == nil {
		t.Errorf("expected wrapped data extraction, got: %v", wrapped)
	}

	if _, err := decodeMarketBody(strings.NewReader(`not json`)); err == nil {
		t.Errorf("expected decode error for invalid json")
	}
}
