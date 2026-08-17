package qumulo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestEnsureDirectoryCreatesParentsAndMode(t *testing.T) {
	var mu sync.Mutex
	files := map[string]FileAttributes{
		"/": {ID: "1", Path: "/", Mode: "0755"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		ref, suffix := testFileRef(r.URL.Path)
		switch {
		case r.Method == http.MethodGet && suffix == "/info/attributes":
			attrs, ok := files[ref]
			if !ok {
				writeTestAPIError(w, http.StatusNotFound, ErrClassFSNoSuchEntry)
				return
			}
			_ = json.NewEncoder(w).Encode(attrs)
		case r.Method == http.MethodPost && suffix == "/entries/":
			var in createDirectoryRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			child := joinFSPath(ref, in.Name)
			attrs := FileAttributes{ID: "id-" + in.Name, Path: child, Name: in.Name, Mode: "0755"}
			files[child] = attrs
			_ = json.NewEncoder(w).Encode(attrs)
		case r.Method == http.MethodPatch && suffix == "/info/attributes":
			attrs := files[ref]
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			attrs.Mode = in["mode"]
			files[ref] = attrs
			_ = json.NewEncoder(w).Encode(attrs)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)

	conn, err := NewConnection(DialConfig{Endpoint: srv.URL, Credentials: Credentials{Token: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := conn.EnsureDirectory(context.Background(), "/k8s/nfs/claim-a", "0777")
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Path != "/k8s/nfs/claim-a" || attrs.Mode != "0777" {
		t.Fatalf("unexpected attributes: %#v", attrs)
	}
	// A second call must be a read/reconcile operation, not another create.
	if _, err := conn.EnsureDirectory(context.Background(), "/k8s/nfs/claim-a", "0777"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDirectoryRejectsUnsafePath(t *testing.T) {
	for _, raw := range []string{"", "relative/path", "/bad\npath"} {
		if _, err := cleanFSPath(raw); err == nil {
			t.Fatalf("cleanFSPath(%q) unexpectedly succeeded", raw)
		}
	}
}

func testFileRef(rawPath string) (string, string) {
	rest := strings.TrimPrefix(rawPath, "/v1/files/")
	for _, suffix := range []string{"/info/attributes", "/entries/"} {
		if strings.HasSuffix(rest, suffix) {
			raw := strings.TrimSuffix(rest, suffix)
			decoded, _ := url.PathUnescape(raw)
			if !strings.HasPrefix(decoded, "/") {
				decoded = "/" + decoded
			}
			return decoded, suffix
		}
	}
	return rest, ""
}

func writeTestAPIError(w http.ResponseWriter, status int, class string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIError{ErrorClass: class, Description: class})
}
