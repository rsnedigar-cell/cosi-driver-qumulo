package qumulo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"unicode"
)

const (
	nfsExportsPath          = "/v2/nfs/exports/"
	nfsExportsV3PreviewPath = "/v3/nfs/exports/"
)

// NFSAuthenticationMode is the security flavor required by an export
// restriction.
type NFSAuthenticationMode string

const (
	NFSAuthNone  NFSAuthenticationMode = "AUTHENTICATION_MODE_NONE"
	NFSAuthKRB5  NFSAuthenticationMode = "AUTHENTICATION_MODE_KRB5"
	NFSAuthKRB5I NFSAuthenticationMode = "AUTHENTICATION_MODE_KRB5I"
	NFSAuthKRB5P NFSAuthenticationMode = "AUTHENTICATION_MODE_KRB5P"
)

// NFSUserMapping controls user squashing for an export restriction.
type NFSUserMapping string

const (
	NFSMapNone NFSUserMapping = "NFS_MAP_NONE"
	NFSMapRoot NFSUserMapping = "NFS_MAP_ROOT"
	NFSMapAll  NFSUserMapping = "NFS_MAP_ALL"
)

// NFSIdentityType identifies the kind of user or group used for NFS mapping.
type NFSIdentityType string

const (
	NFSIdentityLocalUser      NFSIdentityType = "LOCAL_USER"
	NFSIdentityLocalGroup     NFSIdentityType = "LOCAL_GROUP"
	NFSIdentityGID            NFSIdentityType = "NFS_GID"
	NFSIdentityUID            NFSIdentityType = "NFS_UID"
	NFSIdentitySMBSID         NFSIdentityType = "SMB_SID"
	NFSIdentityInternal       NFSIdentityType = "INTERNAL"
	NFSIdentityQumuloOperator NFSIdentityType = "QUMULO_OPERATOR"
	NFSIdentityQumuloSupport  NFSIdentityType = "QUMULO_SUPPORT"
)

// NFS32BitField selects NFSv3 values that Core presents as 32-bit values.
type NFS32BitField string

const (
	NFS32BitFileIDs   NFS32BitField = "FILE_IDS"
	NFS32BitFileSizes NFS32BitField = "FILE_SIZES"
	NFS32BitFSSize    NFS32BitField = "FS_SIZE"
	NFS32BitAll       NFS32BitField = "ALL"
)

// NFSIdentity is the id_type/id_value identity form used by the NFS API.
// It is intentionally distinct from Identity, which is the domain/auth_id
// identity form used by the SMB and S3 APIs.
type NFSIdentity struct {
	IDType  NFSIdentityType `json:"id_type"`
	IDValue string          `json:"id_value"`
}

// NFSRestriction describes access for one ordered group of NFS clients.
// An empty HostRestrictions list means all hosts.
type NFSRestriction struct {
	HostRestrictions           []string              `json:"host_restrictions"`
	RequirePrivilegedPort      bool                  `json:"require_privileged_port"`
	ReadOnly                   bool                  `json:"read_only"`
	RequiredAuthenticationMode NFSAuthenticationMode `json:"required_authentication_mode"`
	UserMapping                NFSUserMapping        `json:"user_mapping"`
	MapToUser                  *NFSIdentity          `json:"map_to_user,omitempty"`
	MapToGroup                 *NFSIdentity          `json:"map_to_group,omitempty"`
}

// NFSExport is the representation returned by Qumulo Core. ETag is response
// metadata used for optimistic concurrency and is never serialized.
type NFSExport struct {
	ID                     string           `json:"id"`
	ExportPath             string           `json:"export_path"`
	FSPath                 string           `json:"fs_path"`
	Description            string           `json:"description"`
	Restrictions           []NFSRestriction `json:"restrictions"`
	FieldsToPresentAs32Bit []NFS32BitField  `json:"fields_to_present_as_32_bit"`
	TenantID               int64            `json:"tenant_id,omitempty"`
	ETag                   string           `json:"-"`
}

