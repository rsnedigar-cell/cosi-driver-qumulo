package csidriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

var (
	errVolumeConflict        = errors.New("volume name is already in use with different parameters")
	errVolumeNotFound        = errors.New("volume no longer exists")
	errVolumeIdentityChanged = errors.New("volume resource identity changed")
	errVolumeResourceMissing = errors.New("volume resource is missing before deletion was claimed")
	errSnapshotsPresent      = errors.New("volume has driver-created snapshots; delete VolumeSnapshot objects first")
	errSnapshotsUnsupported  = errors.New("Qumulo Core does not support directory snapshots")
)

type storageResource struct {
	ID   string
	Name string
	Path string
}

type storageBackend interface {
	EnsureVersion(context.Context, string) (string, error)
	CheckVolume(context.Context, volumeOptions) error
	EnsureDirectory(context.Context, string, string) (*qumulo.FileAttributes, error)
	EnsureQuota(context.Context, string, int64) (int64, error)
	EnsureNFS(context.Context, volumeOptions) (storageResource, error)
	EnsureSMB(context.Context, volumeOptions) (storageResource, error)
	ValidateVolume(context.Context, volumeHandle) error
	PrepareVolumeDeletion(context.Context, volumeHandle) error
	DeleteVolumeResource(context.Context, volumeHandle, bool) error
	FileAttributes(context.Context, string) (*qumulo.FileAttributes, error)
	TreeDelete(context.Context, string) error
	EnsureSnapshotFeature(context.Context) error
	CreateSnapshot(context.Context, string, string) (*qumulo.Snapshot, error)
	FindSnapshot(context.Context, string, string) (*qumulo.Snapshot, error)
	GetSnapshot(context.Context, string) (*qumulo.Snapshot, error)
	DeleteSnapshot(context.Context, string) error
	HasDriverSnapshots(context.Context, string) (bool, error)
	CopySnapshotTree(context.Context, string, string, string, string) error
}

type qumuloBackend struct {
	conn *qumulo.Connection
}

func (b *qumuloBackend) EnsureVersion(ctx context.Context, floor string) (string, error) {
	return b.conn.EnsureVersion(ctx, floor)
}

func (b *qumuloBackend) EnsureDirectory(ctx context.Context, fsPath, mode string) (*qumulo.FileAttributes, error) {
	return b.conn.EnsureDirectory(ctx, fsPath, mode)
}

// CheckVolume enforces CSI's name-idempotency rule before creating a
// directory or quota. A request cannot reuse a name for another protocol,
// export prefix, or backing directory.
func (b *qumuloBackend) CheckVolume(ctx context.Context, opts volumeOptions) error {
	description, err := managedDescription(opts)
	if err != nil {
		return err
	}
	found := false
	exports, err := b.conn.ListNFSExports(ctx)
	if err != nil {
		return err
	}
	for _, export := range exports {
		managedName := managedVolumeName(export.Description)
		if managedName != opts.ResourceName && export.ExportPath != opts.NFSExportPath && export.FSPath != opts.FSPath {
			continue
		}
		if opts.Protocol != protocolNFS || export.ExportPath != opts.NFSExportPath || export.FSPath != opts.FSPath || export.Description != description {
			return fmt.Errorf("%w: NFS export %q targets %q", errVolumeConflict, export.ExportPath, export.FSPath)
		}
		found = true
	}

	shares, err := b.conn.ListSMBShares(ctx, false)
	if err != nil {
		return err
	}
	for _, share := range shares {
		managedName := managedVolumeName(share.Description)
		if managedName != opts.ResourceName && share.ShareName != opts.ResourceName && share.FSPath != opts.FSPath {
			continue
		}
		if opts.Protocol != protocolSMB || share.ShareName != opts.ResourceName || share.FSPath != opts.FSPath || share.Description != description {
			return fmt.Errorf("%w: SMB share %q targets %q", errVolumeConflict, share.ShareName, share.FSPath)
		}
		found = true
	}
	if !found {
		return nil
	}

	// An existing share/export makes this a retry. Its immutable marker includes
	// the original capacity range, while its backing object must still be the
	// same directory. A missing/undersized quota can be repaired after a partial
	// create; a later ControllerExpandVolume does not change the marker.
	attrs, err := b.conn.FileAttributes(ctx, opts.FSPath)
	if err != nil {
		return fmt.Errorf("%w: existing volume backing directory is unavailable: %v", errVolumeConflict, err)
	}
	if attrs == nil || attrs.ID == "" {
		return fmt.Errorf("%w: existing volume backing directory has no stable identity", errVolumeConflict)
	}
	return nil
}

