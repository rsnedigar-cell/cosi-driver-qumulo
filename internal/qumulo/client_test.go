package qumulo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Rotating any credential or TLS input must change the cache key, or the
// driver would keep using a stale cached connection after a rotation.
func TestCacheKeyRotation(t *testing.T) {
	base := CacheKey("q.example", "8000", Credentials{Token: "t1", Username: "u", Password: "p1"}, []byte("ca-1"), false)
	variants := []string{
		CacheKey("q.example", "8000", Credentials{Token: "t2", Username: "u", Password: "p1"}, []byte("ca-1"), false),
		CacheKey("q.example", "8000", Credentials{Token: "t1", Username: "u", Password: "p2"}, []byte("ca-1"), false),
		CacheKey("q.example", "8000", Credentials{Token: "t1", Username: "u2", Password: "p1"}, []byte("ca-1"), false),
		CacheKey("q.example", "8000", Credentials{Token: "t1", Username: "u", Password: "p1"}, []byte("ca-2"), false),
		CacheKey("q.example", "8000", Credentials{Token: "t1", Username: "u", Password: "p1"}, []byte("ca-1"), true),
		CacheKey("q2.example", "8000", Credentials{Token: "t1", Username: "u", Password: "p1"}, []byte("ca-1"), false),
	}
	seen := map[string]bool{base: true}
	for i, v := range variants {
		if seen[v] {
			t.Fatalf("variant %d collided with a previous key", i)
		}
		seen[v] = true
	}
	// Same inputs must be stable.
	if base != CacheKey("q.example", "8000", Credentials{Token: "t1", Username: "u", Password: "p1"}, []byte("ca-1"), false) {
		t.Fatal("cache key not deterministic")
	}
}

func TestNewConnectionRejectsUnsafeEndpointShapes(t *testing.T) {
	for _, endpoint := range []string{
		"http://qumulo.example.com:8000",
		"https://user:password@qumulo.example.com:8000",
		"https://qumulo.example.com:8000/unexpected/path",
		"https://qumulo.example.com:0",
	} {
		if _, err := NewConnection(DialConfig{Endpoint: endpoint, Credentials: Credentials{Token: "token"}}); err == nil {
			t.Errorf("NewConnection(%q) unexpectedly succeeded", endpoint)
		}
	}
}

func TestDoJSONDoesNotReplayPOSTAfterServerError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error_class":"internal_error","description":"ambiguous"}`))
	}))
	t.Cleanup(srv.Close)

	conn, err := NewConnection(DialConfig{
		Endpoint:    srv.URL,
		Credentials: Credentials{Token: "token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.DoJSON(context.Background(), http.MethodPost, "/resource", nil, nil, map[string]string{"name": "one"}, nil)
	if err == nil {
		t.Fatal("expected POST failure")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ambiguous POST was replayed %d times; want one attempt", got)
	}
}

func TestDoJSONDoesNotReplayDELETEAfterServerError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error_class":"internal_error","description":"ambiguous delete"}`))
	}))
	t.Cleanup(srv.Close)

	conn, err := NewConnection(DialConfig{
		Endpoint:    srv.URL,
		Credentials: Credentials{Token: "token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.DoJSON(context.Background(), http.MethodDelete, "/resource/by-name", nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected DELETE failure")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ambiguous DELETE was replayed %d times; want one attempt", got)
	}
}

func TestDoJSONStillRetriesReplaySafeMethod(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error_class":"internal_error","description":"retry"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	conn, err := NewConnection(DialConfig{
		Endpoint:    srv.URL,
		Credentials: Credentials{Token: "token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if _, err := conn.DoJSON(context.Background(), http.MethodGet, "/resource", nil, nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || calls.Load() != 2 {
		t.Fatalf("safe retry result: ok=%v calls=%d", out.OK, calls.Load())
	}
}

func TestDoJSONReplaysPOSTOnceAfterRelogin(t *testing.T) {
	var resourceCalls atomic.Int32
	var loginCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/session/login":
			loginCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"bearer_token":"fresh"}`))
		case "/resource":
			resourceCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error_class":"auth_invalid_credentials_error"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	conn, err := NewConnection(DialConfig{
		Endpoint: srv.URL,
		Credentials: Credentials{
			Token: "stale", Username: "admin", Password: "password",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if _, err := conn.DoJSON(context.Background(), http.MethodPost, "/resource", nil, nil, map[string]string{"name": "one"}, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || resourceCalls.Load() != 2 || loginCalls.Load() != 1 {
		t.Fatalf("reauthenticated POST: ok=%v resourceCalls=%d loginCalls=%d", out.OK, resourceCalls.Load(), loginCalls.Load())
	}
}
