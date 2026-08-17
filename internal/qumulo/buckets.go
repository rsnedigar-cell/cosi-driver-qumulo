package qumulo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Bucket is a Qumulo S3 bucket description.
type Bucket struct {
	Name                   string     `json:"name"`
	CreationTime           string     `json:"creation_time,omitempty"`
	Path                   string     `json:"path,omitempty"`
	AnonymousAccessEnabled bool       `json:"anonymous_access_enabled,omitempty"`
	Versioning             string     `json:"versioning,omitempty"`
	LockConfig             LockConfig `json:"lock_config,omitempty"`
}

type LockConfig struct {
	Enabled          bool              `json:"enabled"`
	DefaultRetention *DefaultRetention `json:"default_retention,omitempty"`
}

type DefaultRetention struct {
	Units string `json:"units"`
	Value int    `json:"value"`
}

type CreateBucketRequest struct {
	Name              string `json:"name"`
	Path              string `json:"path,omitempty"`
	CreateFSPath      *bool  `json:"create_fs_path,omitempty"`
	ObjectLockEnabled *bool  `json:"object_lock_enabled,omitempty"`
	Private           *bool  `json:"private,omitempty"`
}

type bucketList struct {
	Buckets []Bucket `json:"buckets"`
}

type PatchBucketRequest struct {
	Versioning             *string     `json:"versioning,omitempty"`
	LockConfig             *LockConfig `json:"lock_config,omitempty"`
	AnonymousAccessEnabled *bool       `json:"anonymous_access_enabled,omitempty"`
}

type MultipartUpload struct {
	UploadID  string `json:"id"`
	Key       string `json:"key,omitempty"`
	Initiated string `json:"initiated,omitempty"`
	Initiator any    `json:"initiator,omitempty"`
}

type uploadList struct {
	Uploads []MultipartUpload `json:"uploads"`
	Entries []MultipartUpload `json:"entries"`
	Paging  pageMetadata      `json:"paging"`
}

func boolPtr(v bool) *bool { return &v }

func (c *Connection) CreateBucket(ctx context.Context, req CreateBucketRequest) (*Bucket, error) {
	var out Bucket
	_, err := c.DoJSON(ctx, http.MethodPost, "/v1/s3/buckets/", nil, nil, req, &out)
	if err != nil {
		return nil, err
	}
	if out.Name == "" {
		out.Name = req.Name
	}
	return &out, nil
}

func (c *Connection) ListBuckets(ctx context.Context) ([]Bucket, error) {
	var out bucketList
	_, err := c.DoJSON(ctx, http.MethodGet, "/v1/s3/buckets/", nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	return out.Buckets, nil
}

func (c *Connection) GetBucket(ctx context.Context, name string) (*Bucket, error) {
	// Try a direct GET first; Core 7.9.2.2 always answers 405 here (the
	// "client lists and filters" API lock), so the listing below is the
	// authoritative path on live clusters.
	var out Bucket
	_, err := c.DoJSON(ctx, http.MethodGet, "/v1/s3/buckets/"+url.PathEscape(name), nil, nil, nil, &out)
	if err == nil && out.Name != "" {
		return &out, nil
	}
	all, lerr := c.ListBuckets(ctx)
	if lerr != nil {
		// Listing is the authoritative lookup on Core versions whose direct
		// GET always returns 405. Surface the listing failure (for example a
		// retryable 503), not that expected/stale 405 response.
		return nil, lerr
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	// The listing succeeded and lacks the bucket: it does not exist. Return
	// the synthesized not-found, never the stale GET error (a 405 on live
	// Core) — callers key delete idempotency off IsNotFound, and surfacing
	// the 405 here wedges retried deletes forever.
	return nil, &APIError{StatusCode: http.StatusNotFound, ErrorClass: ErrClassS3BucketNotFound, Description: fmt.Sprintf("bucket %q not found", name)}
}

func (c *Connection) PatchBucket(ctx context.Context, name string, req PatchBucketRequest) (*Bucket, error) {
	var out Bucket
	_, err := c.DoJSON(ctx, http.MethodPatch, "/v1/s3/buckets/"+url.PathEscape(name), nil, nil, req, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Connection) DeleteBucket(ctx context.Context, name string, deleteRootDir bool) error {
	q := url.Values{}
	if deleteRootDir {
		q.Set("delete-root-dir", "true")
	} else {
		q.Set("delete-root-dir", "false")
	}
	_, err := c.DoJSON(ctx, http.MethodDelete, "/v1/s3/buckets/"+url.PathEscape(name), q, nil, nil, nil)
	return err
}

func (c *Connection) ListUploads(ctx context.Context, bucket string) ([]MultipartUpload, error) {
	endpoint := "/v1/s3/buckets/" + url.PathEscape(bucket) + "/uploads/"
	var all []MultipartUpload
	after := ""
	seen := make(map[string]struct{})
	for {
		q := url.Values{}
		q.Set("limit", "100")
		if after != "" {
			q.Set("after", after)
		}
		var out uploadList
		_, err := c.DoJSON(ctx, http.MethodGet, endpoint, q, nil, nil, &out)
		if err != nil {
			return nil, err
		}
		page := out.Uploads
		if len(page) == 0 {
			// Retain compatibility with older response shapes used by some
			// Qumulo releases and development fakes.
			page = out.Entries
		}
		all = append(all, page...)
		nextAfter, hasNext, err := nextPageAfter(out.Paging.Next, seen)
		if err != nil {
			return nil, fmt.Errorf("list multipart uploads for bucket %q: %w", bucket, err)
		}
		if !hasNext {
			break
		}
		after = nextAfter
	}
	return all, nil
}

func (c *Connection) AbortUpload(ctx context.Context, bucket, uploadID string) error {
	_, err := c.DoJSON(ctx, http.MethodDelete, "/v1/s3/buckets/"+url.PathEscape(bucket)+"/uploads/"+url.PathEscape(uploadID), nil, nil, nil, nil)
	return err
}

func (c *Connection) AbortAllUploads(ctx context.Context, bucket string) (int, error) {
	ups, err := c.ListUploads(ctx, bucket)
	if err != nil {
		if api, ok := AsAPIError(err); ok && api.IsNotFound() {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, u := range ups {
		id := u.UploadID
		if id == "" {
			continue
		}
		if err := c.AbortUpload(ctx, bucket, id); err != nil {
			if api, ok := AsAPIError(err); ok && api.IsNotFound() {
				continue
			}
			return n, err
		}
		n++
	}
	return n, nil
}
