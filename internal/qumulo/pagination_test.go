package qumulo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestListAccessKeysFollowsPagingNextAcrossShortPages(t *testing.T) {
	var calls atomic.Int32
	conn := newPaginationTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/s3/access-keys/" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		switch calls.Add(1) {
		case 1:
			if got := r.URL.Query().Get("after"); got != "" {
				t.Errorf("first after = %q, want empty", got)
			}
			writePaginationJSON(t, w, map[string]any{
				"entries": []map[string]any{{"access_key_id": "key-1"}},
				"paging": map[string]any{
					"next": "https://different.example/v1/s3/access-keys/?limit=1&after=cursor%2B1",
				},
			})
		case 2:
			if got := r.URL.Query().Get("after"); got != "cursor+1" {
				t.Errorf("second after = %q, want cursor+1", got)
			}
			writePaginationJSON(t, w, map[string]any{
				"entries": []map[string]any{{"access_key_id": "key-2"}},
				"paging":  map[string]any{"next": nil},
			})
		default:
			t.Errorf("unexpected extra access-key page request")
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	keys, err := conn.ListAccessKeys(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("page requests = %d, want 2", calls.Load())
	}
	if len(keys) != 2 || keys[0].AccessKeyID != "key-1" || keys[1].AccessKeyID != "key-2" {
		t.Fatalf("keys = %#v, want key-1 and key-2", keys)
	}
}

func TestListAccessKeysRejectsPaginationNoProgress(t *testing.T) {
	var calls atomic.Int32
	conn := newPaginationTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 2 && r.URL.Query().Get("after") != "repeat" {
			t.Errorf("second after = %q, want repeat", r.URL.Query().Get("after"))
		}
		writePaginationJSON(t, w, map[string]any{
			"entries": []map[string]any{{"access_key_id": "key"}},
			"paging":  map[string]any{"next": "/v1/s3/access-keys/?after=repeat"},
		})
	})

	_, err := conn.ListAccessKeys(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("error = %v, want pagination no-progress error", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("page requests = %d, want 2", calls.Load())
	}
}

func TestListAccessKeysRejectsNextURIWithoutCursor(t *testing.T) {
	conn := newPaginationTestConnection(t, func(w http.ResponseWriter, _ *http.Request) {
		writePaginationJSON(t, w, map[string]any{
			"entries": []map[string]any{{"access_key_id": "key"}},
			"paging":  map[string]any{"next": "/v1/s3/access-keys/?limit=100"},
		})
	})

	_, err := conn.ListAccessKeys(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "no after cursor") {
		t.Fatalf("error = %v, want missing-cursor error", err)
	}
}

func TestListUploadsFollowsPagingNextAcrossShortPages(t *testing.T) {
	var calls atomic.Int32
	conn := newPaginationTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/s3/buckets/test-bucket/uploads/" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		switch calls.Add(1) {
		case 1:
			if got := r.URL.Query().Get("after"); got != "" {
				t.Errorf("first after = %q, want empty", got)
			}
			writePaginationJSON(t, w, map[string]any{
				"uploads": []map[string]any{{"id": "upload-1"}},
				"paging": map[string]any{
					"next": "https://different.example/v1/s3/buckets/ignored/uploads/?after=upload%2Fcursor&limit=1",
				},
			})
		case 2:
			if got := r.URL.Query().Get("after"); got != "upload/cursor" {
				t.Errorf("second after = %q, want upload/cursor", got)
			}
			writePaginationJSON(t, w, map[string]any{
				"uploads": []map[string]any{{"id": "upload-2"}},
				"paging":  map[string]any{"next": nil},
			})
		default:
			t.Errorf("unexpected extra upload page request")
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	uploads, err := conn.ListUploads(context.Background(), "test-bucket")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("page requests = %d, want 2", calls.Load())
	}
	if len(uploads) != 2 || uploads[0].UploadID != "upload-1" || uploads[1].UploadID != "upload-2" {
		t.Fatalf("uploads = %#v, want upload-1 and upload-2", uploads)
	}
}

func TestListUploadsRejectsPaginationNoProgress(t *testing.T) {
	var calls atomic.Int32
	conn := newPaginationTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 2 && r.URL.Query().Get("after") != "repeat" {
			t.Errorf("second after = %q, want repeat", r.URL.Query().Get("after"))
		}
		writePaginationJSON(t, w, map[string]any{
			"uploads": []map[string]any{{"id": "upload"}},
			"paging":  map[string]any{"next": "/v1/s3/buckets/test-bucket/uploads/?after=repeat"},
		})
	})

	_, err := conn.ListUploads(context.Background(), "test-bucket")
	if err == nil || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("error = %v, want pagination no-progress error", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("page requests = %d, want 2", calls.Load())
	}
}

func newPaginationTestConnection(t *testing.T, handler http.HandlerFunc) *Connection {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	conn, err := NewConnection(DialConfig{
		Endpoint:    srv.URL,
		Credentials: Credentials{Token: "token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func writePaginationJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encode test response: %v", err)
	}
}