// NFSExportRequest contains the writable fields used to create or fully
// replace an NFS export.
type NFSExportRequest struct {
	ExportPath             string           `json:"export_path"`
	TenantID               int64            `json:"tenant_id,omitempty"`
	FSPath                 string           `json:"fs_path"`
	Description            string           `json:"description"`
	Restrictions           []NFSRestriction `json:"restrictions"`
	FieldsToPresentAs32Bit []NFS32BitField  `json:"fields_to_present_as_32_bit,omitempty"`
}

// PatchNFSExportRequest contains only fields that should be modified. A
// pointer to an empty slice explicitly clears the corresponding array.
type PatchNFSExportRequest struct {
	ExportPath             *string           `json:"export_path,omitempty"`
	TenantID               *int64            `json:"tenant_id,omitempty"`
	FSPath                 *string           `json:"fs_path,omitempty"`
	Description            *string           `json:"description,omitempty"`
	Restrictions           *[]NFSRestriction `json:"restrictions,omitempty"`
	FieldsToPresentAs32Bit *[]NFS32BitField  `json:"fields_to_present_as_32_bit,omitempty"`
}

// NFSExportWriteOptions controls filesystem-path creation and optimistic
// concurrency for writes.
type NFSExportWriteOptions struct {
	AllowFSPathCreate bool
	IfMatch           string
}

