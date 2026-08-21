package qumulo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// FileAttributes is a subset of GET /v1/files/{ref}/info/attributes.
type FileAttributes struct {
	ID    string `json:"id"`
	Path  string `json:"path,omitempty"`
	Name  string `json:"name,omitempty"`
	Mode  string `json:"mode,omitempty"`
	Owner string `json:"owner,omitempty"`
	Type  string `json:"type,omitempty"`
}

// encodeFSPath builds the single path-encoded file ref Qumulo expects,
// including a leading slash: /k8s-buckets/foo → %2Fk8s-buckets%2Ffoo.
// Component-wise encoding without the leading %2F 404s on Core 7.9.2.2.
func encodeFSPath(p string) string {
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// PathEscape leaves slashes; Qumulo wants them encoded in the ref.
	return strings.ReplaceAll(url.PathEscape(p), "/", "%2F")
}

func filePath(fsPath, suffix string) string {
	return "/v1/files/" + fileRef(fsPath) + suffix
}

// fileRef encodes a filesystem path (`/k8s/...`) or a raw numeric file id.
func fileRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return encodeFSPath("/")
	}
	if strings.HasPrefix(ref, "/") {
		return encodeFSPath(ref)
	}
	return url.PathEscape(ref)
}

func (c *Connection) FileAttributes(ctx context.Context, fsPath string) (*FileAttributes, error) {
	return c.fileAttributes(ctx, fsPath, "")
}

