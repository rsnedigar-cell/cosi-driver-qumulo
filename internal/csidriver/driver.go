package csidriver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

const defaultVolumeBytes = int64(1 << 30)

// Driver serves CSI Identity, Controller, and Node RPCs. Deployments normally
// run controller and node modes separately, but all mode is useful in tests.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	cfg       Config
	connector backendConnector
	mounter   mounter
	log       *slog.Logger
	createMu  sync.Mutex
	creates   map[string]*createNameLock
	targetMu  sync.Mutex
	targets   map[string]*nodeTargetLock
}

type createNameLock struct {
	mu   sync.Mutex
	refs int
}

type nodeTargetLock struct {
	mu                 sync.Mutex
	refs               int
	publishFingerprint string
}

func New(cfg Config, log *slog.Logger) *Driver {
	if log == nil {
		log = slog.Default()
	}
	return &Driver{
		cfg:       cfg,
		connector: newQumuloConnector(cfg, log),
		mounter:   newPlatformMounter(),
		log:       log,
		creates:   map[string]*createNameLock{},
		targets:   map[string]*nodeTargetLock{},
	}
}

func (d *Driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	if strings.TrimSpace(d.cfg.Name) == "" {
		return nil, status.Error(codes.Unavailable, "CSI driver name is not configured")
	}
	return &csi.GetPluginInfoResponse{Name: d.cfg.Name, VendorVersion: d.cfg.Version}, nil
}

func (d *Driver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	caps := []*csi.PluginCapability{}
	if d.controllerEnabled() {
		caps = append(caps,
			&csi.PluginCapability{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE}}},
			&csi.PluginCapability{Type: &csi.PluginCapability_VolumeExpansion_{VolumeExpansion: &csi.PluginCapability_VolumeExpansion{Type: csi.PluginCapability_VolumeExpansion_ONLINE}}},
		)
	}
	return &csi.GetPluginCapabilitiesResponse{Capabilities: caps}, nil
}

func (d *Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	ready := strings.TrimSpace(d.cfg.Name) != "" && strings.TrimSpace(d.cfg.Address) != "" && len(d.cfg.HandleKey) >= 32
	if d.controllerEnabled() {
		ready = ready && strings.TrimSpace(d.cfg.Endpoint) != ""
	}
	if d.nodeEnabled() {
		ready = ready && strings.TrimSpace(d.cfg.NodeID) != ""
	}
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(ready)}, nil
}

