package qumulo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTreeDeleteWaitsForDurableCompletion(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch call {
		case 1:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"42")
			writeTreeDeleteNotFound(t, w)
		case 2:
			assertTreeDeleteRequest(t, r, http.MethodPost, treeDeleteJobsPath)
			var request treeDeleteJob
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode submit request: %v", err)
			}
			if request.ID != "42" {
				t.Errorf("submitted file ID = %q, want 42", request.ID)
			}
			writeProtocolJSON(t, w, http.StatusAccepted, nil)
		case 3:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"42")
			writeProtocolJSON(t, w, http.StatusOK, map[string]any{
				"id":                 "42",
				"last_error_message": nil,
			})
		case 4:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"42")
			writeTreeDeleteNotFound(t, w)
		default:
			t.Errorf("unexpected request %d: %s %s", call, r.Method, r.URL.Path)
			writeProtocolJSON(t, w, http.StatusInternalServerError, nil)
		}
	})

	if err := conn.treeDelete(context.Background(), "42", time.Millisecond); err != nil {
		t.Fatalf("TreeDelete() error = %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("TreeDelete() made %d requests, want 4", got)
	}
}

func TestTreeDeleteResumesExistingJob(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"existing")
		switch call {
		case 1:
			writeProtocolJSON(t, w, http.StatusOK, map[string]any{
				"id":                 "existing",
				"last_error_message": nil,
			})
		case 2:
			writeTreeDeleteNotFound(t, w)
		default:
			t.Errorf("unexpected request %d", call)
			writeProtocolJSON(t, w, http.StatusInternalServerError, nil)
		}
	})

	if err := conn.treeDelete(context.Background(), "existing", time.Millisecond); err != nil {
		t.Fatalf("TreeDelete() error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("TreeDelete() made %d requests, want 2", got)
	}
}

func TestTreeDeleteWaitsAfterConcurrentSubmission(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch call {
		case 1:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"concurrent")
			writeTreeDeleteNotFound(t, w)
		case 2:
			assertTreeDeleteRequest(t, r, http.MethodPost, treeDeleteJobsPath)
			writeProtocolJSON(t, w, http.StatusConflict, APIError{
				ErrorClass:  ErrClassFSEntryExists,
				Description: "tree-delete job already exists",
			})
		case 3:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"concurrent")
			writeProtocolJSON(t, w, http.StatusOK, map[string]any{
				"id":                 "concurrent",
				"last_error_message": nil,
			})
		case 4:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"concurrent")
			writeTreeDeleteNotFound(t, w)
		default:
			t.Errorf("unexpected request %d: %s %s", call, r.Method, r.URL.Path)
			writeProtocolJSON(t, w, http.StatusInternalServerError, nil)
		}
	})

	if err := conn.treeDelete(context.Background(), "concurrent", time.Millisecond); err != nil {
		t.Fatalf("TreeDelete() error = %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("TreeDelete() made %d requests, want 4", got)
	}
}

func TestTreeDeleteSurfacesAbortedJob(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"locked")
		writeProtocolJSON(t, w, http.StatusOK, map[string]any{
			"id":                 "locked",
			"status":             "ABORTED",
			"last_error_message": "file is locked",
		})
	})

	err := conn.treeDelete(context.Background(), "locked", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "aborted") || !strings.Contains(err.Error(), "file is locked") {
		t.Fatalf("TreeDelete() error = %v, want aborted job message", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("aborted existing job made %d requests, want 1", got)
	}
}

func TestTreeDeleteTerminalErrorRecognizesExplicitAbort(t *testing.T) {
	t.Parallel()

	tests := map[string]*treeDeleteJobStatus{
		"status":  {Status: "ABORTED"},
		"boolean": {Aborted: true},
	}
	for name, status := range tests {
		t.Run(name, func(t *testing.T) {
			err := treeDeleteTerminalError("17", status)
			if err == nil || !strings.Contains(err.Error(), "aborted") {
				t.Fatalf("treeDeleteTerminalError() = %v, want aborted error", err)
			}
		})
	}
}

func TestTreeDeleteAmbiguousSubmissionRequiresObservableJob(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch call {
		case 1, 3:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"ambiguous")
			writeTreeDeleteNotFound(t, w)
		case 2:
			assertTreeDeleteRequest(t, r, http.MethodPost, treeDeleteJobsPath)
			writeProtocolJSON(t, w, http.StatusServiceUnavailable, APIError{
				ErrorClass:  "internal_error",
				Description: "response lost",
			})
		default:
			t.Errorf("unexpected request %d: %s %s", call, r.Method, r.URL.Path)
			writeProtocolJSON(t, w, http.StatusInternalServerError, nil)
		}
	})

	err := conn.treeDelete(context.Background(), "ambiguous", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "unknown outcome") {
		t.Fatalf("TreeDelete() error = %v, want unknown outcome", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("TreeDelete() made %d requests, want 3", got)
	}
}

func TestTreeDeleteAmbiguousSubmissionPollsObservableJob(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch call {
		case 1:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"committed")
			writeTreeDeleteNotFound(t, w)
		case 2:
			assertTreeDeleteRequest(t, r, http.MethodPost, treeDeleteJobsPath)
			writeProtocolJSON(t, w, http.StatusServiceUnavailable, APIError{
				ErrorClass:  "internal_error",
				Description: "response lost after commit",
			})
		case 3:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"committed")
			writeProtocolJSON(t, w, http.StatusOK, map[string]any{
				"id":                 "committed",
				"last_error_message": nil,
			})
		case 4:
			assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"committed")
			writeTreeDeleteNotFound(t, w)
		default:
			t.Errorf("unexpected request %d: %s %s", call, r.Method, r.URL.Path)
			writeProtocolJSON(t, w, http.StatusInternalServerError, nil)
		}
	})

	if err := conn.treeDelete(context.Background(), "committed", time.Millisecond); err != nil {
		t.Fatalf("TreeDelete() error = %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("TreeDelete() made %d requests, want 4", got)
	}
}

func TestTreeDeleteHonorsContextWhilePolling(t *testing.T) {
	t.Parallel()

	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		assertTreeDeleteRequest(t, r, http.MethodGet, treeDeleteJobsPath+"slow")
		writeProtocolJSON(t, w, http.StatusOK, map[string]any{
			"id":                 "slow",
			"last_error_message": nil,
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)

	err := conn.treeDelete(ctx, "slow", time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("TreeDelete() error = %v, want context.Canceled", err)
	}
}

func assertTreeDeleteRequest(t *testing.T, r *http.Request, method, path string) {
	t.Helper()
	if r.Method != method || r.URL.EscapedPath() != path {
		t.Errorf("tree-delete request = %s %s, want %s %s", r.Method, r.URL.EscapedPath(), method, path)
	}
}

func writeTreeDeleteNotFound(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeProtocolJSON(t, w, http.StatusNotFound, APIError{
		ErrorClass:  ErrClassRESTNotFound,
		Description: "tree-delete job not found",
	})
}
