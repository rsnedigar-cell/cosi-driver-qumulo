package csidriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

type fakeConnector struct{ backend *fakeBackend }

func (f fakeConnector) Connect(context.Context, string, string, map[string]string) (storageBackend, error) {
	return f.backend, nil
}

type storageBackendConnector struct{ backend storageBackend }

func (f storageBackendConnector) Connect(context.Context, string, string, map[string]string) (storageBackend, error) {
	return f.backend, nil
}

type fakeBackend struct {
	attrs          *qumulo.FileAttributes
	resource       storageResource
	protocol       protocol
	checkErr       error
	validateErr    error
	ensureErr      error
	prepareErr     error
	deleteErr      error
	quota          int64
	deletedRef     string
	preparedRef    string
	treeDelete     string
	directory      string
	directoryModes []string
	spec           string
}

func (f *fakeBackend) EnsureVersion(context.Context, string) (string, error) { return "7.9.2", nil }
func (f *fakeBackend) CheckVolume(_ context.Context, opts volumeOptions) error {
	if f.checkErr != nil {
		return f.checkErr
	}
	if f.spec == "" {
		return nil
	}
	fingerprint, err := volumeSpecFingerprint(opts)
	if err != nil {
		return err
	}
	if fingerprint != f.spec {
		return fmt.Errorf("%w: immutable specification differs", errVolumeConflict)
	}
	if opts.QuotaEnabled && f.quota > 0 {
		if f.quota < opts.RequestedCapacity || opts.CapacityLimit > 0 && f.quota > opts.CapacityLimit {
			return fmt.Errorf("%w: existing capacity is outside the requested range", errVolumeConflict)
		}
	}
	return nil
}
func (f *fakeBackend) EnsureDirectory(_ context.Context, path, mode string) (*qumulo.FileAttributes, error) {
	f.directory = path
	f.directoryModes = append(f.directoryModes, mode)
	if f.attrs == nil {
		f.attrs = &qumulo.FileAttributes{ID: "file-1", Path: path, Mode: "0777"}
	}
	return f.attrs, nil
}
func (f *fakeBackend) EnsureQuota(_ context.Context, _ string, limit int64) (int64, error) {
	if f.quota > limit {
		return f.quota, nil
	}
	f.quota = limit
	return limit, nil
}
func (f *fakeBackend) EnsureNFS(_ context.Context, opts volumeOptions) (storageResource, error) {
	if f.ensureErr != nil {
		return storageResource{}, f.ensureErr
	}
	f.protocol = protocolNFS
	f.spec, _ = volumeSpecFingerprint(opts)
	if f.resource.Name == "" {
		f.resource = storageResource{ID: "nfs-1", Name: "/k8s/claim", Path: f.directory}
	}
	return f.resource, nil
}
func (f *fakeBackend) EnsureSMB(_ context.Context, opts volumeOptions) (storageResource, error) {
	if f.ensureErr != nil {
		return storageResource{}, f.ensureErr
	}
	f.protocol = protocolSMB
	f.spec, _ = volumeSpecFingerprint(opts)
	if f.resource.Name == "" {
		f.resource = storageResource{ID: "smb-1", Name: "claim", Path: f.directory}
	}
	return f.resource, nil
}
func (f *fakeBackend) ValidateVolume(context.Context, volumeHandle) error { return f.validateErr }
func (f *fakeBackend) PrepareVolumeDeletion(_ context.Context, h volumeHandle) error {
	if f.prepareErr != nil {
		return f.prepareErr
	}
	f.preparedRef = h.ResourceID
	return nil
}
func (f *fakeBackend) DeleteVolumeResource(_ context.Context, h volumeHandle, _ bool) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.protocol, f.deletedRef = h.Protocol, h.ResourceID
	return nil
}
func (f *fakeBackend) FileAttributes(context.Context, string) (*qumulo.FileAttributes, error) {
	return f.attrs, nil
}
func (f *fakeBackend) TreeDelete(_ context.Context, id string) error {
	f.treeDelete = id
	return nil
}