func (c *Connection) fileAttributes(ctx context.Context, ref, snapshotID string) (*FileAttributes, error) {
	var out FileAttributes
	query := url.Values{}
	if snapshotID != "" {
		query.Set("snapshot", snapshotID)
	}
	var q url.Values
	if len(query) > 0 {
		q = query
	}
	_, err := c.DoJSON(ctx, http.MethodGet, filePath(ref, "/info/attributes"), q, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

const (
	FileTypeFile      = "FS_FILE_TYPE_FILE"
	FileTypeDirectory = "FS_FILE_TYPE_DIRECTORY"
	FileTypeSymlink   = "FS_FILE_TYPE_SYMLINK"
)

// DirectoryEntry is one child of GET /v1/files/{ref}/entries/.
type DirectoryEntry struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	ID     string `json:"id"`
	Mode   string `json:"mode"`
	Size   string `json:"size"`
	Target string `json:"symlink_target,omitempty"`
}

type directoryEntriesResponse struct {
	Path   string           `json:"path"`
	ID     string           `json:"id"`
	Files  []DirectoryEntry `json:"files"`
	Paging pageMetadata     `json:"paging"`
}

// ListDirectoryEntries lists children of a directory, optionally at a snapshot.
// Pending live lock: paging is modeled with paging.next like other collections.
func (c *Connection) ListDirectoryEntries(ctx context.Context, ref, snapshotID string) ([]DirectoryEntry, error) {
	var out []DirectoryEntry
	query := url.Values{}
	if snapshotID != "" {
		query.Set("snapshot", snapshotID)
	}
	seen := map[string]struct{}{}
	for {
		var page directoryEntriesResponse
		q := url.Values{}
		for k, vs := range query {
			q[k] = vs
		}
		_, err := c.DoJSON(ctx, http.MethodGet, filePath(ref, "/entries/"), q, nil, nil, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Files...)
		after, hasNext, err := nextPageAfter(page.Paging.Next, seen)
		if err != nil {
			return nil, err
		}
		if !hasNext {
			return out, nil
		}
		query.Set("after", after)
	}
}

type copyChunkRequest struct {
	SourceID       string `json:"source_id,omitempty"`
	SourcePath     string `json:"source_path,omitempty"`
	SourceSnapshot qint   `json:"source_snapshot,omitempty"`
	SourceOffset   string `json:"source_offset,omitempty"`
	TargetOffset   string `json:"target_offset,omitempty"`
	Length         string `json:"length,omitempty"`
}

// CopyFileFromSnapshot copies one file's contents from a snapshot into an
// already-created destination file using POST /v1/files/{ref}/copy-chunk.
func (c *Connection) CopyFileFromSnapshot(ctx context.Context, destRef, sourcePath, snapshotID string) error {
	if strings.TrimSpace(destRef) == "" || strings.TrimSpace(sourcePath) == "" {
		return fmt.Errorf("copy source and destination are required")
	}
	snapNum, err := strconv.Atoi(snapshotID)
	if err != nil || snapNum <= 0 {
		return fmt.Errorf("invalid snapshot id %q", snapshotID)
	}
	req := copyChunkRequest{SourceSnapshot: qint(snapNum)}
	if strings.HasPrefix(sourcePath, "/") {
		req.SourcePath = sourcePath
	} else {
		req.SourceID = sourcePath
	}
	for {
		var out copyChunkRequest
		_, err := c.DoJSON(ctx, http.MethodPost, filePath(destRef, "/copy-chunk"), nil, nil, req, &out)
		if err != nil {
			return err
		}
		if strings.TrimSpace(out.Length) == "" || out.Length == "0" {
			return nil
		}
		req = out
	}
}

// PatchFileMode sets the POSIX mode (e.g. "0777") on a directory.
func (c *Connection) PatchFileMode(ctx context.Context, fsPath, mode string) error {
	_, err := c.DoJSON(ctx, http.MethodPatch, filePath(fsPath, "/info/attributes"), nil, nil, map[string]string{"mode": mode}, nil)
	return err
}

// ACE is one Qumulo NFS/SMB ACL entry. Trustee is the numeric auth id.
//
// Core 7.9.2.2 REJECTS a PUT whose ACEs omit "flags" (decode_error on the
// whole aces array), and GET may return ACEs without it — so the field must
// never be omitempty, and PutACL normalizes nil to []. Verified live.
type ACE struct {
	Type    string   `json:"type"`
	Flags   []string `json:"flags"`
	Trustee string   `json:"trustee"`
	Rights  []string `json:"rights"`
}

// ACL is PUT/GET /v1/files/{ref}/info/acl (inner document).
type ACL struct {
	Control                 []string `json:"control"`
	PosixSpecialPermissions []any    `json:"posix_special_permissions"`
	ACES                    []ACE    `json:"aces"`
	ETag                    string   `json:"-"`
}

type aclGetResponse struct {
	Generated bool            `json:"generated"`
	ACL       ACL             `json:"acl"`
	Control   []string        `json:"control"`
	ACES      []ACE           `json:"aces"`
	Raw       json.RawMessage `json:"-"`
}

var (
	fsRightsRW = []string{"READ", "READ_EA", "READ_ATTR", "READ_ACL", "WRITE_EA", "WRITE_ATTR", "EXECUTE", "MODIFY", "EXTEND", "DELETE_CHILD", "SYNCHRONIZE"}
	fsRightsRO = []string{"READ", "READ_EA", "READ_ATTR", "READ_ACL", "EXECUTE", "SYNCHRONIZE"}
	fsInherit  = []string{"OBJECT_INHERIT", "CONTAINER_INHERIT"}
)

func (c *Connection) GetACL(ctx context.Context, fsPath string) (*ACL, error) {
	var raw json.RawMessage
	headers, err := c.DoJSON(ctx, http.MethodGet, filePath(fsPath, "/info/acl"), nil, nil, nil, &raw)
	if err != nil {
		return nil, err
	}
	acl, err := decodeACL(raw)
	if err != nil {
		return nil, err
	}
	acl.ETag = headers.Get(headerETag)
	return acl, nil
}

func decodeACL(raw json.RawMessage) (*ACL, error) {
	var wrap aclGetResponse
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("decode ACL: %w", err)
	}
	if len(wrap.ACL.ACES) > 0 || len(wrap.ACL.Control) > 0 {
		return &wrap.ACL, nil
	}
	if len(wrap.ACES) > 0 || len(wrap.Control) > 0 {
		return &ACL{Control: wrap.Control, ACES: wrap.ACES}, nil
	}
	return &ACL{Control: []string{"PRESENT"}, ACES: nil}, nil
}

func (c *Connection) PutACL(ctx context.Context, fsPath string, acl *ACL) error {
	if acl.Control == nil {
		acl.Control = []string{"PRESENT"}
	}
	// Core 7.9.2.2 rejects null/omitted fields on round-tripped ACEs.
	for i := range acl.ACES {
		if acl.ACES[i].Flags == nil {
			acl.ACES[i].Flags = []string{}
		}
		if acl.ACES[i].Rights == nil {
			acl.ACES[i].Rights = []string{}
		}
	}
	if acl.PosixSpecialPermissions == nil {
		acl.PosixSpecialPermissions = []any{}
	}
	_, err := c.DoJSON(ctx, http.MethodPut, filePath(fsPath, "/info/acl"), nil, ifMatchHeader(acl.ETag), acl, nil)
	return err
}

// GrantDirectoryAccess adds an inheritable ACE for trusteeID. Core 7.9.2.2
// S3 still enforces the directory ACL: a bucket policy Allow is not enough.
//
// Failure behavior is always fail closed. The historical chmod-0777 fallback
// is rejected: if its successful RPC response were lost, the original mode
// would never reach the durable account handle and could not be restored.
type DirectoryGrantResult struct {
	// RestoreMode remains for decoding and revoking legacy q3 account handles.
	// New grants never populate it.
	RestoreMode string
}