func (b *qumuloBackend) EnsureQuota(ctx context.Context, fileID string, limit int64) (int64, error) {
	return b.conn.EnsureQuotaAtLeast(ctx, fileID, limit)
}

func (b *qumuloBackend) EnsureNFS(ctx context.Context, opts volumeOptions) (storageResource, error) {
	description, err := managedDescription(opts)
	if err != nil {
		return storageResource{}, err
	}
	hosts := append([]string(nil), opts.AllowedNetworks...)
	if opts.AllowAllHosts {
		hosts = []string{}
	}
	desired := qumulo.NFSExportRequest{
		ExportPath:  opts.NFSExportPath,
		FSPath:      opts.FSPath,
		Description: description,
		Restrictions: []qumulo.NFSRestriction{{
			HostRestrictions:           hosts,
			RequirePrivilegedPort:      opts.NFSRequirePrivileged,
			ReadOnly:                   false,
			RequiredAuthenticationMode: qumulo.NFSAuthNone,
			UserMapping:                qumulo.NFSMapNone,
		}},
	}
	if opts.NFSRootSquash {
		desired.Restrictions[0].UserMapping = qumulo.NFSMapRoot
		desired.Restrictions[0].MapToUser = &qumulo.NFSIdentity{IDType: qumulo.NFSIdentityUID, IDValue: opts.NFSAnonymousUID}
		desired.Restrictions[0].MapToGroup = &qumulo.NFSIdentity{IDType: qumulo.NFSIdentityGID, IDValue: opts.NFSAnonymousGID}
	}
	export, _, err := b.conn.ClaimNFSExport(ctx, desired, false)
	if err != nil {
		if errors.Is(err, qumulo.ErrResourceClaimConflict) {
			return storageResource{}, fmt.Errorf("%w: %v", errVolumeConflict, err)
		}
		return storageResource{}, err
	}
	return storageResource{ID: export.ID, Name: export.ExportPath, Path: export.FSPath}, nil
}

func (b *qumuloBackend) EnsureSMB(ctx context.Context, opts volumeOptions) (storageResource, error) {
	description, err := managedDescription(opts)
	if err != nil {
		return storageResource{}, err
	}
	// Read+Write only: CHANGE_PERMISSIONS would let any pod holding the
	// share credentials rewrite the share's security descriptor.
	shareRights := []qumulo.SMBRight{qumulo.SMBRightRead, qumulo.SMBRightWrite}
	// Always send an explicit network-permission entry. An empty address
	// range means "all addresses" under Qumulo's model, while omitting the
	// field entirely leaves the outcome to server defaults.
	networkPermissions := []qumulo.SMBNetworkPermission{{
		Type:          qumulo.SMBPermissionAllowed,
		AddressRanges: []string{},
		Rights:        append([]qumulo.SMBRight(nil), shareRights...),
	}}
	if !opts.AllowAllHosts {
		networkPermissions[0].AddressRanges = append([]string(nil), opts.AllowedNetworks...)
	}
	desired := qumulo.SMBShareRequest{
		ShareName:   opts.ResourceName,
		FSPath:      opts.FSPath,
		Description: description,
		Permissions: []qumulo.SMBSharePermission{{
			Type:    qumulo.SMBPermissionAllowed,
			Trustee: opts.SMBTrustee,
			Rights:  shareRights,
		}},
		NetworkPermissions:            networkPermissions,
		AccessBasedEnumerationEnabled: opts.SMBAccessBasedEnum,
		DefaultFileCreateMode:         "0666",
		DefaultDirectoryCreateMode:    opts.DirectoryMode,
		BytesPerSector:                "512",
		RequireEncryption:             opts.SMBRequireEncryption,
	}
	share, _, err := b.conn.ClaimSMBShare(ctx, desired, false)
	if err != nil {
		if errors.Is(err, qumulo.ErrResourceClaimConflict) {
			return storageResource{}, fmt.Errorf("%w: %v", errVolumeConflict, err)
		}
		return storageResource{}, err
	}
	return storageResource{ID: share.ID, Name: share.ShareName, Path: share.FSPath}, nil
}