type deletionRaceBackend struct {
	*fakeBackend
	mu             sync.Mutex
	deleting       bool
	actions        []string
	prepareStarted chan struct{}
	releaseTree    chan struct{}
	prepareOnce    sync.Once
}

func (b *deletionRaceBackend) CheckVolume(ctx context.Context, opts volumeOptions) error {
	b.mu.Lock()
	deleting := b.deleting
	b.mu.Unlock()
	if deleting {
		return fmt.Errorf("%w: volume deletion is in progress", errVolumeConflict)
	}
	return b.fakeBackend.CheckVolume(ctx, opts)
}

func (b *deletionRaceBackend) PrepareVolumeDeletion(_ context.Context, h volumeHandle) error {
	b.mu.Lock()
	b.deleting = true
	b.actions = append(b.actions, "prepare:"+h.ResourceID)
	b.mu.Unlock()
	b.prepareOnce.Do(func() { close(b.prepareStarted) })
	return nil
}

func (b *deletionRaceBackend) TreeDelete(ctx context.Context, id string) error {
	b.mu.Lock()
	b.actions = append(b.actions, "tree:"+id)
	b.mu.Unlock()
	select {
	case <-b.releaseTree:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *deletionRaceBackend) DeleteVolumeResource(_ context.Context, h volumeHandle, requireDeletionClaim bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.actions = append(b.actions, fmt.Sprintf("delete:%s:%t", h.ResourceID, requireDeletionClaim))
	b.deleting = false
	b.deletedRef = h.ResourceID
	return nil
}

func (b *deletionRaceBackend) recordedActions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.actions...)
}

func TestCreateAndDeleteNFSVolume(t *testing.T) {
	cfg := Config{
		Name: DefaultDriverName, Version: "test", Mode: "controller",
		Endpoint: "q.example", DataServer: "q-data.example", RESTPort: "8000",
		BasePath: "/k8s-volumes", VersionFloor: "7.2.0", HandleKey: testHandleKey,
	}
	backend := &fakeBackend{}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-123",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 2 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.protocol != protocolNFS || backend.quota != 2<<30 {
		t.Fatalf("backend state: %#v", backend)
	}
	h, err := decodeVolumeHandle(resp.GetVolume().GetVolumeId(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if h.ResourceID != "nfs-1" || h.ResourceName != "/k8s/claim" || h.NFSVersion != "4.1" {
		t.Fatalf("unexpected handle: %#v", h)
	}
	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: resp.GetVolume().GetVolumeId()}); err != nil {
		t.Fatal(err)
	}
	if backend.preparedRef != "nfs-1" || backend.treeDelete != "file-1" || backend.deletedRef != "nfs-1" {
		t.Fatalf("delete did not claim deletion, remove data, then remove the export: %#v", backend)
	}
}

func TestDeleteTombstoneBlocksConcurrentCreateAcrossDriverReplicas(t *testing.T) {
	cfg := Config{
		Name: DefaultDriverName, Version: "test", Mode: "controller",
		Endpoint: "q.example", DataServer: "q-data.example", RESTPort: "8000",
		BasePath: "/k8s-volumes", VersionFloor: "7.2.0", HandleKey: testHandleKey,
	}
	createRequest := &csi.CreateVolumeRequest{
		Name:               "pvc-delete-race",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8"},
	}
	opts, err := parseVolumeOptions(cfg, createRequest.GetName(), createRequest.GetParameters())
	if err != nil {
		t.Fatal(err)
	}
	backend := &deletionRaceBackend{
		fakeBackend:    &fakeBackend{attrs: &qumulo.FileAttributes{ID: "file-old", Path: opts.FSPath}},
		prepareStarted: make(chan struct{}),
		releaseTree:    make(chan struct{}),
	}
	h := volumeHandle{
		Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer,
		FSPath: opts.FSPath, DirectoryID: "file-old", ResourceID: "export-old", ResourceName: opts.NFSExportPath,
		SpecFingerprint: testSpecFingerprint, QuotaEnabled: true, DeleteData: true, NFSVersion: opts.NFSVersion,
	}
	volumeID, err := h.encode(cfg.HandleKey)
	if err != nil {
		t.Fatal(err)
	}
	deletingDriver := New(cfg, nil)
	deletingDriver.connector = storageBackendConnector{backend: backend}
	creatingDriver := New(cfg, nil)
	creatingDriver.connector = storageBackendConnector{backend: backend}

	deleteResult := make(chan error, 1)
	go func() {
		_, err := deletingDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID})
		deleteResult <- err
	}()
	select {
	case <-backend.prepareStarted:
	case <-time.After(time.Second):
		t.Fatal("DeleteVolume did not establish its deletion claim")
	}
	_, err = creatingDriver.CreateVolume(context.Background(), createRequest)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("concurrent CreateVolume code=%s err=%v", status.Code(err), err)
	}
	close(backend.releaseTree)
	select {
	case err := <-deleteResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("DeleteVolume did not finish")
	}
	if got := strings.Join(backend.recordedActions(), ","); got != "prepare:export-old,tree:file-old,delete:export-old:true" {
		t.Fatalf("unsafe deletion ordering: %s", got)
	}
}

