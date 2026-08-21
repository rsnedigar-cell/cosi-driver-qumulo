package csidriver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

func snapshotTestDriver(t *testing.T) (*Driver, *fakeBackend, Config) {
	t.Helper()
	cfg := Config{
		Name: DefaultDriverName, Version: "test", Mode: "controller",
		Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000",
		BasePath: "/k8s-volumes", HandleKey: testHandleKey,
	}
	backend := &fakeBackend{attrs: &qumulo.FileAttributes{ID: "file-1", Path: "/k8s-volumes/nfs/vol"}}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	return d, backend, cfg
}

func createTestVolume(t *testing.T, d *Driver) string {
	t.Helper()
	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-snap-src",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetVolume().GetVolumeId()
}

func TestControllerAdvertisesCreateDeleteSnapshot(t *testing.T) {
	d, _, _ := snapshotTestDriver(t)
	resp, err := d.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cap := range resp.GetCapabilities() {
		if cap.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT {
			found = true
		}
	}
	if !found {
		t.Fatal("CREATE_DELETE_SNAPSHOT capability missing")
	}
}

func TestCreateSnapshotIdempotentAndSigned(t *testing.T) {
	d, backend, cfg := snapshotTestDriver(t)
	volID := createTestVolume(t, d)
	req := &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: volID}
	first, err := d.CreateSnapshot(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !first.GetSnapshot().GetReadyToUse() || first.GetSnapshot().GetSizeBytes() != 0 {
		t.Fatalf("unexpected snapshot: %#v", first.GetSnapshot())
	}
	if _, err := decodeSnapshotHandle(first.GetSnapshot().GetSnapshotId(), cfg); err != nil {
		t.Fatal(err)
	}
	second, err := d.CreateSnapshot(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.GetSnapshot().GetSnapshotId() != second.GetSnapshot().GetSnapshotId() {
		t.Fatal("idempotent retry returned a different snapshot id")
	}
	if len(backend.snapshots) != 1 {
		t.Fatalf("created %d snapshots, want 1", len(backend.snapshots))
	}
}

func TestCreateSnapshotRejectsTamperedVolume(t *testing.T) {
	d, _, _ := snapshotTestDriver(t)
	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: "qv1:not-a-handle.sig"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
}

func TestCreateSnapshotSourceGone(t *testing.T) {
	d, backend, _ := snapshotTestDriver(t)
	volID := createTestVolume(t, d)
	backend.validateErr = errVolumeNotFound
	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: volID})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
}

func TestDeleteSnapshotIdempotentAndRejectsTampering(t *testing.T) {
	d, backend, cfg := snapshotTestDriver(t)
	volID := createTestVolume(t, d)
	created, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: volID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: created.GetSnapshot().GetSnapshotId()}); err != nil {
		t.Fatal(err)
	}
	if len(backend.snapshots) != 0 {
		t.Fatal("snapshot was not deleted")
	}
	if _, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: created.GetSnapshot().GetSnapshotId()}); err != nil {
		t.Fatalf("delete of missing snapshot: %v", err)
	}
	tampered := created.GetSnapshot().GetSnapshotId()[:len(created.GetSnapshot().GetSnapshotId())-2] + "xx"
	if _, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: tampered}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("tampered snapshot code=%s err=%v", status.Code(err), err)
	}
	_ = cfg
}

func TestCreateVolumeFromSnapshotCopiesAndCleansPartialFailure(t *testing.T) {
	d, backend, cfg := snapshotTestDriver(t)
	volID := createTestVolume(t, d)
	created, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: volID})
	if err != nil {
		t.Fatal(err)
	}
	snapID := created.GetSnapshot().GetSnapshotId()
	backend.spec = ""
	backend.resource = storageResource{}
	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-from-snap",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "smb", "allowedNetworks": "10.0.0.0/8", "smbTrusteeDomain": "LOCAL", "smbTrusteeName": "k8s-smb"},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snapID},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.copiedSnapshot == "" || backend.protocol != protocolSMB {
		t.Fatalf("restore did not copy into an SMB volume: %#v", backend)
	}
	if resp.GetVolume().GetContentSource().GetSnapshot().GetSnapshotId() != snapID {
		t.Fatal("restored volume omitted snapshot content source")
	}

	backend.copyErr = errors.New("copy exploded")
	backend.spec = ""
	backend.resource = storageResource{}
	backend.treeDelete, backend.deletedRef, backend.preparedRef = "", "", ""
	_, err = d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-from-snap-fail",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8"},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snapID},
		}},
	})
	if err == nil {
		t.Fatal("expected copy failure")
	}
	if backend.preparedRef == "" || backend.treeDelete == "" || backend.deletedRef == "" {
		t.Fatalf("partial restore was not cleaned up: %#v", backend)
	}

	backend.copyErr = nil
	backend.spec = ""
	backend.resource = storageResource{}
	if _, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-from-snap-retry",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8"},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snapID},
		}},
	}); err != nil {
		t.Fatalf("retry after partial failure: %v", err)
	}

	tampered := snapID[:len(snapID)-2] + "zz"
	_, err = d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-bad-snap",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8"},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: tampered},
		}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("tampered snapshot restore code=%s err=%v", status.Code(err), err)
	}
	_ = cfg
}

func TestDeleteVolumeBlockedByDriverSnapshot(t *testing.T) {
	d, backend, _ := snapshotTestDriver(t)
	volID := createTestVolume(t, d)
	if _, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: volID}); err != nil {
		t.Fatal(err)
	}
	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volID})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "VolumeSnapshot") {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
	if backend.treeDelete != "" {
		t.Fatal("data was deleted while a snapshot existed")
	}
	snaps, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: volID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: snaps.GetSnapshot().GetSnapshotId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volID}); err != nil {
		t.Fatal(err)
	}
}