// ValidateVolume proves that both immutable parts named by a signed CSI
// handle still exist: the backing directory and its NFS export or SMB share.
// Looking up by the durable resource ID and then comparing every identity
// field prevents a deleted-and-recreated object from satisfying a stale
// handle merely because it reused the same path or display name.
func (b *qumuloBackend) ValidateVolume(ctx context.Context, h volumeHandle) error {
	attrs, err := b.conn.FileAttributes(ctx, h.FSPath)
	if err != nil {
		return err
	}
	if attrs == nil || attrs.ID != h.DirectoryID {
		return fmt.Errorf("%w: backing directory identity changed", errVolumeNotFound)
	}

	expectedDescription := managedDescriptionForHandle(h)
	switch h.Protocol {
	case protocolNFS:
		export, err := b.conn.GetNFSExport(ctx, h.ResourceID)
		if err != nil {
			return err
		}
		if export == nil || export.ID != h.ResourceID || export.ExportPath != h.ResourceName || export.FSPath != h.FSPath {
			return fmt.Errorf("%w: NFS export identity changed", errVolumeNotFound)
		}
		if export.Description == volumeDeletionDescription(h) {
			return fmt.Errorf("%w: NFS export deletion is in progress", errVolumeNotFound)
		}
		if export.Description != expectedDescription {
			return fmt.Errorf("%w: NFS export ownership marker changed", errVolumeNotFound)
		}
	case protocolSMB:
		share, err := b.conn.GetSMBShare(ctx, h.ResourceID)
		if err != nil {
			return err
		}
		if share == nil || share.ID != h.ResourceID || share.ShareName != h.ResourceName || share.FSPath != h.FSPath {
			return fmt.Errorf("%w: SMB share identity changed", errVolumeNotFound)
		}
		if share.Description == volumeDeletionDescription(h) {
			return fmt.Errorf("%w: SMB share deletion is in progress", errVolumeNotFound)
		}
		if share.Description != expectedDescription {
			return fmt.Errorf("%w: SMB share ownership marker changed", errVolumeNotFound)
		}
	default:
		return fmt.Errorf("%w: unsupported volume protocol", errVolumeNotFound)
	}
	return nil
}