func TestCreateSMBVolume(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", BasePath: "/volumes", HandleKey: testHandleKey}
	backend := &fakeBackend{}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "smb-pvc",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "smb", "allowedNetworks": "10.1.0.0/16", "smbTrusteeDomain": "LOCAL", "smbTrusteeName": "k8s-smb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.protocol != protocolSMB || resp.GetVolume().GetVolumeContext()["shareName"] == "" {
		t.Fatalf("SMB was not provisioned: %#v", resp.GetVolume())
	}
}

func TestDeleteRefusesChangedProtocolResource(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/volumes/nfs/a", DirectoryID: "dir-a", ResourceID: "nfs-a", ResourceName: "/k8s/a", SpecFingerprint: testSpecFingerprint, DeleteData: true, NFSVersion: "4.1"}
	volumeID, err := h.encode(cfg.HandleKey)
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{prepareErr: fmt.Errorf("%w: export was retargeted", errVolumeIdentityChanged), attrs: &qumulo.FileAttributes{ID: h.DirectoryID, Path: h.FSPath}}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	_, err = d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("changed resource code=%s err=%v", status.Code(err), err)
	}
	if backend.treeDelete != "" {
		t.Fatalf("data deletion started after resource identity mismatch: %q", backend.treeDelete)
	}
}

func TestCreateRejectsConflictingNameBeforeCreatingDirectory(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", BasePath: "/volumes", HandleKey: testHandleKey}
	backend := &fakeBackend{checkErr: fmt.Errorf("%w: existing NFS volume", errVolumeConflict)}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "same-name",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "smb", "allowedNetworks": "10.1.0.0/16", "smbTrusteeDomain": "LOCAL", "smbTrusteeName": "k8s-smb"},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
	if backend.directory != "" || backend.quota != 0 {
		t.Fatalf("conflict created backend state: %#v", backend)
	}
}

func TestCreateRejectsInvalidParametersBeforeBackendMutation(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", BasePath: "/volumes", HandleKey: testHandleKey}
	tests := []map[string]string{
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "deleteDat": "false"},
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "basePath": "/safe/../other"},
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "nfsExportPrefix": "/safe\nother"},
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "endpoint": "https://q.example:8443"},
	}
	for _, params := range tests {
		backend := &fakeBackend{}
		d := New(cfg, nil)
		d.connector = fakeConnector{backend: backend}
		_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
			Name: "invalid-parameters", VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}, Parameters: params,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("parameters=%v code=%s err=%v", params, status.Code(err), err)
		}
		if backend.directory != "" || backend.protocol != "" || backend.quota != 0 {
			t.Fatalf("invalid parameters mutated backend: %#v", backend)
		}
	}
}

func TestCreateDoesNotApplyDirectoryModeBeforeWinningResourceClaim(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", BasePath: "/volumes", HandleKey: testHandleKey}
	backend := &fakeBackend{ensureErr: fmt.Errorf("%w: concurrent request won", errVolumeConflict)}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "mode-race",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "directoryMode": "0777"},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
	if len(backend.directoryModes) != 1 || backend.directoryModes[0] != "" {
		t.Fatalf("directory modes applied before claim: %#v", backend.directoryModes)
	}
}

