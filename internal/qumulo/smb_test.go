package qumulo

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSMBShareCRUD(t *testing.T) {
	t.Parallel()

	desired := SMBShareRequest{
		ShareName:   "team share",
		FSPath:      "/data/team-share",
		Description: "managed by COSI",
		Permissions: []SMBSharePermission{{
			Type:    SMBPermissionAllowed,
			Trustee: Identity{Domain: "WORLD", Name: "Everyone"},
			Rights:  []SMBRight{SMBRightRead, SMBRightWrite},
		}},
		NetworkPermissions: []SMBNetworkPermission{{
			Type:          SMBPermissionAllowed,
			AddressRanges: []string{"10.0.0.0/24"},
			Rights:        []SMBRight{SMBRightRead, SMBRightWrite},
		}},
		AccessBasedEnumerationEnabled: true,
		DefaultFileCreateMode:         "0640",
		DefaultDirectoryCreateMode:    "0750",
		BytesPerSector:                "512",
		RequireEncryption:             true,
	}

	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch call {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != smbSharesPath {
				t.Errorf("create request = %s %s", r.Method, r.URL.Path)
			}
			if got := r.URL.Query().Get("allow-fs-path-create"); got != "true" {
				t.Errorf("allow-fs-path-create = %q", got)
			}
			var got SMBShareRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if got.ShareName != desired.ShareName || len(got.Permissions) != 1 || got.Permissions[0].Trustee.Domain != "WORLD" || !got.RequireEncryption {
				t.Errorf("unexpected create body: %+v", got)
			}
			w.Header().Set(headerETag, `"smb-1"`)
			writeProtocolJSON(t, w, http.StatusOK, SMBShare{ID: "31", ShareName: desired.ShareName, FSPath: desired.FSPath})
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != smbSharesPath {
				t.Errorf("list request = %s %s", r.Method, r.URL.Path)
			}
			if got := r.URL.Query().Get("populate-trustee-names"); got != "true" {
				t.Errorf("populate-trustee-names = %q", got)
			}
			writeProtocolJSON(t, w, http.StatusOK, []SMBShare{{ID: "31", ShareName: desired.ShareName, FSPath: desired.FSPath}})
		case 3:
			if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v2/smb/shares/team%20share" {
				t.Errorf("get request = %s %s (%s)", r.Method, r.URL.Path, r.URL.EscapedPath())
			}
			w.Header().Set(headerETag, `"smb-2"`)
			writeProtocolJSON(t, w, http.StatusOK, SMBShare{ID: "31", ShareName: desired.ShareName, FSPath: desired.FSPath})
		case 4:
			if r.Method != http.MethodPut || r.URL.Path != "/v2/smb/shares/31" {
				t.Errorf("replace request = %s %s", r.Method, r.URL.Path)
			}
			if got := r.Header.Get(headerIfMatch); got != `"smb-2"` {
				t.Errorf("If-Match = %q", got)
			}
			writeProtocolJSON(t, w, http.StatusOK, SMBShare{ID: "31", ShareName: desired.ShareName, FSPath: desired.FSPath})
		case 5:
			if r.Method != http.MethodPatch || r.URL.Path != "/v2/smb/shares/31" {
				t.Errorf("patch request = %s %s", r.Method, r.URL.Path)
			}
			var got map[string]any
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got["description"] != "updated" || len(got) != 1 {
				t.Errorf("unexpected patch body: %#v", got)
			}
			writeProtocolJSON(t, w, http.StatusOK, SMBShare{ID: "31", ShareName: desired.ShareName, FSPath: desired.FSPath, Description: "updated"})
		case 6:
			if r.Method != http.MethodDelete || r.URL.Path != "/v2/smb/shares/31" {
				t.Errorf("delete request = %s %s", r.Method, r.URL.Path)
			}
			if got := r.Header.Get(headerIfMatch); got != `"smb-3"` {
				t.Errorf("delete If-Match = %q", got)
			}
			writeProtocolJSON(t, w, http.StatusOK, nil)
		default:
			t.Errorf("unexpected request %d: %s %s", call, r.Method, r.URL.Path)
			writeProtocolJSON(t, w, http.StatusInternalServerError, nil)
		}
	})

	ctx := context.Background()
	created, err := conn.CreateSMBShare(ctx, desired, true)
	if err != nil || created.ID != "31" || created.ETag != `"smb-1"` {
		t.Fatalf("CreateSMBShare() = %+v, %v", created, err)
	}
	listed, err := conn.ListSMBShares(ctx, true)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListSMBShares() = %+v, %v", listed, err)
	}
	got, err := conn.GetSMBShare(ctx, desired.ShareName)
	if err != nil || got.ETag != `"smb-2"` {
		t.Fatalf("GetSMBShare() = %+v, %v", got, err)
	}
	if _, err := conn.ReplaceSMBShare(ctx, "31", desired, SMBShareWriteOptions{IfMatch: got.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PatchSMBShare(ctx, "31", PatchSMBShareRequest{Description: ptr("updated")}, SMBShareWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := conn.DeleteSMBShare(ctx, "31", `"smb-3"`); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 6 {
		t.Fatalf("calls = %d, want 6", calls.Load())
	}
}

func TestEnsureSMBShareReconcilesCreateConflict(t *testing.T) {
	t.Parallel()

	desired := SMBShareRequest{
		ShareName:         "claim",
		FSPath:            "/data/claim",
		Description:       "desired",
		Permissions:       []SMBSharePermission{{Type: SMBPermissionAllowed, Trustee: Identity{Domain: "WORLD"}, Rights: []SMBRight{SMBRightRead}}},
		RequireEncryption: true,
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
			writeProtocolJSON(t, w, http.StatusOK, SMBShare{
				ID: "41", ShareName: desired.ShareName, FSPath: desired.FSPath,
				Description: "old", Permissions: desired.Permissions,
			})
		case http.MethodPost:
			posts.Add(1)
			writeProtocolJSON(t, w, http.StatusConflict, map[string]any{"description": "already exists"})
		case http.MethodPatch:
			patches.Add(1)
			if r.URL.Path != "/v2/smb/shares/41" || r.Header.Get(headerIfMatch) != `"before"` {
				t.Errorf("conditional patch = %s If-Match %q", r.URL.Path, r.Header.Get(headerIfMatch))
			}
			var patch PatchSMBShareRequest
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if patch.Description == nil || *patch.Description != desired.Description || patch.RequireEncryption == nil || !*patch.RequireEncryption {
				t.Errorf("patch = %+v", patch)
			}
			w.Header().Set(headerETag, `"after"`)
			writeProtocolJSON(t, w, http.StatusOK, SMBShare{ID: "41", ShareName: desired.ShareName, FSPath: desired.FSPath, Description: desired.Description, RequireEncryption: true})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	got, created, err := conn.EnsureSMBShare(context.Background(), desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if created || got.Description != "desired" || !got.RequireEncryption || got.ETag != `"after"` {
		t.Fatalf("EnsureSMBShare() = %+v, created=%v", got, created)
	}
	if gets.Load() != 2 || posts.Load() != 1 || patches.Load() != 1 {
		t.Fatalf("gets=%d posts=%d patches=%d", gets.Load(), posts.Load(), patches.Load())
	}
}

func TestEnsureSMBShareRefusesRetarget(t *testing.T) {
	t.Parallel()

	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		writeProtocolJSON(t, w, http.StatusOK, SMBShare{ID: "1", ShareName: "share", FSPath: "/someone-elses-data"})
	})
	_, _, err := conn.EnsureSMBShare(context.Background(), SMBShareRequest{ShareName: "share", FSPath: "/our-data"}, false)
	if err == nil || !strings.Contains(err.Error(), "refusing to retarget") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnsureSMBShareReconcilesAmbiguousCreate(t *testing.T) {
	t.Parallel()

	desired := SMBShareRequest{ShareName: "ambiguous", FSPath: "/data/ambiguous"}
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
			writeProtocolJSON(t, w, http.StatusOK, SMBShare{ID: "89", ShareName: desired.ShareName, FSPath: desired.FSPath, Permissions: []SMBSharePermission{}, NetworkPermissions: []SMBNetworkPermission{}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	got, created, err := conn.EnsureSMBShare(context.Background(), desired, false)
	if err != nil || created || got == nil || got.ID != "89" {
		t.Fatalf("EnsureSMBShare() = %+v, created=%v, err=%v", got, created, err)
	}
}

func TestSMBShareValidation(t *testing.T) {
	t.Parallel()

	conn := &Connection{}
	invalid := []SMBShareRequest{
		{ShareName: "bad\nname", FSPath: "/data"},
		{ShareName: "share", FSPath: "relative"},
		{ShareName: "share", FSPath: "/data", DefaultFileCreateMode: "0688"},
		{ShareName: "share", FSPath: "/data", BytesPerSector: "4096"},
	}
	for _, req := range invalid {
		if _, err := conn.CreateSMBShare(context.Background(), req, false); err == nil {
			t.Errorf("CreateSMBShare(%+v) unexpectedly succeeded", req)
		}
	}
}

func TestDeleteSMBShareIfExistsIsIdempotent(t *testing.T) {
	t.Parallel()

	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, _ *http.Request) {
		writeProtocolJSON(t, w, http.StatusNotFound, map[string]any{"error_class": ErrClassRESTNotFound})
	})
	deleted, err := conn.DeleteSMBShareIfExists(context.Background(), "missing")
	if err != nil || deleted {
		t.Fatalf("DeleteSMBShareIfExists() = %v, %v", deleted, err)
	}
}

func TestSMBV3PreviewTenantShapes(t *testing.T) {
	t.Parallel()

	desired := SMBShareRequest{
		ShareName:               "tenant-share",
		TenantID:                9,
		FSPath:                  "/tenant-data",
		BytesPerSector:          "512",
		ExpandFSPathVariables:   true,
		OfflineFilesCachingMode: SMBOfflineFilesManualCaching,
	}
	var calls atomic.Int32
	conn := newProtocolTestConnection(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != smbSharesV3PreviewPath {
				t.Errorf("create request = %s %s", r.Method, r.URL.Path)
			}
			if r.URL.RawQuery != "" {
				t.Errorf("v3 create query = %q", r.URL.RawQuery)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["tenant_id"] != float64(9) || body["allow_fs_path_create"] != true || body["expand_fs_path_variables"] != true {
				t.Errorf("v3 create body = %#v", body)
			}
			if _, exists := body["bytes_per_sector"]; exists {
				t.Errorf("v3 request included v2-only bytes_per_sector: %#v", body)
			}
			writeProtocolJSON(t, w, http.StatusOK, SMBShare{ID: "90", ShareName: desired.ShareName, TenantID: 9, FSPath: desired.FSPath})
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != smbSharesV3PreviewPath {
				t.Errorf("list request = %s %s", r.Method, r.URL.Path)
			}
			if r.URL.Query().Get("populate-trustee-names") != "true" {
				t.Errorf("list query = %q", r.URL.RawQuery)
			}
			writeProtocolJSON(t, w, http.StatusOK, map[string]any{"entries": []SMBShare{{ID: "90", ShareName: desired.ShareName, TenantID: 9, FSPath: desired.FSPath}}})
		case 3:
			if r.Method != http.MethodGet || r.URL.Path != "/v3/smb/shares/90" {
				t.Errorf("get request = %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set(headerETag, `"tenant-smb"`)
			writeProtocolJSON(t, w, http.StatusOK, SMBShare{ID: "90", ShareName: desired.ShareName, TenantID: 9, FSPath: desired.FSPath})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	created, err := conn.CreateSMBShare(context.Background(), desired, true)
	if err != nil || created.TenantID != 9 {
		t.Fatalf("CreateSMBShare() = %+v, %v", created, err)
	}
	listed, err := conn.ListSMBSharesV3Preview(context.Background(), true)
	if err != nil || len(listed) != 1 || listed[0].TenantID != 9 {
		t.Fatalf("ListSMBSharesV3Preview() = %+v, %v", listed, err)
	}
	got, err := conn.GetSMBShareV3Preview(context.Background(), "90")
	if err != nil || got.ETag != `"tenant-smb"` {
		t.Fatalf("GetSMBShareV3Preview() = %+v, %v", got, err)
	}
}
