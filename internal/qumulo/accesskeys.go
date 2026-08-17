package qumulo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Identity is a Qumulo auth identity reference.
type Identity struct {
	Domain string `json:"domain,omitempty"`
	AuthID string `json:"auth_id,omitempty"`
	UID    *int   `json:"uid,omitempty"`
	GID    *int   `json:"gid,omitempty"`
	SID    string `json:"sid,omitempty"`
	Name   string `json:"name,omitempty"`
}

type AccessKey struct {
	AccessKeyID     string   `json:"access_key_id"`
	SecretAccessKey string   `json:"secret_access_key,omitempty"`
	Owner           Identity `json:"owner"`
	CreationTime    string   `json:"creation_time,omitempty"`
}

type accessKeyList struct {
	Entries []AccessKey  `json:"entries"`
	Paging  pageMetadata `json:"paging"`
}

type createAccessKeyRequest struct {
	User Identity `json:"user"`
}

func (c *Connection) CreateAccessKey(ctx context.Context, user Identity) (*AccessKey, error) {
	var out AccessKey
	_, err := c.DoJSON(ctx, http.MethodPost, "/v1/s3/access-keys/", nil, nil, createAccessKeyRequest{User: user}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Connection) ListAccessKeys(ctx context.Context, user string) ([]AccessKey, error) {
	// Core 7.9.2.2: ?user=... cannot be combined with limit/after.
	if user != "" {
		q := url.Values{}
		q.Set("user", user)
		var out accessKeyList
		_, err := c.DoJSON(ctx, http.MethodGet, "/v1/s3/access-keys/", q, nil, nil, &out)
		if err != nil {
			return nil, err
		}
		return out.Entries, nil
	}
	var all []AccessKey
	after := ""
	seen := make(map[string]struct{})
	for {
		q := url.Values{}
		if after != "" {
			q.Set("after", after)
		}
		q.Set("limit", "100")
		var out accessKeyList
		_, err := c.DoJSON(ctx, http.MethodGet, "/v1/s3/access-keys/", q, nil, nil, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Entries...)
		nextAfter, hasNext, err := nextPageAfter(out.Paging.Next, seen)
		if err != nil {
			return nil, fmt.Errorf("list access keys: %w", err)
		}
		if !hasNext {
			break
		}
		after = nextAfter
	}
	return all, nil
}

// ListAccessKeysByOwner returns the keys owned by a driver-managed local
// user, identified by auth_id when known (preferred) with the username as a
// fallback match.
//
// Core 7.9.2.2 locks (verified live):
//   - Deleting a local user does NOT delete its access keys.
//   - After delete+recreate of a same-named user, identity resolution BY
//     NAME can resolve to the REMOVED identity and fail with
//     cred_invalid_local_user_error (400) — so ?user=<name> is a trap.
//
// Strategy: fast path ?user=<auth_id> when we have one; on any 4xx, or when
// no auth_id is known, fall back to the paginated full listing filtered by
// owner auth_id or name. Without this a retried Grant wedges forever on the
// tombstoned identity.
func (c *Connection) ListAccessKeysByOwner(ctx context.Context, username, authID string) ([]AccessKey, error) {
	if authID != "" {
		keys, err := c.ListAccessKeys(ctx, authID)
		if err == nil {
			return keys, nil
		}
		if api, ok := AsAPIError(err); !ok || api.StatusCode < 400 || api.StatusCode >= 500 {
			return nil, err
		}
	}
	all, err := c.ListAccessKeys(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []AccessKey
	for _, k := range all {
		// Once an immutable auth id is known, never fall back to a name
		// match: Core permits a deleted local user name to be recreated, and
		// matching that replacement would revoke the new identity's keys.
		match := false
		if authID != "" {
			match = k.Owner.AuthID == authID
		} else if username != "" {
			match = k.Owner.Name == username
		}
		if match {
			out = append(out, k)
		}
	}
	return out, nil
}

func (c *Connection) DeleteAccessKey(ctx context.Context, accessKeyID string) error {
	_, err := c.DoJSON(ctx, http.MethodDelete, "/v1/s3/access-keys/"+url.PathEscape(accessKeyID), nil, nil, nil, nil)
	return err
}
