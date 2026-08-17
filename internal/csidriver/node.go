package csidriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if !d.nodeEnabled() {
		return nil, status.Error(codes.Unimplemented, "node service is disabled")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if strings.TrimSpace(req.GetTargetPath()) == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if err := validateTargetPath(d.cfg.KubeletRoot, req.GetTargetPath()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	targetState, unlockTarget := d.lockNodeTarget(req.GetTargetPath())
	defer unlockTarget()
	h, err := decodeVolumeHandle(req.GetVolumeId(), d.cfg)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateVolumeCapabilities(h.Protocol, []*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if readerOnlyAccessMode(req.GetVolumeCapability()) && !req.GetReadonly() {
		return nil, status.Error(codes.InvalidArgument, "reader-only volume access mode requires readonly=true")
	}
	if len(req.GetVolumeContext()) > 0 && !volumeContextCompatible(req.GetVolumeContext(), volumeContextForHandle(h)) {
		return nil, status.Error(codes.InvalidArgument, "volume context does not match the signed volume handle")
	}
	publishFingerprint, err := nodePublishFingerprint(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	source := mountSource(h)
	flags := req.GetVolumeCapability().GetMount().GetMountFlags()
	var fsType string
	var base []string
	switch h.Protocol {
	case protocolNFS:
		if h.NFSVersion == "" {
			h.NFSVersion = "4.1"
		}
		if err := rejectControlledMountOptions(flags,
			"vers", "version", "nfsvers", "mountvers", "minorversion",
			"sec", "security", "xprtsec",
			"addr", "mountaddr", "mounthost", "clientaddr",
			"port", "mountport", "nfsprog", "mountprog",
			"proto", "protocol", "mountproto", "tcp", "tcp6", "udp", "udp6", "rdma", "rdma6",
			"timeo", "retrans", "soft", "softerr", "softreval", "nosoftreval"); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		fsType = "nfs4"
		base = []string{"vers=" + h.NFSVersion, "sec=sys", "hard", "timeo=600", "retrans=2", "nodev", "nosuid"}
		if h.NFSVersion == "3" {
			fsType = "nfs"
			// NFSv3 file locking needs rpc.statd/rpcbind, which container
			// hosts rarely run; without nolock the mount hangs or fails.
			base = append(base, "nolock")
		}
	case protocolSMB:
		if err := rejectControlledMountOptions(flags,
			"vers", "version", "sec", "security", "seal", "noseal", "sign", "nosign",
			"addr", "ip", "port", "unc", "source", "prefixpath", "prepath",
			"cred", "creds", "credential", "credentials", "credfile", "credentialsfile",
			"user", "username",
			"pass", "password", "pass2", "password2",
			"domain", "dom", "workgroup", "domainauto",
			"guest", "nullauth", "multiuser", "cruid", "upcall_target",
			"proto", "protocol", "tcp", "rdma", "soft", "softerr", "softreval",
			"sharesock", "nosharesock"); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		fsType = "cifs"
		base = []string{"vers=3.1.1", "nosharesock", "actimeo=30", "nodev", "nosuid"}
		if h.SMBEncrypted {
			base = append(base, "seal")
		}
	}

	record, mounted, err := d.mounter.Lookup(ctx, req.GetTargetPath())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if mounted {
		if err := validateExistingMount(h, record, req.GetReadonly(), flags); err != nil {
			return nil, status.Errorf(codes.AlreadyExists, "target is already mounted incompatibly: %v", err)
		}
		persistedFingerprint, err := readPublishState(d.cfg, req.GetTargetPath())
		if err != nil {
			return nil, status.Error(codes.AlreadyExists, "target is already mounted but its original publish arguments cannot be verified")
		}
		if persistedFingerprint != publishFingerprint {
			return nil, status.Error(codes.AlreadyExists, "target is already mounted with different publish arguments")
		}
		if targetState.publishFingerprint != "" && targetState.publishFingerprint != publishFingerprint {
			return nil, status.Error(codes.AlreadyExists, "target is already mounted with different publish arguments")
		}
		targetState.publishFingerprint = publishFingerprint
		return &csi.NodePublishVolumeResponse{}, nil
	}
	targetState.publishFingerprint = ""

	cleanup := func() {}
	if h.Protocol == protocolSMB {
		credentialsPath, removeCredentials, err := writeSMBCredentials(d.cfg.StateDir, req.GetSecrets())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		cleanup = removeCredentials
		base = append(base, "credentials="+credentialsPath)
	}
	defer cleanup()
	options, err := mergeMountOptions(base, flags, req.GetReadonly())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// Persist the non-secret request fingerprint before invoking mount. If the
	// mount helper reports an error after the kernel accepted the mount, a new
	// node pod can still verify the original publish arguments safely.
	if err := writePublishState(d.cfg, req.GetTargetPath(), publishFingerprint); err != nil {
		return nil, status.Error(codes.Internal, "persist node publish arguments: "+err.Error())
	}
	targetState.publishFingerprint = publishFingerprint
	if err := d.mounter.Mount(ctx, source, req.GetTargetPath(), fsType, options); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if !d.nodeEnabled() {
		return nil, status.Error(codes.Unimplemented, "node service is disabled")
	}
	if strings.TrimSpace(req.GetVolumeId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if strings.TrimSpace(req.GetTargetPath()) == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if err := validateTargetPath(d.cfg.KubeletRoot, req.GetTargetPath()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	targetState, unlockTarget := d.lockNodeTarget(req.GetTargetPath())
	defer unlockTarget()
	// Unpublish must stay possible even when the handle can no longer be
	// decoded (rotated handle key, changed endpoint binding): a volume that
	// cannot be unmounted wedges pod teardown forever. The target path is
	// already validated to live under the kubelet root and serialized by the
	// per-target lock, so unmounting by path alone is safe; the mounted-
	// identity cross-check is only possible when the handle decodes.
	h, herr := decodeVolumeHandle(req.GetVolumeId(), d.cfg)
	if herr != nil {
		d.log.Warn("unpublish proceeding without a decodable volume handle", "target", req.GetTargetPath(), "err", herr)
	}
	record, mounted, err := d.mounter.Lookup(ctx, req.GetTargetPath())
	if err != nil {
		if os.IsNotExist(err) {
			if err := removePublishState(d.cfg, req.GetTargetPath()); err != nil {
				return nil, status.Error(codes.Internal, "remove node publish state: "+err.Error())
			}
			targetState.publishFingerprint = ""
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	if mounted {
		if herr == nil {
			if err := validateMountedVolumeIdentity(h, record); err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "target is mounted by a different volume: %v", err)
			}
		}
		if err := d.mounter.Unmount(ctx, req.GetTargetPath()); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if err := removePublishState(d.cfg, req.GetTargetPath()); err != nil {
		return nil, status.Error(codes.Internal, "remove node publish state: "+err.Error())
	}
	targetState.publishFingerprint = ""
	if err := os.Remove(req.GetTargetPath()); err != nil && !os.IsNotExist(err) {
		// Kubelet owns parent directories. Only remove the empty leaf we made.
		d.log.Warn("remove CSI target directory", "target", req.GetTargetPath(), "err", err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (d *Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	// Network filesystems are published directly; STAGE_UNSTAGE and node-side
	// expansion are intentionally not advertised.
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (d *Driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	if strings.TrimSpace(d.cfg.NodeID) == "" {
		return nil, status.Error(codes.Unavailable, "node ID is not configured")
	}
	return &csi.NodeGetInfoResponse{NodeId: d.cfg.NodeID}, nil
}

func (d *Driver) NodeExpandVolume(context.Context, *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "network filesystem volumes do not require node-side expansion")
}

func mountSource(h volumeHandle) string {
	server := strings.Trim(h.Server, "[]")
	if ip := net.ParseIP(server); ip != nil && strings.Contains(server, ":") {
		server = "[" + server + "]"
	}
	if h.Protocol == protocolSMB {
		return "//" + server + "/" + strings.Trim(h.ResourceName, "/")
	}
	return server + ":" + h.ResourceName
}

func readerOnlyAccessMode(capability *csi.VolumeCapability) bool {
	mode := capability.GetAccessMode().GetMode()
	return mode == csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY ||
		mode == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
}

func sameMountSource(proto protocol, actual, expected string) bool {
	if proto == protocolSMB {
		return strings.EqualFold(strings.TrimRight(actual, "/"), strings.TrimRight(expected, "/"))
	}
	return actual == expected
}

type nodePublishFingerprintData struct {
	VolumeID          string   `json:"volumeID"`
	StagingTargetPath string   `json:"stagingTargetPath"`
	FSType            string   `json:"fsType"`
	AccessMode        int32    `json:"accessMode"`
	MountFlags        []string `json:"mountFlags"`
	ReadOnly          bool     `json:"readOnly"`
	PublishContext    []string `json:"publishContext"`
	VolumeContext     []string `json:"volumeContext"`
}

func nodePublishFingerprint(req *csi.NodePublishVolumeRequest) (string, error) {
	flags, err := sanitizedMountFlags(req.GetVolumeCapability().GetMount().GetMountFlags())
	if err != nil {
		return "", err
	}
	spec := nodePublishFingerprintData{
		VolumeID:          req.GetVolumeId(),
		StagingTargetPath: req.GetStagingTargetPath(),
		FSType:            strings.ToLower(strings.TrimSpace(req.GetVolumeCapability().GetMount().GetFsType())),
		AccessMode:        int32(req.GetVolumeCapability().GetAccessMode().GetMode()),
		MountFlags:        flags,
		ReadOnly:          req.GetReadonly(),
		PublishContext:    canonicalMapEntries(req.GetPublishContext()),
		VolumeContext:     canonicalMapEntries(req.GetVolumeContext()),
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode node publish arguments: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalMapEntries(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		out = append(out, key, values[key])
	}
	return out
}

func (d *Driver) lockNodeTarget(target string) (*nodeTargetLock, func()) {
	key := filepath.Clean(target)
	d.targetMu.Lock()
	if d.targets == nil {
		d.targets = map[string]*nodeTargetLock{}
	}
	lock := d.targets[key]
	if lock == nil {
		lock = &nodeTargetLock{}
		d.targets[key] = lock
	}
	lock.refs++
	d.targetMu.Unlock()

	lock.mu.Lock()
	return lock, func() {
		d.targetMu.Lock()
		lock.refs--
		if lock.refs == 0 && lock.publishFingerprint == "" && d.targets[key] == lock {
			delete(d.targets, key)
		}
		d.targetMu.Unlock()
		lock.mu.Unlock()
	}
}

func rejectControlledMountOptions(flags []string, keys ...string) error {
	blocked := map[string]bool{}
	for _, key := range keys {
		blocked[strings.ToLower(key)] = true
	}
	clean, err := sanitizedMountFlags(flags)
	if err != nil {
		return err
	}
	for _, option := range clean {
		key, err := mountOptionKey(option)
		if err != nil {
			return err
		}
		if blocked[key] {
			return fmt.Errorf("mount option %q is controlled by the Qumulo CSI driver", key)
		}
	}
	return nil
}
