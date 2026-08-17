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

func newProtocolTestConnection(t *testing.T, handler http.HandlerFunc) *Connection {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	conn, err := NewConnection(DialConfig{
		Endpoint:    server.URL,
		Credentials: Credentials{Token: "test-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func writeProtocolJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if value != nil {
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Errorf("encode test response: %v", err)
		}
	}
}

func TestNFSExportCRUD(t *testing.T) {
	t.Parallel()

	restrictions := []NFSRestriction{{
		HostRestrictions:           []string{"10.0.0.0/24"},
		RequirePrivilegedPort:      true,
		RequiredAuthenticationMode: NFSAuthNone,
		UserMapping:                NFSMapRoot,
		MapToUser:                  &NFSIdentity{IDType: NFSIdentityUID, IDValue: "65534"},
		MapToGroup:                 &NFSIdentity{IDType: NFSIdentityGID, IDValue: "65534"},
	}}
	desired := NFSExportRequest{
		ExportPath:             "/exports/team one",
		FSPath:                 "/data/team-one",
		Description:            "managed by COSI",
		Restrictions:           restrictions,
		FieldsToPresentAs32Bit: []NFS32BitField{NFS32BitFileIDs},
	}

	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch call {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != nfsExportsPath {
				t.Errorf("create request = %s %s", r.Method, r.URL.Path)
			}
			if got := r.URL.Query().Get("allow-fs-path-create"); got != "true" {
				t.Errorf("allow-fs-path-create = %q", got)
			}
			var got NFSExportRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if got.ExportPath != desired.ExportPath || len(got.Restrictions) != 1 || got.Restrictions[0].MapToUser.IDValue != "65534" {
				t.Errorf("unexpected create body: %+v", got)
			}
			w.Header().Set(headerETag, `"nfs-1"`)
			writeProtocolJSON(t, w, http.StatusOK, NFSExport{ID: "17", ExportPath: desired.ExportPath, FSPath: desired.FSPath})
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != nfsExportsPath {
				t.Errorf("list request = %s %s", r.Method, r.URL.Path)
			}
			writeProtocolJSON(t, w, http.StatusOK, []NFSExport{{ID: "17", ExportPath: desired.ExportPath, FSPath: desired.FSPath}})
		case 3:
			if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v2/nfs/exports/%2Fexports%2Fteam%20one" {
				t.Errorf("get request = %s %s (%s)", r.Method, r.URL.Path, r.URL.EscapedPath())
			}
			w.Header().Set(headerETag, `"nfs-2"`)
			writeProtocolJSON(t, w, http.StatusOK, NFSExport{ID: "17", ExportPath: desired.ExportPath, FSPath: desired.FSPath})
		case 4:
			if r.Method != http.MethodPut || r.URL.Path != "/v2/nfs/exports/17" {
				t.Errorf("replace request = %s %s", r.Method, r.URL.Path)
			}
			if got := r.Header.Get(headerIfMatch); got != `"nfs-2"` {
				t.Errorf("If-Match = %q", got)
			}
			writeProtocolJSON(t, w, http.StatusOK, NFSExport{ID: "17", ExportPath: desired.ExportPath, FSPath: desired.FSPath})
		case 5:
			if r.Method != http.MethodPatch || r.URL.Path != "/v2/nfs/exports/17" {
				t.Errorf("patch request = %s %s", r.Method, r.URL.Path)
			}
			var got map[string]any
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got["description"] != "updated" || len(got) != 1 {
				t.Errorf("unexpected patch body: %#v", got)
			}
			writeProtocolJSON(t, w, http.StatusOK, NFSExport{ID: "17", ExportPath: desired.ExportPath, FSPath: desired.FSPath, Description: "updated"})
		case 6:
			if r.Method != http.MethodDelete || r.URL.Path != "/v2/nfs/exports/17" {
				t.Errorf("delete request = %s %s", r.Method, r.URL.Path)
			}
			if got := r.Header.Get(headerIfMatch); got != `"nfs-3"` {
				t.Errorf("delete If-Match = %q", got)
			}
			writeProtocolJSON(t, w, http.StatusOK, nil)
		default:
			t.Errorf("unexpected request %d: %s %s", call, r.Method, r.URL.Path)
			writeProtocolJSON(t, w, http.StatusInternalServerError, nil)
		}
	})

	ctx := context.Background()
	created, err := conn.CreateNFSExport(ctx, desired, true)
	if err != nil || created.ID != "17" || created.ETag != `"nfs-1"` {
		t.Fatalf("CreateNFSExport() = %+v, %v", created, err)
	}
	listed, err := conn.ListNFSExports(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListNFSExports() = %+v, %v", listed, err)
	}
	got, err := conn.GetNFSExport(ctx, desired.ExportPath)
	if err != nil || got.ETag != `"nfs-2"` {
		t.Fatalf("GetNFSExport() = %+v, %v", got, err)
	}
	if _, err := conn.ReplaceNFSExport(ctx, "17", desired, NFSExportWriteOptions{IfMatch: got.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PatchNFSExport(ctx, "17", PatchNFSExportRequest{Description: ptr("updated")}, NFSExportWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := conn.DeleteNFSExport(ctx, "17", `"nfs-3"`); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 6 {
		t.Fatalf("calls = %d, want 6", calls.Load())
	}
}

func TestEnsureNFSExportReconcilesCreateConflict(t *testing.T) {
	t.Parallel()

	desired := NFSExportRequest{
		ExportPath:  "/exports/claim",
		FSPath:      "/data/claim",
		Description: "desired",
		Restrictions: []NFSRestriction{{
			HostRestrictions:           []string{"10.0.0.0/24"},
			RequiredAuthenticationMode: NFSAuthNone,
			UserMapping:                NFSMapRoot,
		}},
	}
	var gets atomic.Int32
	var posts atomic.Int32
	var patches atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if gets.Add(1) == 1 {
				writeProtocolJSON(t, w, http.StatusNotFound, map[string]any{"error_class": ErrClassRESTNotFound})
				return
			}
			w.Header().Set(headerETag, `"before"`)
			writeProtocolJSON(t, w, http.StatusOK, NFSExport{
				ID: "29", ExportPath: desired.ExportPath, FSPath: desired.FSPath,
				Description: "old", Restrictions: desired.Restrictions,
			})
		case http.MethodPost:
			posts.Add(1)
			writeProtocolJSON(t, w, http.StatusConflict, map[string]any{"description": "already exists"})
		case http.MethodPatch:
			patches.Add(1)
			if r.URL.Path != "/v2/nfs/exports/29" || r.Header.Get(headerIfMatch) != `"before"` {
				t.Errorf("conditional patch = %s If-Match %q", r.URL.Path, r.Header.Get(headerIfMatch))
			}
			var patch PatchNFSExportRequest
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if patch.Description == nil || *patch.Description != desired.Description {
				t.Errorf("patch = %+v", patch)
			}
			w.Header().Set(headerETag, `"after"`)
			writeProtocolJSON(t, w, http.StatusOK, NFSExport{ID: "29", ExportPath: desired.ExportPath, FSPath: desired.FSPath, Description: desired.Description})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	got, created, err := conn.EnsureNFSExport(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if created || got.Description != "desired" || got.ETag != `"after"` {
		t.Fatalf("EnsureNFSExport() = %+v, created=%v", got, created)
	}
	if gets.Load() != 2 || posts.Load() != 1 || patches.Load() != 1 {
		t.Fatalf("gets=%d posts=%d patches=%d", gets.Load(), posts.Load(), patches.Load())
	}
}

func TestEnsureNFSExportRefusesRetarget(t *testing.T) {
	t.Parallel()

	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		writeProtocolJSON(t, w, http.StatusOK, NFSExport{ID: "1", ExportPath: "/export", FSPath: "/someone-elses-data"})
	})
	_, _, err := conn.EnsureNFSExport(context.Background(), NFSExportRequest{ExportPath: "/export", FSPath: "/our-data"}, false)
	if err == nil || !strings.Contains(err.Error(), "refusing to retarget") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnsureNFSExportReconcilesAmbiguousCreate(t *testing.T) {
	t.Parallel()

	desired := NFSExportRequest{ExportPath: "/ambiguous", FSPath: "/data/ambiguous"}
	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeProtocolJSON(t, w, http.StatusNotFound, map[string]any{"error_class": ErrClassRESTNotFound})
		case 2:
			if r.Method != http.MethodPost {
				t.Errorf("request 2 method = %s", r.Method)
			}
			writeProtocolJSON(t, w, http.StatusInternalServerError, map[string]any{"description": "response lost after commit"})
		case 3:
			writeProtocolJSON(t, w, http.StatusOK, NFSExport{ID: "88", ExportPath: desired.ExportPath, FSPath: desired.FSPath, Restrictions: []NFSRestriction{}, FieldsToPresentAs32Bit: []NFS32BitField{}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	got, created, err := conn.EnsureNFSExport(context.Background(), desired, false)
	if err != nil || created || got == nil || got.ID != "88" {
		t.Fatalf("EnsureNFSExport() = %+v, created=%v, err=%v", got, created, err)
	}
}

func TestDeleteNFSExportIfExistsIsIdempotent(t *testing.T) {
	t.Parallel()

	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, _ *http.Request) {
		writeProtocolJSON(t, w, http.StatusNotFound, map[string]any{"error_class": ErrClassRESTNotFound})
	})
	deleted, err := conn.DeleteNFSExportIfExists(context.Background(), "/missing")
	if err != nil || deleted {
		t.Fatalf("DeleteNFSExportIfExists() = %v, %v", deleted, err)
	}
}

func TestNFSV3PreviewTenantShapes(t *testing.T) {
	t.Parallel()

	desired := NFSExportRequest{
		ExportPath: "/tenant-export",
		TenantID:   7,
		FSPath:     "/tenant-data",
	}
	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != nfsExportsV3PreviewPath {
				t.Errorf("create request = %s %s", r.Method, r.URL.Path)
			}
			if r.URL.Query().Get("allow-fs-path-create") != "true" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["tenant_id"] != float64(7) {
				t.Errorf("tenant body = %#v", body)
			}
			writeProtocolJSON(t, w, http.StatusOK, NFSExport{ID: "70", ExportPath: desired.ExportPath, TenantID: 7, FSPath: desired.FSPath})
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != nfsExportsV3PreviewPath {
				t.Errorf("list request = %s %s", r.Method, r.URL.Path)
			}
			writeProtocolJSON(t, w, http.StatusOK, map[string]any{"entries": []NFSExport{{ID: "70", ExportPath: desired.ExportPath, TenantID: 7, FSPath: desired.FSPath}}})
		case 3:
			if r.Method != http.MethodGet || r.URL.Path != "/v3/nfs/exports/70" {
				t.Errorf("get request = %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set(headerETag, `"tenant-nfs"`)
			writeProtocolJSON(t, w, http.StatusOK, NFSExport{ID: "70", ExportPath: desired.ExportPath, TenantID: 7, FSPath: desired.FSPath})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	created, err := conn.CreateNFSExport(context.Background(), desired, true)
	if err != nil || created.TenantID != 7 {
		t.Fatalf("CreateNFSExport() = %+v, %v", created, err)
	}
	listed, err := conn.ListNFSExportsV3Preview(context.Background())
	if err != nil || len(listed) != 1 || listed[0].TenantID != 7 {
		t.Fatalf("ListNFSExportsV3Preview() = %+v, %v", listed, err)
	}
	got, err := conn.GetNFSExportV3Preview(context.Background(), "70")
	if err != nil || got.ETag != `"tenant-nfs"` {
		t.Fatalf("GetNFSExportV3Preview() = %+v, %v", got, err)
	}
}

func TestNFSExportRejectsControlCharacters(t *testing.T) {
	t.Parallel()

	conn := &Connection{}
	_, err := conn.CreateNFSExport(context.Background(), NFSExportRequest{ExportPath: "/bad\nexport", FSPath: "/data"}, false)
	if err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("error = %v", err)
	}
}
