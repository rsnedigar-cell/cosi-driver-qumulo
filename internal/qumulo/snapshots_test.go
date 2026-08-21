package qumulo_test

import (
	"context"
	"testing"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
	fakequmulo "github.com/rsnedigar-cell/cosi-driver-qumulo/test/fake_qumulo"
)

func TestSnapshotCRUDAndRestoreCopy(t *testing.T) {
	srv := fakequmulo.New()
	t.Cleanup(srv.Close)
	srv.SeedFile("/", qumulo.FileTypeDirectory, nil)
	conn, err := qumulo.NewConnection(qumulo.DialConfig{Endpoint: srv.URL, Credentials: qumulo.Credentials{Token: srv.Token}, TLS: qumulo.TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir, err := conn.EnsureDirectory(ctx, "/vol/src", "0755")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.CreateFile(ctx, "/vol/src", "hello.txt"); err != nil {
		t.Fatal(err)
	}
	srv.SeedFile("/vol/src/hello.txt", qumulo.FileTypeFile, []byte("marker\n"))

	snap, err := conn.CreateSnapshot(ctx, dir.ID, "csi-deadbeefdeadbeef")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if snap.IDString() == "" || snap.NameSuffix()[:4] != "csi-" {
		t.Fatalf("unexpected snapshot: %#v", snap)
	}
	found, err := conn.FindSnapshotBySuffix(ctx, dir.ID, snap.NameSuffix())
	if err != nil || found == nil || found.IDString() != snap.IDString() {
		t.Fatalf("find snapshot: found=%#v err=%v", found, err)
	}

	if _, err := conn.EnsureDirectory(ctx, "/vol/dst", "0755"); err != nil {
		t.Fatal(err)
	}
	if err := conn.CopySnapshotTree(ctx, "/vol/src", dir.ID, snap.IDString(), "/vol/dst"); err != nil {
		t.Fatalf("copy tree: %v", err)
	}
	if string(srv.FileBodies["/vol/dst/hello.txt"]) != "marker\n" {
		t.Fatalf("copied body=%q", srv.FileBodies["/vol/dst/hello.txt"])
	}

	if err := conn.DeleteSnapshot(ctx, snap.IDString()); err != nil {
		t.Fatal(err)
	}
	if err := conn.DeleteSnapshot(ctx, snap.IDString()); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestSnapshotCopyFailureInjected(t *testing.T) {
	srv := fakequmulo.New()
	t.Cleanup(srv.Close)
	srv.SeedFile("/", qumulo.FileTypeDirectory, nil)
	srv.FailCopy = true
	conn, err := qumulo.NewConnection(qumulo.DialConfig{Endpoint: srv.URL, Credentials: qumulo.Credentials{Token: srv.Token}, TLS: qumulo.TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir, err := conn.EnsureDirectory(ctx, "/vol/src", "0755")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.CreateFile(ctx, "/vol/src", "hello.txt"); err != nil {
		t.Fatal(err)
	}
	srv.SeedFile("/vol/src/hello.txt", qumulo.FileTypeFile, []byte("x"))
	snap, err := conn.CreateSnapshot(ctx, dir.ID, "csi-abcdabcdabcdabcd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.EnsureDirectory(ctx, "/vol/dst", "0755"); err != nil {
		t.Fatal(err)
	}
	if err := conn.CopySnapshotTree(ctx, "/vol/src", dir.ID, snap.IDString(), "/vol/dst"); err == nil {
		t.Fatal("expected injected copy failure")
	}
}
