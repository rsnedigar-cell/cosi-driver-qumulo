package qumulo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

const (
	smbSharesPath          = "/v2/smb/shares/"
	smbSharesV3PreviewPath = "/v3/smb/shares/"
)

// SMBPermissionType determines whether an SMB share permission grants or
// denies its rights.
type SMBPermissionType string

const (
	SMBPermissionAllowed SMBPermissionType = "ALLOWED"
	SMBPermissionDenied  SMBPermissionType = "DENIED"
)

// SMBRight is a right in an SMB share ACL or network ACL.
type SMBRight string

const (
	SMBRightRead              SMBRight = "READ"
	SMBRightWrite             SMBRight = "WRITE"
	SMBRightChangePermissions SMBRight = "CHANGE_PERMISSIONS"
	SMBRightAll               SMBRight = "ALL"
	SMBRightReadData          SMBRight = "READ_DATA"
	SMBRightReadEA            SMBRight = "READ_EA"
	SMBRightReadAttributes    SMBRight = "READ_ATTR"
	SMBRightReadACL           SMBRight = "READ_ACL"
	SMBRightWriteEA           SMBRight = "WRITE_EA"
	SMBRightWriteAttributes   SMBRight = "WRITE_ATTR"
	SMBRightWriteACL          SMBRight = "WRITE_ACL"
	SMBRightChangeOwner       SMBRight = "CHANGE_OWNER"
	SMBRightWriteGroup        SMBRight = "WRITE_GROUP"
	SMBRightDelete            SMBRight = "DELETE"
	SMBRightExecute           SMBRight = "EXECUTE"
	SMBRightModify            SMBRight = "MODIFY"
	SMBRightExtend            SMBRight = "EXTEND"
	SMBRightAddFile           SMBRight = "ADD_FILE"
	SMBRightAddSubdirectory   SMBRight = "ADD_SUBDIR"
	SMBRightDeleteChild       SMBRight = "DELETE_CHILD"
	SMBRightSynchronize       SMBRight = "SYNCHRONIZE"
)

// SMBOfflineFilesCachingMode controls the client-side offline-files mode
// advertised by the v3 SMB API.
type SMBOfflineFilesCachingMode string

const (
	SMBOfflineFilesNoCaching        SMBOfflineFilesCachingMode = "NO_CACHING"
	SMBOfflineFilesManualCaching    SMBOfflineFilesCachingMode = "MANUAL_CACHING"
	SMBOfflineFilesAutomaticCaching SMBOfflineFilesCachingMode = "AUTOMATIC_CACHING"
)

// SMBSharePermission is an identity-based SMB share ACL entry.
type SMBSharePermission struct {
	Type    SMBPermissionType `json:"type"`
	Trustee Identity          `json:"trustee"`
	Rights  []SMBRight        `json:"rights"`
}

// SMBNetworkPermission is an address-based SMB share ACL entry. An empty
// AddressRanges slice applies the entry to all client addresses.
type SMBNetworkPermission struct {
	Type          SMBPermissionType `json:"type"`
	AddressRanges []string          `json:"address_ranges"`
	Rights        []SMBRight        `json:"rights"`
}

// SMBShare is the representation returned by Qumulo Core. ETag is response
// metadata used for optimistic concurrency and is never serialized.
type SMBShare struct {
	ID                            string                     `json:"id"`
	ShareName                     string                     `json:"share_name"`
	TenantID                      int64                      `json:"tenant_id,omitempty"`
	FSPath                        string                     `json:"fs_path"`
	Description                   string                     `json:"description"`
	Permissions                   []SMBSharePermission       `json:"permissions"`
	NetworkPermissions            []SMBNetworkPermission     `json:"network_permissions"`
	AccessBasedEnumerationEnabled bool                       `json:"access_based_enumeration_enabled"`
	DefaultFileCreateMode         string                     `json:"default_file_create_mode"`
	DefaultDirectoryCreateMode    string                     `json:"default_directory_create_mode"`
	BytesPerSector                string                     `json:"bytes_per_sector"`
	RequireEncryption             bool                       `json:"require_encryption"`
	AllowFSPathCreate             bool                       `json:"allow_fs_path_create,omitempty"`
	ExpandFSPathVariables         bool                       `json:"expand_fs_path_variables,omitempty"`
	OfflineFilesCachingMode       SMBOfflineFilesCachingMode `json:"offline_files_caching_mode,omitempty"`
	ETag                          string                     `json:"-"`
}