func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if !d.controllerEnabled() {
		return nil, status.Error(codes.Unimplemented, "controller service is disabled")
	}
	if strings.TrimSpace(req.GetName()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	unlock := d.lockCreateName("name\x00" + req.GetName())
	defer unlock()
	if req.GetVolumeContentSource() != nil {
		return nil, status.Error(codes.InvalidArgument, "volume cloning and snapshot sources are not supported")
	}
	opts, err := parseVolumeOptions(d.cfg, req.GetName(), req.GetParameters())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	unlockPath := d.lockCreateName("path\x00" + opts.FSPath)
	defer unlockPath()
	if err := validateVolumeCapabilities(opts.Protocol, req.GetVolumeCapabilities()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	capacity, err := requestedCapacity(req.GetCapacityRange())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	opts.RequestedCapacity = capacity
	if req.GetCapacityRange() != nil {
		opts.CapacityLimit = req.GetCapacityRange().GetLimitBytes()
	}

	backend, err := d.connector.Connect(ctx, opts.Endpoint, opts.RESTPort, req.GetSecrets())
	if err != nil {
		return nil, toGRPCError(err)
	}
	if _, err := backend.EnsureVersion(ctx, d.cfg.VersionFloor); err != nil {
		return nil, toGRPCError(err)
	}
	if err := backend.CheckVolume(ctx, opts); err != nil {
		return nil, toGRPCError(err)
	}
	// Establish the path without mutating its permissions. The protocol
	// resource description below is the durable, cluster-side claim for this
	// CreateVolume specification. Applying the requested mode before winning
	// that claim would let an incompatible request racing during an HA leader
	// handoff chmod the eventual winner's directory.
	attrs, err := backend.EnsureDirectory(ctx, opts.FSPath, "")
	if err != nil {
		return nil, toGRPCError(err)
	}
	if attrs == nil || attrs.ID == "" {
		return nil, status.Error(codes.Internal, "Qumulo returned no file ID for the volume directory")
	}
	var resource storageResource
	switch opts.Protocol {
	case protocolNFS:
		resource, err = backend.EnsureNFS(ctx, opts)
	case protocolSMB:
		resource, err = backend.EnsureSMB(ctx, opts)
	}
	if err != nil {
		return nil, toGRPCError(err)
	}
	if resource.ID == "" || resource.Name == "" {
		return nil, status.Error(codes.Internal, "Qumulo returned an incomplete file share identity")
	}
	configuredAttrs, err := backend.EnsureDirectory(ctx, opts.FSPath, opts.DirectoryMode)
	if err != nil {
		return nil, toGRPCError(err)
	}
	if configuredAttrs == nil || configuredAttrs.ID != attrs.ID {
		return nil, status.Error(codes.FailedPrecondition, "volume directory identity changed while provisioning")
	}
	attrs = configuredAttrs

	// The share/export description is the cluster-side atomic claim for the
	// immutable CreateVolume specification. Apply quota only after that claim
	// so an incompatible concurrent request cannot grow the winner's volume
	// before losing the name race.
	if opts.QuotaEnabled && capacity > 0 {
		capacity, err = backend.EnsureQuota(ctx, attrs.ID, capacity)
		if err != nil {
			return nil, toGRPCError(err)
		}
		if limit := req.GetCapacityRange().GetLimitBytes(); limit > 0 && capacity > limit {
			return nil, status.Errorf(codes.AlreadyExists, "existing volume capacity %d exceeds requested limit %d", capacity, limit)
		}
	}

	specFingerprint, err := volumeSpecFingerprint(opts)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	handle := volumeHandle{
		Protocol:        opts.Protocol,
		Endpoint:        opts.Endpoint,
		RESTPort:        opts.RESTPort,
		Server:          opts.Server,
		FSPath:          opts.FSPath,
		DirectoryID:     attrs.ID,
		ResourceID:      resource.ID,
		ResourceName:    resource.Name,
		SpecFingerprint: specFingerprint,
		QuotaEnabled:    opts.QuotaEnabled,
		DeleteData:      opts.DeleteData,
		NFSVersion:      opts.NFSVersion,
		SMBEncrypted:    opts.SMBRequireEncryption,
	}
	// Revalidate immediately before returning so a concurrent HA replica that
	// has durably claimed deletion cannot leave CreateVolume reporting a stale
	// export/share as successfully provisioned.
	if err := backend.ValidateVolume(ctx, handle); err != nil {
		return nil, toGRPCError(err)
	}
	volumeID, err := handle.encode(d.cfg.HandleKey)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      volumeID,
		CapacityBytes: capacity,
		VolumeContext: volumeContextForHandle(handle),
	}}, nil
}

