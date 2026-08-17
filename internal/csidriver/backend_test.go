package csidriver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

func TestCheckVolumeRejectsRenamedManagedSMBShare(t *testing.T) {
	opts := volumeOptions{
		RequestName: "claim", Protocol: protocolSMB, Endpoint: "q.example", Server: "q.example", RESTPort: "8000",
		FSPath: "/volumes/smb/claim", ResourceName: "claim", DirectoryMode: "0777",
		AllowedNetworks: []string{"10.0.0.0/8"}, QuotaEnabled: true, DeleteData: true,
		SMBRequireEncryption: true, SMBAccessBasedEnum: true, SMBTrustee: qumulo.Identity{Domain: "LOCAL", Name: "k8s-smb"},
		RequestedCapacity: 1 << 30,
	}
	description, err := managedDescription(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/nfs/exports/":
			_ = json.NewEncoder(w).Encode([]qumulo.NFSExport{})
		case "/v2/smb/shares/":
			_ = json.NewEncoder(w).Encode([]qumulo.SMBShare{{
				ID: "share-1", ShareName: "renamed-by-operator", FSPath: opts.FSPath, Description: description,
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	conn, err := qumulo.NewConnection(qumulo.DialConfig{Endpoint: srv.URL, Credentials: qumulo.Credentials{Token: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	err = (&qumuloBackend{conn: conn}).CheckVolume(context.Background(), opts)
	if !errors.Is(err, errVolumeConflict) {
		t.Fatalf("renamed managed share was not rejected: %v", err)
	}
}

func TestPrepareVolumeDeletionConditionallyTombstonesResource(t *testing.T) {
	tests := []struct {
		name string
		h    volumeHandle
		path string
	}{
		{
			name: "nfs",
			h: volumeHandle{
				Protocol: protocolNFS, ResourceID: "export-1", ResourceName: "/k8s/claim",
				FSPath: "/volumes/nfs/claim", DirectoryID: "file-1", SpecFingerprint: testSpecFingerprint,
			},
			path: "/v2/nfs/exports/export-1",
		},
		{
			name: "smb",
			h: volumeHandle{
				Protocol: protocolSMB, ResourceID: "share-1", ResourceName: "claim",
				FSPath: "/volumes/smb/claim", DirectoryID: "file-1", SpecFingerprint: testSpecFingerprint,
			},
			path: "/v2/smb/shares/share-1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			description := managedDescriptionForHandle(test.h)
			etag := `"v1"`
			patches := 0
			deletes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					w.Header().Set("ETag", etag)
				case http.MethodPatch:
					if r.Header.Get("If-Match") != etag {
						t.Errorf("If-Match=%q, want %q", r.Header.Get("If-Match"), etag)
					}
					var patch struct {
						Description *string `json:"description"`
					}
					if err := json.NewDecoder(r.Body).Decode(&patch); err != nil || patch.Description == nil {
						t.Errorf("invalid deletion patch: %#v err=%v", patch, err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					description = *patch.Description
					patches++
					etag = `"v2"`
					w.Header().Set("ETag", etag)
				case http.MethodDelete:
					if r.Header.Get("If-Match") != etag {
						t.Errorf("delete If-Match=%q, want %q", r.Header.Get("If-Match"), etag)
					}
					if description != volumeDeletionDescription(test.h) {
						t.Errorf("resource was deleted without the exact per-handle tombstone: %q", description)
					}
					deletes++
					w.WriteHeader(http.StatusNoContent)
					return
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if test.h.Protocol == protocolNFS {
					_ = json.NewEncoder(w).Encode(qumulo.NFSExport{
						ID: test.h.ResourceID, ExportPath: test.h.ResourceName, FSPath: test.h.FSPath, Description: description,
					})
				} else {
					_ = json.NewEncoder(w).Encode(qumulo.SMBShare{
						ID: test.h.ResourceID, ShareName: test.h.ResourceName, FSPath: test.h.FSPath, Description: description,
					})
				}
			}))
			t.Cleanup(srv.Close)
			conn, err := qumulo.NewConnection(qumulo.DialConfig{Endpoint: srv.URL, Credentials: qumulo.Credentials{Token: "test"}})
			if err != nil {
				t.Fatal(err)
			}
			backend := &qumuloBackend{conn: conn}
			if err := backend.PrepareVolumeDeletion(context.Background(), test.h); err != nil {
				t.Fatal(err)
			}
			if description != volumeDeletionDescription(test.h) || patches != 1 {
				t.Fatalf("description=%q patches=%d", description, patches)
			}
			if err := backend.PrepareVolumeDeletion(context.Background(), test.h); err != nil {
				t.Fatal(err)
			}
			if patches != 1 {
				t.Fatalf("idempotent retry patched resource %d times", patches)
			}
			if err := backend.DeleteVolumeResource(context.Background(), test.h, true); err != nil {
				t.Fatal(err)
			}
			if deletes != 1 {
				t.Fatalf("tombstoned resource received %d delete requests", deletes)
			}
		})
	}
}

func TestVolumeOperationsRefuseRepurposedProtocolResource(t *testing.T) {
	tests := []struct {
		name string
		h    volumeHandle
		path string
	}{
		{
			name: "nfs",
			h: volumeHandle{
				Protocol: protocolNFS, ResourceID: "export-1", ResourceName: "/k8s/claim",
				FSPath: "/volumes/nfs/claim", DirectoryID: "file-1", SpecFingerprint: testSpecFingerprint,
			},
			path: "/v2/nfs/exports/export-1",
		},
		{
			name: "smb",
			h: volumeHandle{
				Protocol: protocolSMB, ResourceID: "share-1", ResourceName: "claim",
				FSPath: "/volumes/smb/claim", DirectoryID: "file-1", SpecFingerprint: testSpecFingerprint,
			},
			path: "/v2/smb/shares/share-1",
		},
	}
	for _, test := range tests {
		for _, operation := range []string{"validate", "prepare", "delete"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				mutations := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method != http.MethodGet {
						mutations++
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					if strings.HasPrefix(r.URL.Path, "/v1/files/") && strings.HasSuffix(r.URL.Path, "/info/attributes") {
						_ = json.NewEncoder(w).Encode(qumulo.FileAttributes{ID: test.h.DirectoryID, Path: test.h.FSPath})
						return
					}
					if r.URL.Path != test.path {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("ETag", `"operator-v2"`)
					if test.h.Protocol == protocolNFS {
						_ = json.NewEncoder(w).Encode(qumulo.NFSExport{
							ID: test.h.ResourceID, ExportPath: test.h.ResourceName, FSPath: test.h.FSPath,
							Description: "repurposed by an operator",
						})
					} else {
						_ = json.NewEncoder(w).Encode(qumulo.SMBShare{
							ID: test.h.ResourceID, ShareName: test.h.ResourceName, FSPath: test.h.FSPath,
							Description: "repurposed by an operator",
						})
					}
				}))
				t.Cleanup(srv.Close)
				conn, err := qumulo.NewConnection(qumulo.DialConfig{Endpoint: srv.URL, Credentials: qumulo.Credentials{Token: "test"}})
				if err != nil {
					t.Fatal(err)
				}
				backend := &qumuloBackend{conn: conn}
				switch operation {
				case "validate":
					err = backend.ValidateVolume(context.Background(), test.h)
					if !errors.Is(err, errVolumeNotFound) {
						t.Fatalf("ValidateVolume error=%v, want volume not found", err)
					}
				case "prepare":
					err = backend.PrepareVolumeDeletion(context.Background(), test.h)
					if !errors.Is(err, errVolumeIdentityChanged) {
						t.Fatalf("PrepareVolumeDeletion error=%v, want identity changed", err)
					}
				case "delete":
					err = backend.DeleteVolumeResource(context.Background(), test.h, false)
					if !errors.Is(err, errVolumeIdentityChanged) {
						t.Fatalf("DeleteVolumeResource error=%v, want identity changed", err)
					}
				}
				if mutations != 0 {
					t.Fatalf("repurposed resource received %d mutation requests", mutations)
				}
			})
		}
	}
}

func TestCheckVolumeRejectsDeletionClaim(t *testing.T) {
	opts := volumeOptions{
		RequestName: "claim", Protocol: protocolNFS, Endpoint: "q.example", Server: "q.example", RESTPort: "8000",
		FSPath: "/volumes/nfs/claim", ResourceName: "claim", NFSExportPath: "/k8s/claim", DirectoryMode: "0777",
		AllowedNetworks: []string{"10.0.0.0/8"}, QuotaEnabled: true, DeleteData: true, NFSVersion: "4.1",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/nfs/exports/":
			_ = json.NewEncoder(w).Encode([]qumulo.NFSExport{{
				ID: "export-1", ExportPath: opts.NFSExportPath, FSPath: opts.FSPath,
				Description: volumeDeletionDescription(volumeHandle{Protocol: protocolNFS, ResourceID: "export-1", DirectoryID: "file-1", FSPath: opts.FSPath, SpecFingerprint: testSpecFingerprint}),
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	conn, err := qumulo.NewConnection(qumulo.DialConfig{Endpoint: srv.URL, Credentials: qumulo.Credentials{Token: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	err = (&qumuloBackend{conn: conn}).CheckVolume(context.Background(), opts)
	if !errors.Is(err, errVolumeConflict) {
		t.Fatalf("deletion claim did not block CreateVolume: %v", err)
	}
}