func TestCreateVolumeReturnsStableIDAfterExpansionAndRejectsSpecChange(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", BasePath: "/volumes", HandleKey: testHandleKey}
	backend := &fakeBackend{}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	request := func(deleteData string) *csi.CreateVolumeRequest {
		return &csi.CreateVolumeRequest{
			Name: "stable-volume", CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
			VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
			Parameters:         map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "deleteData": deleteData},
		}
	}
	first, err := d.CreateVolume(context.Background(), request("true"))
	if err != nil {
		t.Fatal(err)
	}
	backend.quota = 2 << 30 // Simulate a successful ControllerExpandVolume.
	second, err := d.CreateVolume(context.Background(), request("true"))
	if err != nil {
		t.Fatal(err)
	}
	if first.GetVolume().GetVolumeId() != second.GetVolume().GetVolumeId() {
		t.Fatal("identical CreateVolume retry returned a different volume ID after expansion")
	}
	if second.GetVolume().GetCapacityBytes() != 2<<30 {
		t.Fatalf("retry capacity=%d want=%d", second.GetVolume().GetCapacityBytes(), int64(2<<30))
	}
	if _, err := d.CreateVolume(context.Background(), request("false")); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("incompatible retry code=%s err=%v", status.Code(err), err)
	}
}

func TestCreateVolumeRejectsEffectiveEquivalentParameterChange(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", BasePath: "/volumes", HandleKey: testHandleKey}
	backend := &fakeBackend{}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	request := func(explicitDefault bool) *csi.CreateVolumeRequest {
		params := map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8"}
		if explicitDefault {
			params["nfsVersion"] = "4.1"
		}
		return &csi.CreateVolumeRequest{
			Name: "exact-parameters", VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}, Parameters: params,
		}
	}
	if _, err := d.CreateVolume(context.Background(), request(false)); err != nil {
		t.Fatal(err)
	}
	mutations := len(backend.directoryModes)
	if _, err := d.CreateVolume(context.Background(), request(true)); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("changed parameters code=%s err=%v, want AlreadyExists", status.Code(err), err)
	}
	if len(backend.directoryModes) != mutations {
		t.Fatalf("incompatible retry mutated the directory: before=%d after=%d", mutations, len(backend.directoryModes))
	}
}

func TestCreateVolumeRejectsUnsupportedContentSource(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", BasePath: "/volumes", HandleKey: testHandleKey}
	d := New(cfg, nil)
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "clone", VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: "source-volume"}}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unsupported content source code=%s err=%v", status.Code(err), err)
	}
}

func TestRequestedCapacityHonorsUpperBoundWithoutRequiredBytes(t *testing.T) {
	const limit = int64(64 << 20)
	capacity, err := requestedCapacity(&csi.CapacityRange{LimitBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	if capacity != limit {
		t.Fatalf("capacity=%d want=%d", capacity, limit)
	}
}

func TestControllerExpandRequiresCapacityAndValidatesCapability(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", BasePath: "/volumes", HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/volumes/nfs/a", DirectoryID: "dir-a", ResourceID: "nfs-a", ResourceName: "/k8s/a", SpecFingerprint: testSpecFingerprint, Capacity: 1024, QuotaEnabled: true, NFSVersion: "4.1"}
	volumeID, err := h.encode(cfg.HandleKey)
	if err != nil {
		t.Fatal(err)
	}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: &fakeBackend{attrs: &qumulo.FileAttributes{ID: "dir-a", Path: h.FSPath}, quota: h.Capacity}}
	if _, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: volumeID}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing capacity code=%s err=%v", status.Code(err), err)
	}
	block := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}}
	if _, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: volumeID, CapacityRange: &csi.CapacityRange{RequiredBytes: 2048}, VolumeCapability: block}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("block capability code=%s err=%v", status.Code(err), err)
	}
}

