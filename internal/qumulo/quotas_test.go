package qumulo

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

func TestEnsureQuotaAtLeastNeverShrinksAndUsesETag(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	limit := int64(4096)
	putCalls := 0
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			w.Header().Set(headerETag, `"quota-1"`)
			writeProtocolJSON(t, w, http.StatusOK, map[string]string{"id": "42", "limit": formatInt64(limit)})
		case http.MethodPut:
			putCalls++
			if got := r.Header.Get(headerIfMatch); got != `"quota-1"` {
				t.Errorf("If-Match = %q", got)
			}
			var request Quota
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode quota: %v", err)
			}
			limit = request.Limit
			writeProtocolJSON(t, w, http.StatusOK, request)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	if got, err := conn.EnsureQuotaAtLeast(context.Background(), "42", 1024); err != nil || got != 4096 {
		t.Fatal(err)
	}
	if putCalls != 0 || limit != 4096 {
		t.Fatalf("smaller request changed quota: calls=%d limit=%d", putCalls, limit)
	}
	if got, err := conn.EnsureQuotaAtLeast(context.Background(), "42", 8192); err != nil || got != 8192 {
		t.Fatal(err)
	}
	if putCalls != 1 || limit != 8192 {
		t.Fatalf("larger request was not applied: calls=%d limit=%d", putCalls, limit)
	}
}

func TestEnsureQuotaAtLeastCreatesMissingQuota(t *testing.T) {
	t.Parallel()

	exists := false
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if !exists {
				writeProtocolJSON(t, w, http.StatusNotFound, APIError{ErrorClass: ErrClassRESTNotFound})
				return
			}
			writeProtocolJSON(t, w, http.StatusOK, map[string]string{"id": "7", "limit": "2048"})
		case http.MethodPost:
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode quota: %v", err)
			}
			if request["id"] != "7" || request["limit"] != "2048" {
				t.Errorf("unexpected request: %#v", request)
			}
			exists = true
			writeProtocolJSON(t, w, http.StatusOK, request)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	if got, err := conn.EnsureQuotaAtLeast(context.Background(), "7", 2048); err != nil || got != 2048 {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("quota was not created")
	}
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