func (c *Connection) GrantDirectoryAccess(ctx context.Context, fsPath, trusteeID, accessMode string, fallbackChmod bool) (DirectoryGrantResult, error) {
	if trusteeID == "" {
		return DirectoryGrantResult{}, fmt.Errorf("grant directory access: empty trustee id")
	}
	if fallbackChmod {
		return DirectoryGrantResult{}, fmt.Errorf("aclFallbackChmod is disabled because its original mode cannot be recovered after a lost response")
	}
	readOnly := strings.EqualFold(accessMode, "ro") || strings.EqualFold(accessMode, "readonly")
	rights := fsRightsRW
	if readOnly {
		rights = fsRightsRO
	}
	// Serialize ACL read-modify-write: concurrent grants/revokes on the same
	// connection could otherwise drop each other's ACEs.
	c.aclMu.Lock()
	defer c.aclMu.Unlock()
	for attempt := 0; attempt < 4; attempt++ {
		acl, err := c.GetACL(ctx, fsPath)
		if err != nil {
			return DirectoryGrantResult{}, fmt.Errorf("get ACL %s: %w", fsPath, err)
		}
		desired := ACE{
			Type:    "ALLOWED",
			Flags:   append([]string(nil), fsInherit...),
			Trustee: trusteeID,
			Rights:  append([]string(nil), rights...),
		}
		if aclHasExactTrusteeGrant(acl, desired) {
			return DirectoryGrantResult{}, nil
		}
		// The local user is uniquely minted for this BucketAccess. Reconcile
		// every ACE for that identity to one exact driver-managed grant so a
		// retry cannot retain stale RW rights, a DENIED entry, or missing
		// inheritance flags.
		dst := acl.ACES[:0]
		for _, ace := range acl.ACES {
			if ace.Trustee != trusteeID {
				dst = append(dst, ace)
			}
		}
		acl.ACES = append(dst, desired)
		if err := c.PutACL(ctx, fsPath, acl); err != nil {
			if isAPIPreconditionFailed(err) {
				continue
			}
			return DirectoryGrantResult{}, fmt.Errorf("put ACL %s: %w", fsPath, err)
		}
		return DirectoryGrantResult{}, nil
	}
	return DirectoryGrantResult{}, fmt.Errorf("grant directory access on %s: concurrent ACL modifications did not settle", fsPath)
}

// RevokeDirectoryAccess drops ACEs for trusteeID. Missing is a no-op.
func (c *Connection) RevokeDirectoryAccess(ctx context.Context, fsPath, trusteeID string) error {
	if trusteeID == "" {
		return nil
	}
	c.aclMu.Lock()
	defer c.aclMu.Unlock()
	for attempt := 0; attempt < 4; attempt++ {
		acl, err := c.GetACL(ctx, fsPath)
		if err != nil {
			if api, ok := AsAPIError(err); ok && api.IsNotFound() {
				return nil
			}
			return fmt.Errorf("get ACL %s while revoking trustee %s: %w", fsPath, trusteeID, err)
		}
		dst := acl.ACES[:0]
		for _, ace := range acl.ACES {
			if ace.Trustee == trusteeID {
				continue
			}
			dst = append(dst, ace)
		}
		if len(dst) == len(acl.ACES) {
			return nil
		}
		acl.ACES = dst
		if err := c.PutACL(ctx, fsPath, acl); err != nil {
			if isAPIPreconditionFailed(err) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("revoke directory access on %s: concurrent ACL modifications did not settle", fsPath)
}

func aclHasTrustee(acl *ACL, trusteeID string) bool {
	if acl == nil {
		return false
	}
	for _, ace := range acl.ACES {
		if ace.Trustee == trusteeID {
			return true
		}
	}
	return false
}

func aclHasExactTrusteeGrant(acl *ACL, desired ACE) bool {
	matchCount := 0
	for _, ace := range acl.ACES {
		if ace.Trustee != desired.Trustee {
			continue
		}
		matchCount++
		if matchCount > 1 || ace.Type != desired.Type || !sameStringsAsSet(ace.Flags, desired.Flags) || !sameStringsAsSet(ace.Rights, desired.Rights) {
			return false
		}
	}
	return matchCount == 1
}

func sameStringsAsSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	lcopy, rcopy := append([]string(nil), left...), append([]string(nil), right...)
	slices.Sort(lcopy)
	slices.Sort(rcopy)
	return slices.Equal(lcopy, rcopy)
}