// SMBShareRequest contains the writable fields used to create or fully
// replace an SMB share.
type SMBShareRequest struct {
	ShareName                     string                     `json:"share_name"`
	TenantID                      int64                      `json:"tenant_id,omitempty"`
	FSPath                        string                     `json:"fs_path"`
	Description                   string                     `json:"description"`
	Permissions                   []SMBSharePermission       `json:"permissions"`
	NetworkPermissions            []SMBNetworkPermission     `json:"network_permissions"`
	AccessBasedEnumerationEnabled bool                       `json:"access_based_enumeration_enabled"`
	DefaultFileCreateMode         string                     `json:"default_file_create_mode,omitempty"`
	DefaultDirectoryCreateMode    string                     `json:"default_directory_create_mode,omitempty"`
	BytesPerSector                string                     `json:"bytes_per_sector,omitempty"`
	RequireEncryption             bool                       `json:"require_encryption"`
	ExpandFSPathVariables         bool                       `json:"expand_fs_path_variables,omitempty"`
	OfflineFilesCachingMode       SMBOfflineFilesCachingMode `json:"offline_files_caching_mode,omitempty"`
}

// PatchSMBShareRequest contains only fields that should be modified. A
// pointer to an empty slice explicitly clears the corresponding ACL.
type PatchSMBShareRequest struct {
	ShareName                     *string                     `json:"share_name,omitempty"`
	TenantID                      *int64                      `json:"tenant_id,omitempty"`
	FSPath                        *string                     `json:"fs_path,omitempty"`
	Description                   *string                     `json:"description,omitempty"`
	Permissions                   *[]SMBSharePermission       `json:"permissions,omitempty"`
	NetworkPermissions            *[]SMBNetworkPermission     `json:"network_permissions,omitempty"`
	AccessBasedEnumerationEnabled *bool                       `json:"access_based_enumeration_enabled,omitempty"`
	DefaultFileCreateMode         *string                     `json:"default_file_create_mode,omitempty"`
	DefaultDirectoryCreateMode    *string                     `json:"default_directory_create_mode,omitempty"`
	BytesPerSector                *string                     `json:"bytes_per_sector,omitempty"`
	RequireEncryption             *bool                       `json:"require_encryption,omitempty"`
	AllowFSPathCreate             *bool                       `json:"allow_fs_path_create,omitempty"`
	ExpandFSPathVariables         *bool                       `json:"expand_fs_path_variables,omitempty"`
	OfflineFilesCachingMode       *SMBOfflineFilesCachingMode `json:"offline_files_caching_mode,omitempty"`
}

// SMBShareWriteOptions controls filesystem-path creation and optimistic
// concurrency for writes.
type SMBShareWriteOptions struct {
	AllowFSPathCreate bool
	IfMatch           string
}