func TestValidateVolumeCapabilitiesChecksIdentityAndContext(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/volumes/nfs/a", DirectoryID: "dir-a", ResourceID: "nfs-a", ResourceName: "/k8s/a", SpecFingerprint: testSpecFingerprint, QuotaEnabled: true, NFSVersion: "4.1"}
	volumeID, err := h.encode(cfg.HandleKey)
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	request := &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           volumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		VolumeContext:      volumeContextForHandle(h),
		Parameters:         map[string]string{"unverified": "caller-value"},
	}
	resp, err := d.ValidateVolumeCapabilities(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetConfirmed() == nil {
		t.Fatalf("expected confirmed response: %#v", resp)
	}
	if len(resp.GetConfirmed().GetParameters()) != 0 {
		t.Fatal("unverified creation parameters were echoed as confirmed")
	}

	request.VolumeContext = map[string]string{"protocol": "nfs", "server": "other.example"}
	resp, err = d.ValidateVolumeCapabilities(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetConfirmed() != nil || resp.GetMessage() == "" {
		t.Fatalf("mismatched context was confirmed: %#v", resp)
	}

	request.VolumeContext = volumeContextForHandle(h)
	backend.validateErr = fmt.Errorf("%w: export was deleted", errVolumeNotFound)
	if _, err := d.ValidateVolumeCapabilities(context.Background(), request); status.Code(err) != codes.NotFound {
		t.Fatalf("missing volume code=%s err=%v", status.Code(err), err)
	}
}

func TestControllerExpandRejectsDeletedProtocolResource(t *testing.T) {
	cfg := Config{Name: DefaultDriverName, Version: "test", Mode: "controller", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolSMB, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/volumes/smb/a", DirectoryID: "dir-a", ResourceID: "smb-a", ResourceName: "share-a", SpecFingerprint: testSpecFingerprint, QuotaEnabled: true}
	volumeID, err := h.encode(cfg.HandleKey)
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{validateErr: fmt.Errorf("%w: share was deleted", errVolumeNotFound)}
	d := New(cfg, nil)
	d.connector = fakeConnector{backend: backend}
	_, err = d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: volumeID, CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30}})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("deleted resource code=%s err=%v", status.Code(err), err)
	}
	if backend.quota != 0 {
		t.Fatalf("quota changed after resource deletion: %d", backend.quota)
	}
}

func TestCapabilitiesRejectDriverControlledMountOptions(t *testing.T) {
	capability := mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	capability.GetMount().MountFlags = []string{"vers=3"}
	if err := validateVolumeCapabilities(protocolNFS, []*csi.VolumeCapability{capability}); err == nil {
		t.Fatal("caller-controlled NFS version was accepted")
	}
	capability.GetMount().MountFlags = []string{"sec=ntlm"}
	if err := validateVolumeCapabilities(protocolSMB, []*csi.VolumeCapability{capability}); err == nil {
		t.Fatal("caller-controlled SMB authentication mode was accepted")
	}
	for _, mode := range []string{"ro", "rw"} {
		capability.GetMount().MountFlags = []string{mode}
		if err := validateVolumeCapabilities(protocolNFS, []*csi.VolumeCapability{capability}); err == nil {
			t.Fatalf("caller-controlled read-only state %q was accepted", mode)
		}
	}
}

type fakeMounter struct {
	record         mountRecord
	mounted        bool
	unmountCalls   int
	mountSource    string
	mountTarget    string
	fsType         string
	options        []string
	credentialData string
}

func (m *fakeMounter) Lookup(context.Context, string) (mountRecord, bool, error) {
	return m.record, m.mounted, nil
}
func (m *fakeMounter) Mount(_ context.Context, source, target, fsType string, options []string) error {
	m.mountSource, m.mountTarget, m.fsType = source, target, fsType
	m.options = append([]string(nil), options...)
	for _, option := range options {
		if strings.HasPrefix(option, "credentials=") {
			data, _ := os.ReadFile(strings.TrimPrefix(option, "credentials="))
			m.credentialData = string(data)
		}
	}
	recordedOptions := append([]string(nil), options...)
	m.record, m.mounted = mountRecord{Root: "/", Source: source, FSType: fsType, MountOptions: recordedOptions, Options: recordedOptions}, true
	return nil
}
func (m *fakeMounter) Unmount(context.Context, string) error {
	m.unmountCalls++
	m.mounted = false
	return nil
}

