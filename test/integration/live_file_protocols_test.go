//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"regexp"
	"testing"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

// TestLiveFileProtocolControlPlane exercises real Qumulo directory, quota,
// NFS-export, and SMB-share APIs. It is destructive only below its validated,
// unique /csi-live-* path and is disabled unless explicitly enabled.
func TestLiveFileProtocolControlPlane(t *testing.T) {
	if os.Getenv("QUMULO_LIVE_FILE_PROTOCOLS") != "true" {
		t.Skip("set QUMULO_LIVE_FILE_PROTOCOLS=true to enable destructive NFS/SMB control-plane testing")
	}
	host := os.Getenv("QUMULO_HOST")
	if host == "" {
		t.Skip("QUMULO_HOST not set")
	}
	allowed := os.Getenv("QUMULO_LIVE_ALLOWED_NETWORK")
	if allowed == "" {
		t.Fatal("QUMULO_LIVE_ALLOWED_NETWORK is required so live shares are never open to every host")
	}
	suffix := safeLiveSuffix(t)
	fsPath := "/csi-live-" + suffix
	nfsPath := "/csi-live-" + suffix
	shareName := "csi-live-" + suffix

	conn, err := qumulo.NewConnection(qumulo.DialConfig{
		Endpoint: host,
		Credentials: qumulo.Credentials{
			Token:    os.Getenv("QUMULO_TOKEN"),
			Username: os.Getenv("QUMULO_USERNAME"),
			Password: os.Getenv("QUMULO_PASSWORD"),
		},
		TLS: qumulo.TLSConfig{InsecureSkipVerify: os.Getenv("QUMULO_INSECURE_SKIP_TLS_VERIFY") == "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	attrs, err := conn.EnsureDirectory(ctx, fsPath, "0777")
	if err != nil {
		t.Fatalf("ensure live volume directory: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.TreeDelete(context.Background(), attrs.ID); err != nil {
			t.Logf("cleanup tree-delete %s: %v", fsPath, err)
		}
	})
	if err := conn.CreateQuota(ctx, attrs.ID, 1<<30); err != nil {
		t.Fatalf("ensure live quota: %v", err)
	}

	nfsDesired := qumulo.NFSExportRequest{
		ExportPath:  nfsPath,
		FSPath:      fsPath,
		Description: "Qumulo CSI live integration test",
		Restrictions: []qumulo.NFSRestriction{{
			HostRestrictions:           []string{allowed},
			RequiredAuthenticationMode: qumulo.NFSAuthNone,
			UserMapping:                qumulo.NFSMapNone,
		}},
	}
	nfsExport, _, err := conn.EnsureNFSExport(ctx, nfsDesired, false)
	if err != nil {
		t.Fatalf("ensure live NFS export: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.DeleteNFSExportIfExists(context.Background(), nfsExport.IDOrPath()); err != nil {
			t.Logf("cleanup NFS export: %v", err)
		}
	})
	if again, created, err := conn.EnsureNFSExport(ctx, nfsDesired, false); err != nil || created || again.FSPath != fsPath {
		t.Fatalf("NFS reconcile: created=%v export=%#v err=%v", created, again, err)
	}

	rights := []qumulo.SMBRight{qumulo.SMBRightRead, qumulo.SMBRightWrite, qumulo.SMBRightChangePermissions}
	smbDesired := qumulo.SMBShareRequest{
		ShareName:   shareName,
		FSPath:      fsPath,
		Description: "Qumulo CSI live integration test",
		Permissions: []qumulo.SMBSharePermission{{
			Type:    qumulo.SMBPermissionAllowed,
			Trustee: qumulo.Identity{Domain: "WORLD", Name: "Everyone"},
			Rights:  rights,
		}},
		NetworkPermissions: []qumulo.SMBNetworkPermission{{
			Type:          qumulo.SMBPermissionAllowed,
			AddressRanges: []string{allowed},
			Rights:        rights,
		}},
		AccessBasedEnumerationEnabled: true,
		DefaultFileCreateMode:         "0666",
		DefaultDirectoryCreateMode:    "0777",
		BytesPerSector:                "512",
		RequireEncryption:             true,
	}
	smbShare, _, err := conn.EnsureSMBShare(ctx, smbDesired, false)
	if err != nil {
		t.Fatalf("ensure live SMB share: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.DeleteSMBShareIfExists(context.Background(), smbShare.IDOrName()); err != nil {
			t.Logf("cleanup SMB share: %v", err)
		}
	})
	if again, created, err := conn.EnsureSMBShare(ctx, smbDesired, false); err != nil || created || again.FSPath != fsPath {
		t.Fatalf("SMB reconcile: created=%v share=%#v err=%v", created, again, err)
	}
}

func safeLiveSuffix(t *testing.T) string {
	t.Helper()
	suffix := os.Getenv("QUMULO_LIVE_SUFFIX")
	if suffix == "" {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			t.Fatal(err)
		}
		suffix = hex.EncodeToString(random[:])
	}
	if !regexp.MustCompile(`^[a-z0-9]{4,24}$`).MatchString(suffix) {
		t.Fatalf("QUMULO_LIVE_SUFFIX %q must match [a-z0-9]{4,24}", suffix)
	}
	return suffix
}