// CreateSMBShare creates a share. It uses Qumulo's stable v2 API by default
// and the tenant-aware v3 preview API only when the request needs v3 fields.
func (c *Connection) CreateSMBShare(ctx context.Context, req SMBShareRequest, allowFSPathCreate bool) (*SMBShare, error) {
	if err := validateSMBShareRequest(req); err != nil {
		return nil, err
	}
	req = canonicalSMBShareRequest(req)
	if usesSMBV3Preview(req) {
		return c.CreateSMBShareV3Preview(ctx, req, allowFSPathCreate)
	}
	var out SMBShare
	h, err := c.DoJSON(ctx, http.MethodPost, smbSharesPath, allowFSPathCreateQuery(allowFSPathCreate), nil, req, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// ListSMBShares lists every SMB share. populateTrusteeNames asks Core to
// resolve trustee names in addition to returning their stable identifiers.
func (c *Connection) ListSMBShares(ctx context.Context, populateTrusteeNames bool) ([]SMBShare, error) {
	var query url.Values
	if populateTrusteeNames {
		query = url.Values{"populate-trustee-names": []string{"true"}}
	}
	var out []SMBShare
	_, err := c.DoJSON(ctx, http.MethodGet, smbSharesPath, query, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetSMBShare retrieves a share by ID or share name.
func (c *Connection) GetSMBShare(ctx context.Context, ref string) (*SMBShare, error) {
	pth, err := smbShareRefPath(ref)
	if err != nil {
		return nil, err
	}
	var out SMBShare
	h, err := c.DoJSON(ctx, http.MethodGet, pth, nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// ReplaceSMBShare sets all writable attributes of a share.
func (c *Connection) ReplaceSMBShare(ctx context.Context, ref string, req SMBShareRequest, opts SMBShareWriteOptions) (*SMBShare, error) {
	if err := validateSMBShareRequest(req); err != nil {
		return nil, err
	}
	req = canonicalSMBShareRequest(req)
	if usesSMBV3Preview(req) {
		return c.ReplaceSMBShareV3Preview(ctx, ref, req, opts)
	}
	pth, err := smbShareRefPath(ref)
	if err != nil {
		return nil, err
	}
	var out SMBShare
	h, err := c.DoJSON(ctx, http.MethodPut, pth, allowFSPathCreateQuery(opts.AllowFSPathCreate), ifMatchHeader(opts.IfMatch), req, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// PatchSMBShare modifies selected attributes of a share.
func (c *Connection) PatchSMBShare(ctx context.Context, ref string, req PatchSMBShareRequest, opts SMBShareWriteOptions) (*SMBShare, error) {
	req = canonicalPatchSMBShareRequest(req)
	if req.TenantID != nil || req.AllowFSPathCreate != nil || req.ExpandFSPathVariables != nil || req.OfflineFilesCachingMode != nil {
		return c.PatchSMBShareV3Preview(ctx, ref, req, opts)
	}
	pth, err := smbShareRefPath(ref)
	if err != nil {
		return nil, err
	}
	var out SMBShare
	h, err := c.DoJSON(ctx, http.MethodPatch, pth, allowFSPathCreateQuery(opts.AllowFSPathCreate), ifMatchHeader(opts.IfMatch), req, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// DeleteSMBShare deletes a share by ID or share name. An optional ETag
// prevents deletion when the share changed after it was read.
func (c *Connection) DeleteSMBShare(ctx context.Context, ref, ifMatch string) error {
	pth, err := smbShareRefPath(ref)
	if err != nil {
		return err
	}
	_, err = c.DoJSON(ctx, http.MethodDelete, pth, nil, ifMatchHeader(ifMatch), nil, nil)
	return err
}

// DeleteSMBShareIfExists performs an idempotent, conditional delete. It
// returns true only when this call observed and deleted a share.
func (c *Connection) DeleteSMBShareIfExists(ctx context.Context, ref string) (bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		current, err := c.GetSMBShare(ctx, ref)
		if err != nil {
			if isAPINotFound(err) {
				return false, nil
			}
			return false, err
		}
		target := current.IDOrName()
		if err := c.DeleteSMBShare(ctx, target, current.ETag); err != nil {
			if isAPINotFound(err) {
				return false, nil
			}
			if isAPIPreconditionFailed(err) {
				continue
			}
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("delete SMB share %q: concurrent modifications did not settle", ref)
}

// EnsureSMBShare creates the share or reconciles its writable settings. It
// never retargets an existing share to a different filesystem path. The
// boolean result reports whether this call definitely created the share.
func (c *Connection) EnsureSMBShare(ctx context.Context, desired SMBShareRequest, allowFSPathCreate bool) (*SMBShare, bool, error) {
	return c.ensureSMBShare(ctx, desired, allowFSPathCreate, false)
}

// ClaimSMBShare is the strict create-or-reconcile variant used by CSI. A
// same-name share must carry the exact immutable specification marker. The
// check runs after create conflicts and ETag retries so concurrent,
// incompatible CreateVolume calls cannot patch each other's shares.
func (c *Connection) ClaimSMBShare(ctx context.Context, desired SMBShareRequest, allowFSPathCreate bool) (*SMBShare, bool, error) {
	return c.ensureSMBShare(ctx, desired, allowFSPathCreate, true)
}

func (c *Connection) ensureSMBShare(ctx context.Context, desired SMBShareRequest, allowFSPathCreate, strictDescription bool) (*SMBShare, bool, error) {
	if err := validateSMBShareRequest(desired); err != nil {
		return nil, false, err
	}
	desired = canonicalSMBShareRequest(desired)
	if usesSMBV3Preview(desired) {
		return c.ensureSMBShareV3Preview(ctx, desired, allowFSPathCreate)
	}
	current, err := c.GetSMBShare(ctx, desired.ShareName)
	if err != nil && !isAPINotFound(err) {
		return nil, false, err
	}
	if isAPINotFound(err) {
		created, createErr := c.CreateSMBShare(ctx, desired, allowFSPathCreate)
		if createErr == nil {
			return created, true, nil
		}
		if isAPIAlreadyExists(createErr) {
			current = nil
		} else if isAmbiguousCreateError(createErr) {
			current, err = c.GetSMBShare(ctx, desired.ShareName)
			if err != nil {
				if isAPINotFound(err) {
					return nil, false, createErr
				}
				return nil, false, fmt.Errorf("reconcile ambiguous SMB share create: %w (create error: %v)", err, createErr)
			}
		} else {
			return nil, false, createErr
		}
	}

	for attempt := 0; attempt < 3; attempt++ {
		if current == nil {
			current, err = c.GetSMBShare(ctx, desired.ShareName)
			if err != nil {
				return nil, false, err
			}
		}
		if strictDescription && current.Description != desired.Description {
			return nil, false, fmt.Errorf("%w: SMB share %q has specification marker %q, requested %q", ErrResourceClaimConflict, desired.ShareName, current.Description, desired.Description)
		}
		if current.FSPath != desired.FSPath {
			return nil, false, fmt.Errorf("SMB share %q already targets %q, refusing to retarget it to %q", desired.ShareName, current.FSPath, desired.FSPath)
		}
		patch, changed := smbSharePatch(*current, desired)
		if !changed {
			return current, false, nil
		}
		updated, patchErr := c.PatchSMBShare(ctx, current.IDOrName(), patch, SMBShareWriteOptions{
			AllowFSPathCreate: allowFSPathCreate,
			IfMatch:           current.ETag,
		})
		if patchErr == nil {
			return updated, false, nil
		}
		if !isAPIPreconditionFailed(patchErr) {
			return nil, false, patchErr
		}
		current = nil
	}
	return nil, false, fmt.Errorf("ensure SMB share %q: concurrent modifications did not settle", desired.ShareName)
}

// IDOrName returns the stable ID when Core supplied one, otherwise the share
// name. It is useful as the reference for conditional writes.
func (s SMBShare) IDOrName() string {
	if s.ID != "" {
		return s.ID
	}
	return s.ShareName
}

type smbShareV3WriteRequest struct {
	ShareName                     string                     `json:"share_name"`
	TenantID                      int64                      `json:"tenant_id,omitempty"`
	FSPath                        string                     `json:"fs_path"`
	Description                   string                     `json:"description"`
	Permissions                   []SMBSharePermission       `json:"permissions"`
	NetworkPermissions            []SMBNetworkPermission     `json:"network_permissions"`
	AccessBasedEnumerationEnabled bool                       `json:"access_based_enumeration_enabled"`
	DefaultFileCreateMode         string                     `json:"default_file_create_mode,omitempty"`
	DefaultDirectoryCreateMode    string                     `json:"default_directory_create_mode,omitempty"`
	RequireEncryption             bool                       `json:"require_encryption"`
	AllowFSPathCreate             bool                       `json:"allow_fs_path_create"`
	ExpandFSPathVariables         bool                       `json:"expand_fs_path_variables"`
	OfflineFilesCachingMode       SMBOfflineFilesCachingMode `json:"offline_files_caching_mode,omitempty"`
}

type patchSMBShareV3Request struct {
	ShareName                     *string                     `json:"share_name,omitempty"`
	TenantID                      *int64                      `json:"tenant_id,omitempty"`
	FSPath                        *string                     `json:"fs_path,omitempty"`
	Description                   *string                     `json:"description,omitempty"`
	Permissions                   *[]SMBSharePermission       `json:"permissions,omitempty"`
	NetworkPermissions            *[]SMBNetworkPermission     `json:"network_permissions,omitempty"`
	AccessBasedEnumerationEnabled *bool                       `json:"access_based_enumeration_enabled,omitempty"`
	DefaultFileCreateMode         *string                     `json:"default_file_create_mode,omitempty"`
	DefaultDirectoryCreateMode    *string                     `json:"default_directory_create_mode,omitempty"`
	RequireEncryption             *bool                       `json:"require_encryption,omitempty"`
	AllowFSPathCreate             *bool                       `json:"allow_fs_path_create,omitempty"`
	ExpandFSPathVariables         *bool                       `json:"expand_fs_path_variables,omitempty"`
	OfflineFilesCachingMode       *SMBOfflineFilesCachingMode `json:"offline_files_caching_mode,omitempty"`
}

// CreateSMBShareV3Preview creates a tenant-aware or v3-featured SMB share.
// Qumulo documents this API as preview, so stable v2 remains the default.
func (c *Connection) CreateSMBShareV3Preview(ctx context.Context, req SMBShareRequest, allowFSPathCreate bool) (*SMBShare, error) {
	if err := validateSMBShareRequest(req); err != nil {
		return nil, err
	}
	req = canonicalSMBShareRequest(req)
	body := smbShareV3Body(req, allowFSPathCreate)
	var out SMBShare
	h, err := c.DoJSON(ctx, http.MethodPost, smbSharesV3PreviewPath, nil, nil, body, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// ListSMBSharesV3Preview lists tenant-aware SMB shares. The v3 preview API
// wraps its list in an entries object rather than returning the v2 bare array.
func (c *Connection) ListSMBSharesV3Preview(ctx context.Context, populateTrusteeNames bool) ([]SMBShare, error) {
	var query url.Values
	if populateTrusteeNames {
		query = url.Values{"populate-trustee-names": []string{"true"}}
	}
	var out struct {
		Entries []SMBShare `json:"entries"`
	}
	_, err := c.DoJSON(ctx, http.MethodGet, smbSharesV3PreviewPath, query, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// GetSMBShareV3Preview retrieves a tenant-aware SMB share by ID.
func (c *Connection) GetSMBShareV3Preview(ctx context.Context, id string) (*SMBShare, error) {
	pth, err := smbShareV3RefPath(id)
	if err != nil {
		return nil, err
	}
	var out SMBShare
	h, err := c.DoJSON(ctx, http.MethodGet, pth, nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// ReplaceSMBShareV3Preview replaces every attribute of a tenant-aware or
// v3-featured SMB share.
func (c *Connection) ReplaceSMBShareV3Preview(ctx context.Context, id string, req SMBShareRequest, opts SMBShareWriteOptions) (*SMBShare, error) {
	if err := validateSMBShareRequest(req); err != nil {
		return nil, err
	}
	req = canonicalSMBShareRequest(req)
	pth, err := smbShareV3RefPath(id)
	if err != nil {
		return nil, err
	}
	body := smbShareV3Body(req, opts.AllowFSPathCreate)
	var out SMBShare
	h, err := c.DoJSON(ctx, http.MethodPut, pth, nil, ifMatchHeader(opts.IfMatch), body, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// PatchSMBShareV3Preview modifies selected fields of a tenant-aware or
// v3-featured SMB share.
func (c *Connection) PatchSMBShareV3Preview(ctx context.Context, id string, req PatchSMBShareRequest, opts SMBShareWriteOptions) (*SMBShare, error) {
	req = canonicalPatchSMBShareRequest(req)
	pth, err := smbShareV3RefPath(id)
	if err != nil {
		return nil, err
	}
	body := patchSMBShareV3Request{
		ShareName:                     req.ShareName,
		TenantID:                      req.TenantID,
		FSPath:                        req.FSPath,
		Description:                   req.Description,
		Permissions:                   req.Permissions,
		NetworkPermissions:            req.NetworkPermissions,
		AccessBasedEnumerationEnabled: req.AccessBasedEnumerationEnabled,
		DefaultFileCreateMode:         req.DefaultFileCreateMode,
		DefaultDirectoryCreateMode:    req.DefaultDirectoryCreateMode,
		RequireEncryption:             req.RequireEncryption,
		AllowFSPathCreate:             req.AllowFSPathCreate,
		ExpandFSPathVariables:         req.ExpandFSPathVariables,
		OfflineFilesCachingMode:       req.OfflineFilesCachingMode,
	}
	if opts.AllowFSPathCreate && body.AllowFSPathCreate == nil {
		body.AllowFSPathCreate = ptr(true)
	}
	var out SMBShare
	h, err := c.DoJSON(ctx, http.MethodPatch, pth, nil, ifMatchHeader(opts.IfMatch), body, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// DeleteSMBShareV3Preview deletes a tenant-aware SMB share by ID.
func (c *Connection) DeleteSMBShareV3Preview(ctx context.Context, id, ifMatch string) error {
	pth, err := smbShareV3RefPath(id)
	if err != nil {
		return err
	}
	_, err = c.DoJSON(ctx, http.MethodDelete, pth, nil, ifMatchHeader(ifMatch), nil, nil)
	return err
}

func (c *Connection) ensureSMBShareV3Preview(ctx context.Context, desired SMBShareRequest, allowFSPathCreate bool) (*SMBShare, bool, error) {
	current, err := c.findSMBShareV3Preview(ctx, desired.TenantID, desired.ShareName)
	if err != nil {
		return nil, false, err
	}
	if current == nil {
		created, createErr := c.CreateSMBShareV3Preview(ctx, desired, allowFSPathCreate)
		if createErr == nil {
			return created, true, nil
		}
		if isAPIAlreadyExists(createErr) {
			current = nil
		} else if isAmbiguousCreateError(createErr) {
			current, err = c.findSMBShareV3Preview(ctx, desired.TenantID, desired.ShareName)
			if err != nil {
				return nil, false, fmt.Errorf("reconcile ambiguous tenant SMB share create: %w (create error: %v)", err, createErr)
			}
			if current == nil {
				return nil, false, createErr
			}
		} else {
			return nil, false, createErr
		}
	}

	for attempt := 0; attempt < 3; attempt++ {
		if current == nil {
			current, err = c.findSMBShareV3Preview(ctx, desired.TenantID, desired.ShareName)
			if err != nil {
				return nil, false, err
			}
			if current == nil {
				continue
			}
		}
		if current.FSPath != desired.FSPath {
			return nil, false, fmt.Errorf("SMB share %q in tenant %d already targets %q, refusing to retarget it to %q", desired.ShareName, desired.TenantID, current.FSPath, desired.FSPath)
		}
		patch, changed := smbShareV3Patch(*current, desired, allowFSPathCreate)
		if !changed {
			return current, false, nil
		}
		updated, patchErr := c.PatchSMBShareV3Preview(ctx, current.ID, patch, SMBShareWriteOptions{IfMatch: current.ETag})
		if patchErr == nil {
			return updated, false, nil
		}
		if !isAPIPreconditionFailed(patchErr) {
			return nil, false, patchErr
		}
		current = nil
	}
	return nil, false, fmt.Errorf("ensure SMB share %q in tenant %d: concurrent creation or modification did not settle", desired.ShareName, desired.TenantID)
}

func (c *Connection) findSMBShareV3Preview(ctx context.Context, tenantID int64, shareName string) (*SMBShare, error) {
	shares, err := c.ListSMBSharesV3Preview(ctx, false)
	if err != nil {
		return nil, err
	}
	var found *SMBShare
	for i := range shares {
		if shares[i].ShareName != shareName || (tenantID != 0 && shares[i].TenantID != tenantID) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple SMB shares named %q found for tenant %d", shareName, tenantID)
		}
		found = &shares[i]
	}
	if found == nil {
		return nil, nil
	}
	if found.ID == "" {
		return nil, fmt.Errorf("SMB v3 list returned share %q without an ID", shareName)
	}
	full, err := c.GetSMBShareV3Preview(ctx, found.ID)
	if isAPINotFound(err) {
		return nil, nil
	}
	return full, err
}

func smbShareV3Body(req SMBShareRequest, allowFSPathCreate bool) smbShareV3WriteRequest {
	return smbShareV3WriteRequest{
		ShareName:                     req.ShareName,
		TenantID:                      req.TenantID,
		FSPath:                        req.FSPath,
		Description:                   req.Description,
		Permissions:                   req.Permissions,
		NetworkPermissions:            req.NetworkPermissions,
		AccessBasedEnumerationEnabled: req.AccessBasedEnumerationEnabled,
		DefaultFileCreateMode:         req.DefaultFileCreateMode,
		DefaultDirectoryCreateMode:    req.DefaultDirectoryCreateMode,
		RequireEncryption:             req.RequireEncryption,
		AllowFSPathCreate:             allowFSPathCreate,
		ExpandFSPathVariables:         req.ExpandFSPathVariables,
		OfflineFilesCachingMode:       req.OfflineFilesCachingMode,
	}
}

func usesSMBV3Preview(req SMBShareRequest) bool {
	return req.TenantID != 0 || req.ExpandFSPathVariables || req.OfflineFilesCachingMode != ""
}

func validateSMBShareRequest(req SMBShareRequest) error {
	if err := rejectProtocolControlCharacters("SMB share name", req.ShareName); err != nil {
		return err
	}
	if strings.TrimSpace(req.ShareName) == "" {
		return fmt.Errorf("SMB share name is required")
	}
	if req.TenantID < 0 {
		return fmt.Errorf("SMB tenant ID cannot be negative")
	}
	if err := rejectProtocolControlCharacters("SMB filesystem path", req.FSPath); err != nil {
		return err
	}
	if strings.TrimSpace(req.FSPath) == "" {
		return fmt.Errorf("SMB filesystem path is required")
	}
	if !strings.HasPrefix(req.FSPath, "/") {
		return fmt.Errorf("SMB filesystem path %q must be absolute", req.FSPath)
	}
	if req.BytesPerSector != "" && req.BytesPerSector != "512" {
		return fmt.Errorf("SMB bytes_per_sector must be 512 when specified")
	}
	if err := validateSMBMode("default_file_create_mode", req.DefaultFileCreateMode); err != nil {
		return err
	}
	if err := validateSMBMode("default_directory_create_mode", req.DefaultDirectoryCreateMode); err != nil {
		return err
	}
	switch req.OfflineFilesCachingMode {
	case "", SMBOfflineFilesNoCaching, SMBOfflineFilesManualCaching, SMBOfflineFilesAutomaticCaching:
		return nil
	default:
		return fmt.Errorf("unsupported SMB offline-files caching mode %q", req.OfflineFilesCachingMode)
	}
}

func canonicalSMBShareRequest(req SMBShareRequest) SMBShareRequest {
	req.Permissions = nonNilProtocolSlice(req.Permissions)
	req.NetworkPermissions = nonNilProtocolSlice(req.NetworkPermissions)
	return req
}

func canonicalPatchSMBShareRequest(req PatchSMBShareRequest) PatchSMBShareRequest {
	if req.Permissions != nil && *req.Permissions == nil {
		empty := []SMBSharePermission{}
		req.Permissions = &empty
	}
	if req.NetworkPermissions != nil && *req.NetworkPermissions == nil {
		empty := []SMBNetworkPermission{}
		req.NetworkPermissions = &empty
	}
	return req
}

func validateSMBMode(field, mode string) error {
	if mode == "" {
		return nil
	}
	v, err := strconv.ParseUint(mode, 8, 12)
	if err != nil || v > 0o777 {
		return fmt.Errorf("SMB %s %q must be an octal mode from 0000 through 0777", field, mode)
	}
	return nil
}

func smbShareRefPath(ref string) (string, error) {
	if err := rejectProtocolControlCharacters("SMB share reference", ref); err != nil {
		return "", err
	}
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("SMB share reference is required")
	}
	return smbSharesPath + url.PathEscape(ref), nil
}

func smbShareV3RefPath(id string) (string, error) {
	if err := rejectProtocolControlCharacters("SMB share ID", id); err != nil {
		return "", err
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("SMB share ID is required")
	}
	return smbSharesV3PreviewPath + url.PathEscape(id), nil
}

func smbSharePatch(current SMBShare, desired SMBShareRequest) (PatchSMBShareRequest, bool) {
	return smbSharePatchForVersion(current, desired, true)
}

func smbSharePatchForVersion(current SMBShare, desired SMBShareRequest, includeBytesPerSector bool) (PatchSMBShareRequest, bool) {
	var patch PatchSMBShareRequest
	changed := false
	if current.ShareName != desired.ShareName {
		patch.ShareName = ptr(desired.ShareName)
		changed = true
	}
	if current.Description != desired.Description {
		patch.Description = ptr(desired.Description)
		changed = true
	}
	if !equalSMBSharePermissions(desired.Permissions, current.Permissions) {
		patch.Permissions = ptr(nonNilProtocolSlice(desired.Permissions))
		changed = true
	}
	if !equalSMBNetworkPermissions(current.NetworkPermissions, desired.NetworkPermissions) {
		patch.NetworkPermissions = ptr(nonNilProtocolSlice(desired.NetworkPermissions))
		changed = true
	}
	if current.AccessBasedEnumerationEnabled != desired.AccessBasedEnumerationEnabled {
		patch.AccessBasedEnumerationEnabled = ptr(desired.AccessBasedEnumerationEnabled)
		changed = true
	}
	if !smbModeEqual(current.DefaultFileCreateMode, desired.DefaultFileCreateMode, "0644") {
		patch.DefaultFileCreateMode = ptr(desired.DefaultFileCreateMode)
		changed = true
	}
	if !smbModeEqual(current.DefaultDirectoryCreateMode, desired.DefaultDirectoryCreateMode, "0755") {
		patch.DefaultDirectoryCreateMode = ptr(desired.DefaultDirectoryCreateMode)
		changed = true
	}
	if includeBytesPerSector && !smbModeEqual(current.BytesPerSector, desired.BytesPerSector, "512") {
		patch.BytesPerSector = ptr(desired.BytesPerSector)
		changed = true
	}
	if current.RequireEncryption != desired.RequireEncryption {
		patch.RequireEncryption = ptr(desired.RequireEncryption)
		changed = true
	}
	return patch, changed
}

func smbShareV3Patch(current SMBShare, desired SMBShareRequest, allowFSPathCreate bool) (PatchSMBShareRequest, bool) {
	patch, changed := smbSharePatchForVersion(current, desired, false)
	if desired.TenantID != 0 && current.TenantID != desired.TenantID {
		patch.TenantID = ptr(desired.TenantID)
		changed = true
	}
	if current.AllowFSPathCreate != allowFSPathCreate {
		patch.AllowFSPathCreate = ptr(allowFSPathCreate)
		changed = true
	}
	if current.ExpandFSPathVariables != desired.ExpandFSPathVariables {
		patch.ExpandFSPathVariables = ptr(desired.ExpandFSPathVariables)
		changed = true
	}
	if !smbModeEqual(string(current.OfflineFilesCachingMode), string(desired.OfflineFilesCachingMode), string(SMBOfflineFilesNoCaching)) {
		wanted := desired.OfflineFilesCachingMode
		if wanted == "" {
			wanted = SMBOfflineFilesNoCaching
		}
		patch.OfflineFilesCachingMode = ptr(wanted)
		changed = true
	}
	return patch, changed
}

func smbModeEqual(current, desired, defaultValue string) bool {
	if desired == "" {
		return current == "" || current == defaultValue
	}
	return current == desired
}

// equalSMBSharePermissions compares a desired permission list (as the driver
// requests it) with the current list Core reports. Core canonicalizes
// trustees — a request naming only auth_id or name comes back with auth_id,
// sid, uid, name, and domain all populated — so a field-for-field DeepEqual
// would report every share as drifted and re-patch forever. Trustees match
// on the strongest identifier the desired side actually specifies.
func equalSMBSharePermissions(desired, current []SMBSharePermission) bool {
	if len(desired) != len(current) {
		return false
	}
	for i := range desired {
		if desired[i].Type != current[i].Type || !smbTrusteeMatches(desired[i].Trustee, current[i].Trustee) || !slices.Equal(desired[i].Rights, current[i].Rights) {
			return false
		}
	}
	return true
}

func smbTrusteeMatches(desired, current Identity) bool {
	if desired.AuthID != "" {
		return desired.AuthID == current.AuthID
	}
	if desired.SID != "" {
		return desired.SID == current.SID
	}
	if desired.UID != nil {
		return current.UID != nil && *desired.UID == *current.UID
	}
	if desired.Name != "" {
		if !strings.EqualFold(desired.Name, current.Name) {
			return false
		}
		return desired.Domain == "" || strings.EqualFold(desired.Domain, current.Domain)
	}
	return reflect.DeepEqual(desired, current)
}

func equalSMBNetworkPermissions(a, b []SMBNetworkPermission) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || !slices.Equal(a[i].AddressRanges, b[i].AddressRanges) || !slices.Equal(a[i].Rights, b[i].Rights) {
			return false
		}
	}
	return true
}