type blockingMounter struct {
	mu           sync.Mutex
	record       mountRecord
	mounted      bool
	lookupCalls  int
	mountStarted chan struct{}
	releaseMount chan struct{}
	secondLookup chan struct{}
	startOnce    sync.Once
	secondOnce   sync.Once
}

func (m *blockingMounter) Lookup(context.Context, string) (mountRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookupCalls++
	if m.lookupCalls == 2 {
		m.secondOnce.Do(func() { close(m.secondLookup) })
	}
	return m.record, m.mounted, nil
}

func (m *blockingMounter) Mount(_ context.Context, source, _ string, fsType string, options []string) error {
	m.startOnce.Do(func() { close(m.mountStarted) })
	<-m.releaseMount
	m.mu.Lock()
	defer m.mu.Unlock()
	recorded := append([]string(nil), options...)
	m.record = mountRecord{Root: "/", Source: source, FSType: fsType, MountOptions: recorded, Options: recorded}
	m.mounted = true
	return nil
}

func (m *blockingMounter) Unmount(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted = false
	return nil
}

func TestNodePublishSMBUsesEphemeralCredentialFile(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		Name: DefaultDriverName, Version: "test", Mode: "node", NodeID: "node-a",
		Endpoint: "q.example", DataServer: "q-data.example", RESTPort: "8000",
		KubeletRoot: root, StateDir: filepath.Join(root, "state"), HandleKey: testHandleKey,
	}
	h := volumeHandle{Protocol: protocolSMB, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/smb/a", DirectoryID: "dir-a", ResourceID: "share-id-a", ResourceName: "share-a", SpecFingerprint: testSpecFingerprint, SMBEncrypted: true}
	volumeID, _ := h.encode(cfg.HandleKey)
	target := filepath.Join(root, "pods", "pod-a", "volumes", "target")
	m := &fakeMounter{}
	d := New(cfg, nil)
	d.mounter = m
	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, TargetPath: target,
		VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		Secrets:          map[string]string{"username": "alice", "password": "not-logged", "domain": "EXAMPLE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.mountSource != "//q-data.example/share-a" || m.fsType != "cifs" || !strings.Contains(m.credentialData, "password=not-logged") {
		t.Fatalf("unexpected mount: %#v", m)
	}
	for _, option := range m.options {
		if strings.Contains(option, "not-logged") {
			t.Fatal("password leaked into mount arguments")
		}
		if strings.HasPrefix(option, "credentials=") {
			if _, err := os.Stat(strings.TrimPrefix(option, "credentials=")); !os.IsNotExist(err) {
				t.Fatal("temporary credential file was not removed")
			}
		}
	}
	// Publishing the same source to an already-mounted target is idempotent.
	if _, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, TargetPath: target, VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER), Secrets: map[string]string{"username": "alice", "password": "x"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNodePublishRejectsDifferentExistingSource(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Name: DefaultDriverName, Mode: "node", NodeID: "node", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", KubeletRoot: root, StateDir: root, HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/a", DirectoryID: "dir-a", ResourceID: "export-id-a", ResourceName: "/exports/a", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1"}
	volumeID, _ := h.encode(cfg.HandleKey)
	d := New(cfg, nil)
	d.mounter = &fakeMounter{mounted: true, record: mountRecord{Root: "/", Source: "other:/export", FSType: "nfs4"}}
	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: volumeID, TargetPath: filepath.Join(root, "pods", "a"), VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
}

func TestNodePublishRejectsReadonlyRetryAgainstReadWriteMount(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Name: DefaultDriverName, Mode: "node", NodeID: "node", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", KubeletRoot: root, StateDir: root, HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/a", DirectoryID: "dir-a", ResourceID: "export-id-a", ResourceName: "/exports/a", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1"}
	volumeID, _ := h.encode(cfg.HandleKey)
	m := &fakeMounter{mounted: true, record: mountRecord{
		Root: "/", Source: mountSource(h), FSType: "nfs4",
		Options: []string{"rw", "vers=4.1", "sec=sys", "hard", "timeo=600", "retrans=2", "nodev", "nosuid"},
	}}
	d := New(cfg, nil)
	d.mounter = m
	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, TargetPath: filepath.Join(root, "pods", "a"), Readonly: true,
		VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
	if m.mountSource != "" || m.unmountCalls != 0 {
		t.Fatalf("incompatible retry modified mount state: %#v", m)
	}
}

func TestNodePublishRequiresReadonlyForReaderOnlyAccessMode(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Name: DefaultDriverName, Mode: "node", NodeID: "node", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", KubeletRoot: root, StateDir: root, HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/a", DirectoryID: "dir-a", ResourceID: "export-id-a", ResourceName: "/exports/a", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1"}
	volumeID, _ := h.encode(cfg.HandleKey)
	m := &fakeMounter{}
	d := New(cfg, nil)
	d.mounter = m

	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, TargetPath: filepath.Join(root, "pods", "a"),
		VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code=%s err=%v, want InvalidArgument", status.Code(err), err)
	}
	if m.mountSource != "" {
		t.Fatalf("inconsistent reader-only request reached the mounter: %#v", m)
	}

	if _, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, TargetPath: filepath.Join(root, "pods", "a"), Readonly: true,
		VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY),
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.options, "ro") || slices.Contains(m.options, "rw") {
		t.Fatalf("reader-only request was not mounted read-only: %v", m.options)
	}
}

