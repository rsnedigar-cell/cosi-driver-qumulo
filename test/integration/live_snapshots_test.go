//go:build integration

package integration

import (
	"context"
	"os"
	"testing"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

// TestLiveSnapshots is a destructive control-plane test of directory
// snapshots and restore-copy. It is skipped unless explicitly enabled.
func TestLiveSnapshots(t *testing.T) {
	if os.Getenv("QUMULO_LIVE_SNAPSHOTS") != "true" {
		t.Skip("set QUMULO_LIVE_SNAPSHOTS=true to enable destructive snapshot testing")
	}
	host := os.Getenv("QUMULO_HOST")
	if host == "" {
		t.Skip("QUMULO_HOST not set")
	}
	suffix := safeLiveSuffix(t)
	srcPath := "/csi-live-snap-src-" + suffix
	dstPath := "/csi-live-snap-dst-" + suffix
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
	src, err := conn.EnsureDirectory(ctx, srcPath, "0777")
	if err != nil {
		t.Fatalf("source directory: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.TreeDelete(context.Background(), src.ID); err != nil {
			t.Logf("cleanup source: %v", err)
		}
	})
	if _, err := conn.CreateFile(ctx, srcPath, "marker.txt"); err != nil {
		t.Fatalf("create marker: %v", err)
	}
	nameSuffix := "csi-" + suffix
	if len(nameSuffix) > 20 {
		nameSuffix = nameSuffix[:20]
	}
	snap, err := conn.CreateSnapshot(ctx, src.ID, nameSuffix)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.DeleteSnapshot(context.Background(), snap.IDString()); err != nil {
			t.Logf("cleanup snapshot: %v", err)
		}
	})
	if got := snap.NameSuffix(); got != nameSuffix {
		t.Fatalf("name suffix %q want %q (visible name %q)", got, nameSuffix, snap.Name)
	}
	dst, err := conn.EnsureDirectory(ctx, dstPath, "0777")
	if err != nil {
		t.Fatalf("dest directory: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.TreeDelete(context.Background(), dst.ID); err != nil {
			t.Logf("cleanup dest: %v", err)
		}
	})
	if err := conn.CopySnapshotTree(ctx, srcPath, src.ID, snap.IDString(), dstPath); err != nil {
		t.Fatalf("restore copy: %v", err)
	}
	entries, err := conn.ListDirectoryEntries(ctx, dstPath, "")
	if err != nil {
		t.Fatalf("list restored: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "marker.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored directory missing marker.txt: %#v", entries)
	}
	has, err := conn.HasDriverSnapshots(ctx, src.ID)
	if err != nil || !has {
		t.Fatalf("HasDriverSnapshots=%v err=%v", has, err)
	}
	if err := conn.DeleteSnapshot(ctx, snap.IDString()); err != nil {
		t.Fatal(err)
	}
	has, err = conn.HasDriverSnapshots(ctx, src.ID)
	if err != nil || has {
		t.Fatalf("after delete HasDriverSnapshots=%v err=%v", has, err)
	}
}