// PrepareVolumeDeletion atomically turns the exact signed export/share into
// a durable deletion claim. Keeping that claim in Core until tree-delete has
// completed prevents a concurrent same-name CreateVolume from reusing the
// still-existing directory and having it removed by the older DeleteVolume.
func (b *qumuloBackend) PrepareVolumeDeletion(ctx context.Context, h volumeHandle) error {
	description := volumeDeletionDescription(h)
	managedDescription := managedDescriptionForHandle(h)
	for attempt := 0; attempt < 3; attempt++ {
		switch h.Protocol {
		case protocolNFS:
			export, err := b.conn.GetNFSExport(ctx, h.ResourceID)
			if err != nil {
				return deletionClaimReadError(err, "NFS export")
			}
			if export == nil || export.ID != h.ResourceID || export.ExportPath != h.ResourceName || export.FSPath != h.FSPath {
				return fmt.Errorf("%w: refusing to claim a renamed or retargeted NFS export", errVolumeIdentityChanged)
			}
			if export.Description == description {
				return nil
			}
			if export.Description != managedDescription {
				return fmt.Errorf("%w: NFS export ownership marker changed; refusing to overwrite it", errVolumeIdentityChanged)
			}
			_, err = b.conn.PatchNFSExport(ctx, h.ResourceID, qumulo.PatchNFSExportRequest{Description: &description}, qumulo.NFSExportWriteOptions{IfMatch: export.ETag})
			if retry, failed := classifyDeletionClaimWriteError(err); failed != nil {
				return failed
			} else if retry {
				continue
			}
			return nil
		case protocolSMB:
			share, err := b.conn.GetSMBShare(ctx, h.ResourceID)
			if err != nil {
				return deletionClaimReadError(err, "SMB share")
			}
			if share == nil || share.ID != h.ResourceID || share.ShareName != h.ResourceName || share.FSPath != h.FSPath {
				return fmt.Errorf("%w: refusing to claim a renamed or retargeted SMB share", errVolumeIdentityChanged)
			}
			if share.Description == description {
				return nil
			}
			if share.Description != managedDescription {
				return fmt.Errorf("%w: SMB share ownership marker changed; refusing to overwrite it", errVolumeIdentityChanged)
			}
			_, err = b.conn.PatchSMBShare(ctx, h.ResourceID, qumulo.PatchSMBShareRequest{Description: &description}, qumulo.SMBShareWriteOptions{IfMatch: share.ETag})
			if retry, failed := classifyDeletionClaimWriteError(err); failed != nil {
				return failed
			} else if retry {
				continue
			}
			return nil
		default:
			return fmt.Errorf("%w: unsupported volume protocol", errVolumeIdentityChanged)
		}
	}
	return fmt.Errorf("prepare volume deletion: %w during concurrent modification", errVolumeIdentityChanged)
}

func deletionClaimReadError(err error, resource string) error {
	if api, ok := qumulo.AsAPIError(err); ok && api.IsNotFound() {
		return fmt.Errorf("%w: %s", errVolumeResourceMissing, resource)
	}
	return err
}

func classifyDeletionClaimWriteError(err error) (retry bool, failed error) {
	if err == nil {
		return false, nil
	}
	api, ok := qumulo.AsAPIError(err)
	if !ok {
		return false, err
	}
	if api.StatusCode == http.StatusPreconditionFailed {
		return true, nil
	}
	if api.IsNotFound() {
		return false, fmt.Errorf("%w: volume resource disappeared while claiming deletion", errVolumeIdentityChanged)
	}
	return false, err
}

const volumeDeletionDescriptionPrefix = "Deletion in progress by " + DefaultDriverName + "; token="

func volumeDeletionDescription(h volumeHandle) string {
	sum := sha256.Sum256([]byte(string(h.Protocol) + "\x00" + h.ResourceID + "\x00" + h.DirectoryID + "\x00" + h.FSPath + "\x00" + h.SpecFingerprint))
	return volumeDeletionDescriptionPrefix + hex.EncodeToString(sum[:16])
}

func managedDescriptionForHandle(h volumeHandle) string {
	return fmt.Sprintf("Managed by %s; volume=%s; spec=%s", DefaultDriverName, path.Base(h.FSPath), h.SpecFingerprint)
}