// CreateNFSExport creates an export using Qumulo's stable v2 NFS API.
func (c *Connection) CreateNFSExport(ctx context.Context, req NFSExportRequest, allowFSPathCreate bool) (*NFSExport, error) {
	if err := validateNFSExportRequest(req); err != nil {
		return nil, err
	}
	req = canonicalNFSExportRequest(req)
	if req.TenantID != 0 {
		return c.CreateNFSExportV3Preview(ctx, req, allowFSPathCreate)
	}
	var out NFSExport
	h, err := c.DoJSON(ctx, http.MethodPost, nfsExportsPath, allowFSPathCreateQuery(allowFSPathCreate), nil, req, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// ListNFSExports lists every NFS export.
func (c *Connection) ListNFSExports(ctx context.Context) ([]NFSExport, error) {
	var out []NFSExport
	_, err := c.DoJSON(ctx, http.MethodGet, nfsExportsPath, nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetNFSExport retrieves an export by ID or export path.
func (c *Connection) GetNFSExport(ctx context.Context, ref string) (*NFSExport, error) {
	pth, err := nfsExportRefPath(ref)
	if err != nil {
		return nil, err
	}
	var out NFSExport
	h, err := c.DoJSON(ctx, http.MethodGet, pth, nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// ReplaceNFSExport sets all writable attributes of an export.
func (c *Connection) ReplaceNFSExport(ctx context.Context, ref string, req NFSExportRequest, opts NFSExportWriteOptions) (*NFSExport, error) {
	if err := validateNFSExportRequest(req); err != nil {
		return nil, err
	}
	req = canonicalNFSExportRequest(req)
	if req.TenantID != 0 {
		return c.ReplaceNFSExportV3Preview(ctx, ref, req, opts)
	}
	pth, err := nfsExportRefPath(ref)
	if err != nil {
		return nil, err
	}
	var out NFSExport
	h, err := c.DoJSON(ctx, http.MethodPut, pth, allowFSPathCreateQuery(opts.AllowFSPathCreate), ifMatchHeader(opts.IfMatch), req, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// PatchNFSExport modifies selected attributes of an export.
func (c *Connection) PatchNFSExport(ctx context.Context, ref string, req PatchNFSExportRequest, opts NFSExportWriteOptions) (*NFSExport, error) {
	req = canonicalPatchNFSExportRequest(req)
	if req.TenantID != nil {
		return c.PatchNFSExportV3Preview(ctx, ref, req, opts)
	}
	pth, err := nfsExportRefPath(ref)
	if err != nil {
		return nil, err
	}
	var out NFSExport
	h, err := c.DoJSON(ctx, http.MethodPatch, pth, allowFSPathCreateQuery(opts.AllowFSPathCreate), ifMatchHeader(opts.IfMatch), req, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// DeleteNFSExport deletes an export by ID or export path. An optional ETag
// prevents deletion when the export changed after it was read.
func (c *Connection) DeleteNFSExport(ctx context.Context, ref, ifMatch string) error {
	pth, err := nfsExportRefPath(ref)
	if err != nil {
		return err
	}
	_, err = c.DoJSON(ctx, http.MethodDelete, pth, nil, ifMatchHeader(ifMatch), nil, nil)
	return err
}

// DeleteNFSExportIfExists performs an idempotent, conditional delete. It
// returns true only when this call observed and deleted an export.
func (c *Connection) DeleteNFSExportIfExists(ctx context.Context, ref string) (bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		current, err := c.GetNFSExport(ctx, ref)
		if err != nil {
			if isAPINotFound(err) {
				return false, nil
			}
			return false, err
		}
		target := current.ID
		if target == "" {
			target = ref
		}
		if err := c.DeleteNFSExport(ctx, target, current.ETag); err != nil {
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
	return false, fmt.Errorf("delete NFS export %q: concurrent modifications did not settle", ref)
}

// EnsureNFSExport creates the export or reconciles its writable settings. It
// never retargets an existing export to a different filesystem path. The
// boolean result reports whether this call definitely created the export.
func (c *Connection) EnsureNFSExport(ctx context.Context, desired NFSExportRequest, allowFSPathCreate bool) (*NFSExport, bool, error) {
	return c.ensureNFSExport(ctx, desired, allowFSPathCreate, false)
}

// ClaimNFSExport is the strict create-or-reconcile variant used by CSI. If a
// same-path export already exists, its description must exactly match the
// caller's immutable specification marker. This check is repeated after a
// create conflict and after every ETag retry, closing the preflight/create
// race without mutating another request's resource.
func (c *Connection) ClaimNFSExport(ctx context.Context, desired NFSExportRequest, allowFSPathCreate bool) (*NFSExport, bool, error) {
	return c.ensureNFSExport(ctx, desired, allowFSPathCreate, true)
}

func (c *Connection) ensureNFSExport(ctx context.Context, desired NFSExportRequest, allowFSPathCreate, strictDescription bool) (*NFSExport, bool, error) {
	if err := validateNFSExportRequest(desired); err != nil {
		return nil, false, err
	}
	desired = canonicalNFSExportRequest(desired)
	if desired.TenantID != 0 {
		return c.ensureNFSExportV3Preview(ctx, desired, allowFSPathCreate)
	}
	current, err := c.GetNFSExport(ctx, desired.ExportPath)
	if err != nil && !isAPINotFound(err) {
		return nil, false, err
	}
	if isAPINotFound(err) {
		created, createErr := c.CreateNFSExport(ctx, desired, allowFSPathCreate)
		if createErr == nil {
			return created, true, nil
		}
		if isAPIAlreadyExists(createErr) {
			current = nil
		} else if isAmbiguousCreateError(createErr) {
			current, err = c.GetNFSExport(ctx, desired.ExportPath)
			if err != nil {
				if isAPINotFound(err) {
					return nil, false, createErr
				}
				return nil, false, fmt.Errorf("reconcile ambiguous NFS export create: %w (create error: %v)", err, createErr)
			}
		} else {
			return nil, false, createErr
		}
	}

	for attempt := 0; attempt < 3; attempt++ {
		if current == nil {
			current, err = c.GetNFSExport(ctx, desired.ExportPath)
			if err != nil {
				return nil, false, err
			}
		}
		if strictDescription && current.Description != desired.Description {
			return nil, false, fmt.Errorf("%w: NFS export %q has specification marker %q, requested %q", ErrResourceClaimConflict, desired.ExportPath, current.Description, desired.Description)
		}
		if current.FSPath != desired.FSPath {
			return nil, false, fmt.Errorf("NFS export %q already targets %q, refusing to retarget it to %q", desired.ExportPath, current.FSPath, desired.FSPath)
		}
		patch, changed := nfsExportPatch(*current, desired)
		if !changed {
			return current, false, nil
		}
		updated, patchErr := c.PatchNFSExport(ctx, current.IDOrPath(), patch, NFSExportWriteOptions{
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
	return nil, false, fmt.Errorf("ensure NFS export %q: concurrent modifications did not settle", desired.ExportPath)
}

func (e NFSExport) IDOrPath() string {
	if e.ID != "" {
		return e.ID
	}
	return e.ExportPath
}

// CreateNFSExportV3Preview creates a tenant-aware export. Qumulo documents
// the v3 NFS surface as preview; callers should use it only when tenant-aware
// provisioning is required.
func (c *Connection) CreateNFSExportV3Preview(ctx context.Context, req NFSExportRequest, allowFSPathCreate bool) (*NFSExport, error) {
	if err := validateNFSExportRequest(req); err != nil {
		return nil, err
	}
	req = canonicalNFSExportRequest(req)
	var out NFSExport
	h, err := c.DoJSON(ctx, http.MethodPost, nfsExportsV3PreviewPath, allowFSPathCreateQuery(allowFSPathCreate), nil, req, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// ListNFSExportsV3Preview lists tenant-aware exports. Unlike the stable v2
// endpoint, the v3 preview endpoint wraps the array in an entries object.
func (c *Connection) ListNFSExportsV3Preview(ctx context.Context) ([]NFSExport, error) {
	var out struct {
		Entries []NFSExport `json:"entries"`
	}
	_, err := c.DoJSON(ctx, http.MethodGet, nfsExportsV3PreviewPath, nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// GetNFSExportV3Preview retrieves a tenant-aware export by its ID.
func (c *Connection) GetNFSExportV3Preview(ctx context.Context, id string) (*NFSExport, error) {
	pth, err := nfsExportV3RefPath(id)
	if err != nil {
		return nil, err
	}
	var out NFSExport
	h, err := c.DoJSON(ctx, http.MethodGet, pth, nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// ReplaceNFSExportV3Preview sets every attribute of a tenant-aware export.
func (c *Connection) ReplaceNFSExportV3Preview(ctx context.Context, id string, req NFSExportRequest, opts NFSExportWriteOptions) (*NFSExport, error) {
	if err := validateNFSExportRequest(req); err != nil {
		return nil, err
	}
	req = canonicalNFSExportRequest(req)
	pth, err := nfsExportV3RefPath(id)
	if err != nil {
		return nil, err
	}
	var out NFSExport
	h, err := c.DoJSON(ctx, http.MethodPut, pth, allowFSPathCreateQuery(opts.AllowFSPathCreate), ifMatchHeader(opts.IfMatch), req, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// PatchNFSExportV3Preview modifies selected attributes of a tenant-aware
// export.
func (c *Connection) PatchNFSExportV3Preview(ctx context.Context, id string, req PatchNFSExportRequest, opts NFSExportWriteOptions) (*NFSExport, error) {
	req = canonicalPatchNFSExportRequest(req)
	pth, err := nfsExportV3RefPath(id)
	if err != nil {
		return nil, err
	}
	var out NFSExport
	h, err := c.DoJSON(ctx, http.MethodPatch, pth, allowFSPathCreateQuery(opts.AllowFSPathCreate), ifMatchHeader(opts.IfMatch), req, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = h.Get(headerETag)
	return &out, nil
}

// DeleteNFSExportV3Preview deletes a tenant-aware export by ID.
func (c *Connection) DeleteNFSExportV3Preview(ctx context.Context, id, ifMatch string) error {
	pth, err := nfsExportV3RefPath(id)
	if err != nil {
		return err
	}
	_, err = c.DoJSON(ctx, http.MethodDelete, pth, nil, ifMatchHeader(ifMatch), nil, nil)
	return err
}

func (c *Connection) ensureNFSExportV3Preview(ctx context.Context, desired NFSExportRequest, allowFSPathCreate bool) (*NFSExport, bool, error) {
	current, err := c.findNFSExportV3Preview(ctx, desired.TenantID, desired.ExportPath)
	if err != nil {
		return nil, false, err
	}
	if current == nil {
		created, createErr := c.CreateNFSExportV3Preview(ctx, desired, allowFSPathCreate)
		if createErr == nil {
			return created, true, nil
		}
		if isAPIAlreadyExists(createErr) {
			current = nil
		} else if isAmbiguousCreateError(createErr) {
			current, err = c.findNFSExportV3Preview(ctx, desired.TenantID, desired.ExportPath)
			if err != nil {
				return nil, false, fmt.Errorf("reconcile ambiguous tenant NFS export create: %w (create error: %v)", err, createErr)
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
			current, err = c.findNFSExportV3Preview(ctx, desired.TenantID, desired.ExportPath)
			if err != nil {
				return nil, false, err
			}
			if current == nil {
				continue
			}
		}
		if current.FSPath != desired.FSPath {
			return nil, false, fmt.Errorf("NFS export %q in tenant %d already targets %q, refusing to retarget it to %q", desired.ExportPath, desired.TenantID, current.FSPath, desired.FSPath)
		}
		patch, changed := nfsExportPatch(*current, desired)
		if !changed {
			return current, false, nil
		}
		updated, patchErr := c.PatchNFSExportV3Preview(ctx, current.ID, patch, NFSExportWriteOptions{
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
	return nil, false, fmt.Errorf("ensure NFS export %q in tenant %d: concurrent creation or modification did not settle", desired.ExportPath, desired.TenantID)
}

func (c *Connection) findNFSExportV3Preview(ctx context.Context, tenantID int64, exportPath string) (*NFSExport, error) {
	exports, err := c.ListNFSExportsV3Preview(ctx)
	if err != nil {
		return nil, err
	}
	var found *NFSExport
	for i := range exports {
		if exports[i].TenantID != tenantID || exports[i].ExportPath != exportPath {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple NFS exports named %q found in tenant %d", exportPath, tenantID)
		}
		found = &exports[i]
	}
	if found == nil {
		return nil, nil
	}
	if found.ID == "" {
		return nil, fmt.Errorf("NFS v3 list returned export %q in tenant %d without an ID", exportPath, tenantID)
	}
	full, err := c.GetNFSExportV3Preview(ctx, found.ID)
	if isAPINotFound(err) {
		return nil, nil
	}
	return full, err
}

func validateNFSExportRequest(req NFSExportRequest) error {
	if err := rejectProtocolControlCharacters("NFS export path", req.ExportPath); err != nil {
		return err
	}
	if strings.TrimSpace(req.ExportPath) == "" {
		return fmt.Errorf("NFS export path is required")
	}
	if !strings.HasPrefix(req.ExportPath, "/") {
		return fmt.Errorf("NFS export path %q must be absolute", req.ExportPath)
	}
	if req.TenantID < 0 {
		return fmt.Errorf("NFS tenant ID cannot be negative")
	}
	if err := rejectProtocolControlCharacters("NFS filesystem path", req.FSPath); err != nil {
		return err
	}
	if strings.TrimSpace(req.FSPath) == "" {
		return fmt.Errorf("NFS filesystem path is required")
	}
	if !strings.HasPrefix(req.FSPath, "/") {
		return fmt.Errorf("NFS filesystem path %q must be absolute", req.FSPath)
	}
	return nil
}

func canonicalNFSExportRequest(req NFSExportRequest) NFSExportRequest {
	req.Restrictions = nonNilProtocolSlice(req.Restrictions)
	req.FieldsToPresentAs32Bit = nonNilProtocolSlice(req.FieldsToPresentAs32Bit)
	return req
}

func canonicalPatchNFSExportRequest(req PatchNFSExportRequest) PatchNFSExportRequest {
	if req.Restrictions != nil && *req.Restrictions == nil {
		empty := []NFSRestriction{}
		req.Restrictions = &empty
	}
	if req.FieldsToPresentAs32Bit != nil && *req.FieldsToPresentAs32Bit == nil {
		empty := []NFS32BitField{}
		req.FieldsToPresentAs32Bit = &empty
	}
	return req
}

func nfsExportRefPath(ref string) (string, error) {
	if err := rejectProtocolControlCharacters("NFS export reference", ref); err != nil {
		return "", err
	}
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("NFS export reference is required")
	}
	return nfsExportsPath + url.PathEscape(ref), nil
}

func nfsExportV3RefPath(id string) (string, error) {
	if err := rejectProtocolControlCharacters("NFS export ID", id); err != nil {
		return "", err
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("NFS export ID is required")
	}
	return nfsExportsV3PreviewPath + url.PathEscape(id), nil
}

func nfsExportPatch(current NFSExport, desired NFSExportRequest) (PatchNFSExportRequest, bool) {
	var patch PatchNFSExportRequest
	changed := false
	if current.ExportPath != desired.ExportPath {
		patch.ExportPath = ptr(desired.ExportPath)
		changed = true
	}
	if desired.TenantID != 0 && current.TenantID != desired.TenantID {
		patch.TenantID = ptr(desired.TenantID)
		changed = true
	}
	if current.Description != desired.Description {
		patch.Description = ptr(desired.Description)
		changed = true
	}
	if !equalNFSRestrictions(current.Restrictions, desired.Restrictions) {
		patch.Restrictions = ptr(nonNilProtocolSlice(desired.Restrictions))
		changed = true
	}
	if !slices.Equal(current.FieldsToPresentAs32Bit, desired.FieldsToPresentAs32Bit) {
		patch.FieldsToPresentAs32Bit = ptr(nonNilProtocolSlice(desired.FieldsToPresentAs32Bit))
		changed = true
	}
	return patch, changed
}

func equalNFSRestrictions(a, b []NFSRestriction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		aa, bb := a[i], b[i]
		aHosts, bHosts := aa.HostRestrictions, bb.HostRestrictions
		aa.HostRestrictions, bb.HostRestrictions = nil, nil
		if !slices.Equal(aHosts, bHosts) || !reflect.DeepEqual(aa, bb) {
			return false
		}
	}
	return true
}

func allowFSPathCreateQuery(allow bool) url.Values {
	if !allow {
		return nil
	}
	return url.Values{"allow-fs-path-create": []string{"true"}}
}

func ifMatchHeader(etag string) http.Header {
	if etag == "" {
		return nil
	}
	return http.Header{headerIfMatch: []string{etag}}
}

func isAPINotFound(err error) bool {
	api, ok := AsAPIError(err)
	return ok && api.IsNotFound()
}

func isAPIAlreadyExists(err error) bool {
	api, ok := AsAPIError(err)
	return ok && api.IsAlreadyExists()
}

func isAPIPreconditionFailed(err error) bool {
	api, ok := AsAPIError(err)
	return ok && (api.StatusCode == http.StatusPreconditionFailed || api.IsClass(ErrClassRESTPrecondition))
}

func isAmbiguousCreateError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if api, ok := AsAPIError(err); ok {
		return api.StatusCode >= http.StatusInternalServerError
	}
	// Non-API failures after an HTTP request is constructed are normally
	// transport failures, for which the server may have committed the POST.
	return true
}

func rejectProtocolControlCharacters(field, value string) error {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains a control character", field)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func nonNilProtocolSlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