func TestNodePublishRejectsRetryThatOmitsOriginallyRequestedFlag(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Name: DefaultDriverName, Mode: "node", NodeID: "node", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", KubeletRoot: root, StateDir: root, HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/a", DirectoryID: "dir-a", ResourceID: "export-id-a", ResourceName: "/exports/a", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1"}
	volumeID, _ := h.encode(cfg.HandleKey)
	d := New(cfg, nil)
	m := &fakeMounter{}
	d.mounter = m
	target := filepath.Join(root, "pods", "a")
	firstCapability := mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	firstCapability.GetMount().MountFlags = []string{"noexec"}
	if _, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, TargetPath: target, VolumeCapability: firstCapability,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, TargetPath: target,
		VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
}

func TestNodePublishDurableFingerprintSurvivesRestartAndUnpublish(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Name: DefaultDriverName, Mode: "node", NodeID: "node", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", KubeletRoot: root, StateDir: root, HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/a", DirectoryID: "dir-a", ResourceID: "export-id-a", ResourceName: "/exports/a", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1"}
	volumeID, err := h.encode(cfg.HandleKey)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "pods", "a")
	m := &fakeMounter{}
	request := func(flags ...string) *csi.NodePublishVolumeRequest {
		capability := mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
		capability.GetMount().MountFlags = append([]string(nil), flags...)
		return &csi.NodePublishVolumeRequest{VolumeId: volumeID, TargetPath: target, VolumeCapability: capability}
	}
	first := New(cfg, nil)
	first.mounter = m
	if _, err := first.NodePublishVolume(context.Background(), request("noexec")); err != nil {
		t.Fatal(err)
	}

	// A new Driver models node-pod recreation: only the mount table and the
	// socket-hostPath marker survive.
	restarted := New(cfg, nil)
	restarted.mounter = m
	if _, err := restarted.NodePublishVolume(context.Background(), request("noexec")); err != nil {
		t.Fatalf("identical publish after restart failed: %v", err)
	}
	if _, err := restarted.NodePublishVolume(context.Background(), request()); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("changed publish after restart code=%s err=%v, want AlreadyExists", status.Code(err), err)
	}
	if _, err := restarted.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: volumeID, TargetPath: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := readPublishState(cfg, target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unpublish retained durable publish state: %v", err)
	}
}