func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if !d.controllerEnabled() {
		return nil, status.Error(codes.Unimplemented, "controller service is disabled")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	h, err := decodeVolumeHandle(req.GetVolumeId(), d.cfg)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	unlockPath := d.lockCreateName("path\x00" + h.FSPath)
	defer unlockPath()
	backend, err := d.connector.Connect(ctx, h.Endpoint, h.RESTPort, req.GetSecrets())
	if err != nil {
		return nil, toGRPCError(err)
	}
	if !h.DeleteData {
		if err := backend.DeleteVolumeResource(ctx, h, false); err != nil {
			return nil, toGRPCError(err)
		}
		return &csi.DeleteVolumeResponse{}, nil
	}
	attrs, err := backend.FileAttributes(ctx, h.FSPath)
	if err != nil {
		if api, ok := qumulo.AsAPIError(err); ok && api.IsNotFound() {
			// Data is already gone. Convert an exact managed resource to the
			// per-handle tombstone before deleting it; if the resource is also
			// gone, the operation is already complete.
			if err := backend.PrepareVolumeDeletion(ctx, h); err != nil {
				if errors.Is(err, errVolumeResourceMissing) {
					return &csi.DeleteVolumeResponse{}, nil
				}
				return nil, toGRPCError(err)
			}
			if err := backend.DeleteVolumeResource(ctx, h, true); err != nil {
				return nil, toGRPCError(err)
			}
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, toGRPCError(err)
	}
	if attrs == nil || attrs.ID == "" {
		return nil, status.Error(codes.Internal, "Qumulo returned no stable identity for the volume directory")
	}
	if attrs.ID != h.DirectoryID {
		return nil, status.Error(codes.FailedPrecondition, "volume path now refers to a different filesystem object; refusing data deletion")
	}
	if err := backend.PrepareVolumeDeletion(ctx, h); err != nil {
		return nil, toGRPCError(err)
	}
	if err := backend.TreeDelete(ctx, attrs.ID); err != nil {
		return nil, toGRPCError(err)
	}
	if err := backend.DeleteVolumeResource(ctx, h, true); err != nil {
		return nil, toGRPCError(err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if !d.controllerEnabled() {
		return nil, status.Error(codes.Unimplemented, "controller service is disabled")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	h, err := decodeVolumeHandle(req.GetVolumeId(), d.cfg)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateVolumeCapabilities(h.Protocol, req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil
	}
	expectedContext := volumeContextForHandle(h)
	if len(req.GetVolumeContext()) > 0 && !volumeContextCompatible(req.GetVolumeContext(), expectedContext) {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: "volume context does not match the provisioned volume"}, nil
	}
	backend, err := d.connector.Connect(ctx, h.Endpoint, h.RESTPort, req.GetSecrets())
	if err != nil {
		return nil, toGRPCError(err)
	}
	if err := backend.ValidateVolume(ctx, h); err != nil {
		return nil, toGRPCError(err)
	}
	confirmedContext := map[string]string(nil)
	if len(req.GetVolumeContext()) > 0 {
		confirmedContext = expectedContext
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeCapabilities: req.GetVolumeCapabilities(),
		VolumeContext:      confirmedContext,
		// Creation parameters are intentionally omitted: the signed handle
		// does not contain the full original parameter set, so echoing caller
		// input here would falsely claim that it had been validated.
	}}, nil
}

func (d *Driver) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: []*csi.ControllerServiceCapability{
		controllerCapability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
		controllerCapability(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
	}}, nil
}

func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if !d.controllerEnabled() {
		return nil, status.Error(codes.Unimplemented, "controller service is disabled")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	h, err := decodeVolumeHandle(req.GetVolumeId(), d.cfg)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "capacity range is required")
	}
	if capability := req.GetVolumeCapability(); capability != nil {
		if err := validateVolumeCapabilities(h.Protocol, []*csi.VolumeCapability{capability}); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	capacity, err := requestedCapacity(req.GetCapacityRange())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !h.QuotaEnabled {
		return nil, status.Error(codes.FailedPrecondition, "volume quota is disabled; expansion cannot be enforced")
	}
	backend, err := d.connector.Connect(ctx, h.Endpoint, h.RESTPort, req.GetSecrets())
	if err != nil {
		return nil, toGRPCError(err)
	}
	if err := backend.ValidateVolume(ctx, h); err != nil {
		return nil, toGRPCError(err)
	}
	capacity, err = backend.EnsureQuota(ctx, h.DirectoryID, capacity)
	if err != nil {
		return nil, toGRPCError(err)
	}
	if limit := req.GetCapacityRange().GetLimitBytes(); limit > 0 && capacity > limit {
		return nil, status.Errorf(codes.OutOfRange, "current volume capacity %d exceeds requested limit %d", capacity, limit)
	}
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: capacity, NodeExpansionRequired: false}, nil
}

func volumeContextForHandle(h volumeHandle) map[string]string {
	context := map[string]string{
		"protocol": string(h.Protocol),
		"server":   h.Server,
	}
	if h.Protocol == protocolNFS {
		context["exportPath"] = h.ResourceName
		context["nfsVersion"] = h.NFSVersion
	} else {
		context["shareName"] = h.ResourceName
		context["encryptionRequired"] = fmt.Sprintf("%t", h.SMBEncrypted)
	}
	return context
}

func equalStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

// volumeContextCompatible reports whether a request's volume context is
// consistent with the signed handle. Kubernetes adds its own keys to the
// context of dynamically provisioned volumes (for example
// storage.kubernetes.io/csiProvisionerIdentity), so foreign keys are
// ignored; only a conflicting value for a key the driver itself publishes
// is a mismatch.
func volumeContextCompatible(request, expected map[string]string) bool {
	for key, want := range expected {
		if got, ok := request[key]; ok && got != want {
			return false
		}
	}
	return true
}