// DeleteVolumeResource conditionally removes only the exact export/share
// captured in the signed handle. A missing resource is success so a retry can
// continue a prior data deletion, while a renamed or retargeted resource is
// never deleted through a stale handle.
func (b *qumuloBackend) DeleteVolumeResource(ctx context.Context, h volumeHandle, requireDeletionClaim bool) error {
	expectedDescription := managedDescriptionForHandle(h)
	if requireDeletionClaim {
		expectedDescription = volumeDeletionDescription(h)
	}
	for attempt := 0; attempt < 3; attempt++ {
		var etag string
		switch h.Protocol {
		case protocolNFS:
			export, err := b.conn.GetNFSExport(ctx, h.ResourceID)
			if err != nil {
				if api, ok := qumulo.AsAPIError(err); ok && api.IsNotFound() {
					return nil
				}
				return err
			}
			if export == nil || export.ID != h.ResourceID || export.ExportPath != h.ResourceName || export.FSPath != h.FSPath {
				return fmt.Errorf("%w: refusing to delete a renamed or retargeted NFS export", errVolumeIdentityChanged)
			}
			if export.Description != expectedDescription {
				return fmt.Errorf("%w: refusing to delete an NFS export whose ownership marker changed", errVolumeIdentityChanged)
			}
			etag = export.ETag
			if err := b.conn.DeleteNFSExport(ctx, h.ResourceID, etag); err != nil {
				if retry, done := classifyConditionalDeleteError(err); done {
					return nil
				} else if retry {
					continue
				}
				return err
			}
		case protocolSMB:
			share, err := b.conn.GetSMBShare(ctx, h.ResourceID)
			if err != nil {
				if api, ok := qumulo.AsAPIError(err); ok && api.IsNotFound() {
					return nil
				}
				return err
			}
			if share == nil || share.ID != h.ResourceID || share.ShareName != h.ResourceName || share.FSPath != h.FSPath {
				return fmt.Errorf("%w: refusing to delete a renamed or retargeted SMB share", errVolumeIdentityChanged)
			}
			if share.Description != expectedDescription {
				return fmt.Errorf("%w: refusing to delete an SMB share whose ownership marker changed", errVolumeIdentityChanged)
			}
			etag = share.ETag
			if err := b.conn.DeleteSMBShare(ctx, h.ResourceID, etag); err != nil {
				if retry, done := classifyConditionalDeleteError(err); done {
					return nil
				} else if retry {
					continue
				}
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported volume protocol", errVolumeIdentityChanged)
		}
		return nil
	}
	return fmt.Errorf("delete volume resource: %w during concurrent modification", errVolumeIdentityChanged)
}

func classifyConditionalDeleteError(err error) (retry, done bool) {
	api, ok := qumulo.AsAPIError(err)
	if !ok {
		return false, false
	}
	if api.IsNotFound() {
		return false, true
	}
	return api.StatusCode == http.StatusPreconditionFailed, false
}

func (b *qumuloBackend) FileAttributes(ctx context.Context, fsPath string) (*qumulo.FileAttributes, error) {
	return b.conn.FileAttributes(ctx, fsPath)
}

func (b *qumuloBackend) TreeDelete(ctx context.Context, fileID string) error {
	return b.conn.TreeDelete(ctx, fileID)
}

func (b *qumuloBackend) EnsureSnapshotFeature(ctx context.Context) error {
	version, err := b.conn.EnsureVersion(ctx, "")
	if err != nil {
		return err
	}
	ok, min, err := qumulo.Supports(version, qumulo.FeatureSnapshots)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w (requires Core %s+)", errSnapshotsUnsupported, min)
	}
	return nil
}

func (b *qumuloBackend) CreateSnapshot(ctx context.Context, directoryID, suffix string) (*qumulo.Snapshot, error) {
	return b.conn.CreateSnapshot(ctx, directoryID, suffix)
}

func (b *qumuloBackend) FindSnapshot(ctx context.Context, directoryID, suffix string) (*qumulo.Snapshot, error) {
	return b.conn.FindSnapshotBySuffix(ctx, directoryID, suffix)
}

func (b *qumuloBackend) GetSnapshot(ctx context.Context, id string) (*qumulo.Snapshot, error) {
	return b.conn.GetSnapshot(ctx, id)
}

func (b *qumuloBackend) DeleteSnapshot(ctx context.Context, id string) error {
	return b.conn.DeleteSnapshot(ctx, id)
}

func (b *qumuloBackend) HasDriverSnapshots(ctx context.Context, directoryID string) (bool, error) {
	return b.conn.HasDriverSnapshots(ctx, directoryID)
}

func (b *qumuloBackend) CopySnapshotTree(ctx context.Context, sourcePath, sourceDirID, snapshotID, destPath string) error {
	return b.conn.CopySnapshotTree(ctx, sourcePath, sourceDirID, snapshotID, destPath)
}