func TestNodePublishFailsClosedWhenMountedStateMarkerIsMissing(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Name: DefaultDriverName, Mode: "node", NodeID: "node", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", KubeletRoot: root, StateDir: root, HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/a", DirectoryID: "dir-a", ResourceID: "export-id-a", ResourceName: "/exports/a", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1"}
	volumeID, err := h.encode(cfg.HandleKey)
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeMounter{mounted: true, record: mountRecord{
		Root: "/", Source: mountSource(h), FSType: "nfs4",
		Options: []string{"rw", "vers=4.1", "sec=sys", "hard", "timeo=600", "retrans=2", "nodev", "nosuid"},
	}}
	d := New(cfg, nil)
	d.mounter = m
	_, err = d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, TargetPath: filepath.Join(root, "pods", "a"),
		VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code=%s err=%v, want fail-closed AlreadyExists", status.Code(err), err)
	}
}

func TestNodePublishSerializesConcurrentOperationsForTarget(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Name: DefaultDriverName, Mode: "node", NodeID: "node", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", KubeletRoot: root, StateDir: root, HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/a", DirectoryID: "dir-a", ResourceID: "export-id-a", ResourceName: "/exports/a", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1"}
	volumeID, _ := h.encode(cfg.HandleKey)
	m := &blockingMounter{mountStarted: make(chan struct{}), releaseMount: make(chan struct{}), secondLookup: make(chan struct{})}
	d := New(cfg, nil)
	d.mounter = m
	request := func() *csi.NodePublishVolumeRequest {
		return &csi.NodePublishVolumeRequest{
			VolumeId: volumeID, TargetPath: filepath.Join(root, "pods", "a"),
			VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		}
	}
	results := make(chan error, 2)
	go func() {
		_, err := d.NodePublishVolume(context.Background(), request())
		results <- err
	}()
	select {
	case <-m.mountStarted:
	case <-time.After(time.Second):
		t.Fatal("first publish did not reach mount")
	}
	secondEntered := make(chan struct{})
	go func() {
		close(secondEntered)
		_, err := d.NodePublishVolume(context.Background(), request())
		results <- err
	}()
	<-secondEntered
	select {
	case <-m.secondLookup:
		t.Fatal("second publish reached the mounter before the first completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(m.releaseMount)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-m.secondLookup:
	case <-time.After(time.Second):
		t.Fatal("second publish never ran after the first completed")
	}
}

func TestNodeUnpublishRefusesDifferentMountedSource(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Name: DefaultDriverName, Mode: "node", NodeID: "node", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", KubeletRoot: root, StateDir: root, HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/a", DirectoryID: "dir-a", ResourceID: "export-id-a", ResourceName: "/exports/a", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1"}
	volumeID, _ := h.encode(cfg.HandleKey)
	m := &fakeMounter{mounted: true, record: mountRecord{Root: "/", Source: "other.example:/exports/b", FSType: "nfs4"}}
	d := New(cfg, nil)
	d.mounter = m
	_, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: volumeID, TargetPath: filepath.Join(root, "pods", "a"),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
	if m.unmountCalls != 0 || !m.mounted {
		t.Fatalf("foreign mount was modified: calls=%d mounted=%v", m.unmountCalls, m.mounted)
	}
}

func TestNodeUnpublishUnmountsExpectedSource(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Name: DefaultDriverName, Mode: "node", NodeID: "node", Endpoint: "q.example", DataServer: "q.example", RESTPort: "8000", KubeletRoot: root, StateDir: root, HandleKey: testHandleKey}
	h := volumeHandle{Protocol: protocolNFS, Endpoint: cfg.Endpoint, RESTPort: cfg.RESTPort, Server: cfg.DataServer, FSPath: "/vol/a", DirectoryID: "dir-a", ResourceID: "export-id-a", ResourceName: "/exports/a", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1"}
	volumeID, _ := h.encode(cfg.HandleKey)
	m := &fakeMounter{mounted: true, record: mountRecord{Root: "/", Source: mountSource(h), FSType: "nfs4"}}
	d := New(cfg, nil)
	d.mounter = m
	if _, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: volumeID, TargetPath: filepath.Join(root, "pods", "a"),
	}); err != nil {
		t.Fatal(err)
	}
	if m.unmountCalls != 1 || m.mounted {
		t.Fatalf("expected mount was not unpublished: calls=%d mounted=%v", m.unmountCalls, m.mounted)
	}
}

func mountCapability(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}