func controllerCapability(kind csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{Type: &csi.ControllerServiceCapability_Rpc{
		Rpc: &csi.ControllerServiceCapability_RPC{Type: kind},
	}}
}

func requestedCapacity(r *csi.CapacityRange) (int64, error) {
	if r == nil {
		return defaultVolumeBytes, nil
	}
	required, limit := r.GetRequiredBytes(), r.GetLimitBytes()
	if required < 0 || limit < 0 {
		return 0, fmt.Errorf("capacity values must not be negative")
	}
	if required == 0 {
		required = defaultVolumeBytes
		if limit > 0 && limit < required {
			required = limit
		}
	}
	if limit > 0 && required > limit {
		return 0, fmt.Errorf("required capacity %d exceeds limit %d", required, limit)
	}
	if required == math.MaxInt64 {
		return 0, fmt.Errorf("requested capacity is too large")
	}
	return required, nil
}

func validateVolumeCapabilities(proto protocol, caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return fmt.Errorf("at least one volume capability is required")
	}
	for _, cap := range caps {
		if cap == nil || cap.GetMount() == nil {
			return fmt.Errorf("only mounted filesystem volumes are supported")
		}
		mode := cap.GetAccessMode().GetMode()
		if mode == csi.VolumeCapability_AccessMode_UNKNOWN {
			return fmt.Errorf("volume access mode is required")
		}
		fsType := strings.ToLower(strings.TrimSpace(cap.GetMount().GetFsType()))
		switch proto {
		case protocolNFS:
			if fsType != "" && fsType != "nfs" && fsType != "nfs4" {
				return fmt.Errorf("NFS volume requested incompatible fsType %q", fsType)
			}
			if err := rejectControlledMountOptions(cap.GetMount().GetMountFlags(), "vers", "nfsvers", "proto", "soft", "softerr"); err != nil {
				return err
			}
		case protocolSMB:
			if fsType != "" && fsType != "cifs" && fsType != "smb3" {
				return fmt.Errorf("SMB volume requested incompatible fsType %q", fsType)
			}
			if err := rejectControlledMountOptions(cap.GetMount().GetMountFlags(), "vers", "seal", "noseal", "sec"); err != nil {
				return err
			}
		}
		if _, err := sanitizedMountFlags(cap.GetMount().GetMountFlags()); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) controllerEnabled() bool {
	return d.cfg.Mode == "controller" || d.cfg.Mode == "all"
}

func (d *Driver) nodeEnabled() bool {
	return d.cfg.Mode == "node" || d.cfg.Mode == "all"
}

func (d *Driver) lockCreateName(name string) func() {
	d.createMu.Lock()
	lock := d.creates[name]
	if lock == nil {
		lock = &createNameLock{}
		d.creates[name] = lock
	}
	lock.refs++
	d.createMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		d.createMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(d.creates, name)
		}
		d.createMu.Unlock()
	}
}

func toGRPCError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if errors.Is(err, errVolumeConflict) {
		return status.Error(codes.AlreadyExists, err.Error())
	}
	if errors.Is(err, errVolumeNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	if errors.Is(err, errVolumeIdentityChanged) || errors.Is(err, errVolumeResourceMissing) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	if api, ok := qumulo.AsAPIError(err); ok {
		switch {
		case api.IsAuth():
			if api.StatusCode == http.StatusForbidden {
				return status.Error(codes.PermissionDenied, api.Error())
			}
			return status.Error(codes.Unauthenticated, api.Error())
		case api.IsAlreadyExists():
			return status.Error(codes.AlreadyExists, api.Error())
		case api.IsNotFound():
			return status.Error(codes.NotFound, api.Error())
		case api.StatusCode == http.StatusPreconditionFailed:
			return status.Error(codes.Aborted, api.Error())
		case api.StatusCode == http.StatusTooManyRequests || api.StatusCode >= 500:
			return status.Error(codes.Unavailable, api.Error())
		case api.StatusCode == http.StatusBadRequest:
			return status.Error(codes.InvalidArgument, api.Error())
		}
	}
	return status.Error(codes.Internal, err.Error())
}
