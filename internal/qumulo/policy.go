package qumulo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrPolicyUnchanged is returned by a MutateBucketPolicy mutate callback to
// signal that the current policy already satisfies the desired state and no
// PUT should be issued.
var ErrPolicyUnchanged = errors.New("bucket policy unchanged")

// Policy is an AWS-style S3 bucket policy document.
type Policy struct {
	Version   string            `json:"Version"`
	Statement []PolicyStatement `json:"Statement"`
}

type PolicyStatement struct {
	Sid       string          `json:"Sid,omitempty"`
	Effect    string          `json:"Effect"`
	Principal json.RawMessage `json:"Principal,omitempty"`
	Action    any             `json:"Action"`
	Resource  any             `json:"Resource,omitempty"`
}

type policyGetResponse struct {
	Policy json.RawMessage `json:"policy"`
	ETag   string          `json:"etag"`
}

// GetBucketPolicy returns the policy document and its ETag.
func (c *Connection) GetBucketPolicy(ctx context.Context, bucket string) (*Policy, string, error) {
	var raw json.RawMessage
	hdr, err := c.DoJSON(ctx, http.MethodGet, "/v1/s3/buckets/"+url.PathEscape(bucket)+"/policy", nil, nil, nil, &raw)
	if err != nil {
		return nil, "", err
	}
	etag := strings.Trim(hdr.Get(headerETag), `"`)
	// Response may be the policy itself or {policy, etag}.
	p, perr := decodePolicy(raw)
	if perr != nil {
		var wrap policyGetResponse
		if err := json.Unmarshal(raw, &wrap); err == nil && len(wrap.Policy) > 0 {
			p, perr = decodePolicy(wrap.Policy)
			if wrap.ETag != "" {
				etag = strings.Trim(wrap.ETag, `"`)
			}
		}
	}
	if perr != nil {
		return nil, etag, fmt.Errorf("decode bucket policy: %w", perr)
	}
	if p == nil {
		p = EmptyPolicy()
	}
	return p, etag, nil
}

func decodePolicy(raw json.RawMessage) (*Policy, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return EmptyPolicy(), nil
	}
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Version == "" && len(p.Statement) == 0 {
		return EmptyPolicy(), nil
	}
	return &p, nil
}

func EmptyPolicy() *Policy {
	return &Policy{Version: "2012-10-17", Statement: []PolicyStatement{}}
}

// PutBucketPolicy writes a policy with If-Match when etag is non-empty.
func (c *Connection) PutBucketPolicy(ctx context.Context, bucket string, policy *Policy, etag string) error {
	err := c.putBucketPolicy(ctx, bucket, policy, etag)
	if err != nil && resourceSpecified(err) {
		stripPolicyResources(policy)
		return c.putBucketPolicy(ctx, bucket, policy, etag)
	}
	return err
}

func (c *Connection) putBucketPolicy(ctx context.Context, bucket string, policy *Policy, etag string) error {
	hdrs := http.Header{}
	if etag != "" {
		hdrs.Set(headerIfMatch, etag)
	}
	q := url.Values{}
	q.Set("allow-remove-self", "true")
	_, err := c.DoJSON(ctx, http.MethodPut, "/v1/s3/buckets/"+url.PathEscape(bucket)+"/policy", q, hdrs, policy, nil)
	return err
}

func resourceSpecified(err error) bool {
	api, ok := AsAPIError(err)
	if !ok {
		return false
	}
	return strings.Contains(strings.ToLower(api.Description), "resourcespecified")
}

func stripPolicyResources(p *Policy) {
	if p == nil {
		return
	}
	for i := range p.Statement {
		p.Statement[i].Resource = nil
	}
}

// MutateBucketPolicy is a read-modify-write loop with ETag conflict retry.
func (c *Connection) MutateBucketPolicy(ctx context.Context, bucket string, mutate func(*Policy) error) error {
	const attempts = 8
	var last error
	for i := 0; i < attempts; i++ {
		p, etag, err := c.GetBucketPolicy(ctx, bucket)
		if err != nil {
			if api, ok := AsAPIError(err); ok && api.IsNotFound() {
				p = EmptyPolicy()
				etag = ""
			} else {
				return err
			}
		}
		if p == nil {
			p = EmptyPolicy()
		}
		if err := mutate(p); err != nil {
			if errors.Is(err, ErrPolicyUnchanged) {
				return nil
			}
			return err
		}
		err = c.PutBucketPolicy(ctx, bucket, p, etag)
		if err == nil {
			return nil
		}
		if api, ok := AsAPIError(err); ok && (api.StatusCode == http.StatusPreconditionFailed || api.ErrorClass == ErrClassRESTPrecondition || api.StatusCode == http.StatusConflict) {
			last = err
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(50*(i+1)) * time.Millisecond):
			}
			continue
		}
		return err
	}
	if last == nil {
		last = fmt.Errorf("policy mutate exhausted retries")
	}
	return last
}

// QumuloPrincipal builds Principal: {"Qumulo": ["auth_id:<id>"]}.
//
// Never reference principals by name ("local:<name>"): Core 7.9.2.2
// resolves the name at PUT time and, after a same-named local user has been
// deleted and recreated, can canonicalize against the REMOVED identity —
// the stored principal then reads "local:" (empty) and matches nobody, so
// every S3 request 403s despite a well-formed grant. auth_id is
// unambiguous. Verified live on 7.9.2.2, 2026-08-17.
func QumuloPrincipal(authID string) json.RawMessage {
	b, _ := json.Marshal(map[string][]string{"Qumulo": {"auth_id:" + authID}})
	return b
}