type immutableVolumeSpec struct {
	RequestName          string          `json:"requestName"`
	Parameters           []string        `json:"parameters,omitempty"`
	Protocol             protocol        `json:"protocol"`
	Endpoint             string          `json:"endpoint"`
	Server               string          `json:"server"`
	RESTPort             string          `json:"restPort"`
	FSPath               string          `json:"fsPath"`
	ResourceName         string          `json:"resourceName"`
	DirectoryMode        string          `json:"directoryMode"`
	AllowedNetworks      []string        `json:"allowedNetworks"`
	AllowAllHosts        bool            `json:"allowAllHosts"`
	QuotaEnabled         bool            `json:"quotaEnabled"`
	DeleteData           bool            `json:"deleteData"`
	NFSExportPath        string          `json:"nfsExportPath,omitempty"`
	NFSRequirePrivileged bool            `json:"nfsRequirePrivileged,omitempty"`
	NFSVersion           string          `json:"nfsVersion,omitempty"`
	NFSRootSquash        bool            `json:"nfsRootSquash,omitempty"`
	NFSAnonymousUID      string          `json:"nfsAnonymousUID,omitempty"`
	NFSAnonymousGID      string          `json:"nfsAnonymousGID,omitempty"`
	SMBRequireEncryption bool            `json:"smbRequireEncryption,omitempty"`
	SMBAccessBasedEnum   bool            `json:"smbAccessBasedEnumeration,omitempty"`
	SMBAllowAllUsers     bool            `json:"smbAllowAllUsers,omitempty"`
	SMBTrustee           qumulo.Identity `json:"smbTrustee,omitempty"`
	InitialCapacity      int64           `json:"initialCapacity"`
	InitialCapacityLimit int64           `json:"initialCapacityLimit,omitempty"`
}

func volumeSpecFingerprint(opts volumeOptions) (string, error) {
	networks := append([]string(nil), opts.AllowedNetworks...)
	sort.Strings(networks)
	spec := immutableVolumeSpec{
		RequestName: opts.RequestName, Parameters: canonicalMapEntries(opts.Parameters),
		Protocol: opts.Protocol, Endpoint: strings.ToLower(endpointHost(opts.Endpoint)), Server: strings.ToLower(strings.Trim(opts.Server, "[]")),
		RESTPort: opts.RESTPort, FSPath: opts.FSPath, ResourceName: opts.ResourceName, DirectoryMode: opts.DirectoryMode,
		AllowedNetworks: networks, AllowAllHosts: opts.AllowAllHosts, QuotaEnabled: opts.QuotaEnabled, DeleteData: opts.DeleteData,
		InitialCapacity: opts.RequestedCapacity, InitialCapacityLimit: opts.CapacityLimit,
	}
	if opts.Protocol == protocolNFS {
		spec.NFSExportPath, spec.NFSRequirePrivileged, spec.NFSVersion = opts.NFSExportPath, opts.NFSRequirePrivileged, opts.NFSVersion
		spec.NFSRootSquash, spec.NFSAnonymousUID, spec.NFSAnonymousGID = opts.NFSRootSquash, opts.NFSAnonymousUID, opts.NFSAnonymousGID
	} else {
		spec.SMBRequireEncryption, spec.SMBAccessBasedEnum = opts.SMBRequireEncryption, opts.SMBAccessBasedEnum
		spec.SMBAllowAllUsers, spec.SMBTrustee = opts.SMBAllowAllUsers, opts.SMBTrustee
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode immutable volume specification: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func managedDescription(opts volumeOptions) (string, error) {
	fingerprint, err := volumeSpecFingerprint(opts)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Managed by %s; volume=%s; spec=%s", DefaultDriverName, opts.ResourceName, fingerprint), nil
}

func managedVolumeName(description string) string {
	prefix := "Managed by " + DefaultDriverName + "; volume="
	if !strings.HasPrefix(description, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(description, prefix)
	name, _, _ := strings.Cut(rest, ";")
	return name
}
