package qumulo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

type Quota struct {
	ID    string `json:"id"`
	Limit int64  `json:"limit"`
	ETag  string `json:"-"`
}

// MarshalJSON uses the decimal-string representation documented by the
// Qumulo quota API. Quota sizes can exceed JavaScript's exactly representable
// integer range, so Core intentionally carries them as strings on the wire.
func (q Quota) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID    string `json:"id"`
		Limit string `json:"limit"`
	}{ID: q.ID, Limit: strconv.FormatInt(q.Limit, 10)})
}

// UnmarshalJSON accepts the documented string representation and the numeric
// representation returned by some older Core releases and test doubles.
func (q *Quota) UnmarshalJSON(raw []byte) error {
	var wire struct {
		ID    string          `json:"id"`
		Limit json.RawMessage `json:"limit"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	var value string
	if err := json.Unmarshal(wire.Limit, &value); err != nil {
		var number json.Number
		if numberErr := json.Unmarshal(wire.Limit, &number); numberErr != nil {
			return fmt.Errorf("decode quota limit: %w", err)
		}
		value = number.String()
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit < 0 {
		return fmt.Errorf("decode quota limit %q", value)
	}
	q.ID, q.Limit = wire.ID, limit
	return nil
}

func (c *Connection) CreateQuota(ctx context.Context, fileID string, limit int64) error {
	if fileID == "" || limit < 0 {
		return fmt.Errorf("quota requires a file ID and non-negative limit")
	}
	_, err := c.DoJSON(ctx, http.MethodPost, "/v1/files/quotas/", nil, nil, Quota{ID: fileID, Limit: limit}, nil)
	if err != nil {
		if api, ok := AsAPIError(err); ok && api.IsAlreadyExists() || isAmbiguousCreateError(err) {
			current, getErr := c.GetQuota(ctx, fileID)
			if getErr != nil {
				return fmt.Errorf("reconcile quota create: %w (create error: %v)", getErr, err)
			}
			if current.Limit == limit {
				return nil
			}
			return c.updateQuota(ctx, fileID, limit, current.ETag)
		}
		return err
	}
	return nil
}

func (c *Connection) GetQuota(ctx context.Context, fileID string) (*Quota, error) {
	if fileID == "" {
		return nil, fmt.Errorf("quota file ID is required")
	}
	var out Quota
	headers, err := c.DoJSON(ctx, http.MethodGet, "/v1/files/quotas/"+url.PathEscape(fileID), nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	out.ETag = headers.Get(headerETag)
	return &out, nil
}

func (c *Connection) UpdateQuota(ctx context.Context, fileID string, limit int64) error {
	return c.updateQuota(ctx, fileID, limit, "")
}

func (c *Connection) updateQuota(ctx context.Context, fileID string, limit int64, ifMatch string) error {
	if fileID == "" || limit < 0 {
		return fmt.Errorf("quota requires a file ID and non-negative limit")
	}
	_, err := c.DoJSON(ctx, http.MethodPut, "/v1/files/quotas/"+url.PathEscape(fileID), nil, ifMatchHeader(ifMatch), Quota{ID: fileID, Limit: limit}, nil)
	return err
}

// EnsureQuotaAtLeast creates or grows a quota but never shrinks it. CSI
// CreateVolume retries and ControllerExpandVolume can overlap, so preserving
// the greatest committed limit is required for safe idempotency.
func (c *Connection) EnsureQuotaAtLeast(ctx context.Context, fileID string, limit int64) (int64, error) {
	if fileID == "" || limit < 0 {
		return 0, fmt.Errorf("quota requires a file ID and non-negative limit")
	}
	for attempt := 0; attempt < 4; attempt++ {
		current, err := c.GetQuota(ctx, fileID)
		if err != nil {
			if api, ok := AsAPIError(err); !ok || !api.IsNotFound() {
				return 0, err
			}
			_, createErr := c.DoJSON(ctx, http.MethodPost, "/v1/files/quotas/", nil, nil, Quota{ID: fileID, Limit: limit}, nil)
			if createErr == nil {
				return limit, nil
			}
			if api, ok := AsAPIError(createErr); ok && api.IsAlreadyExists() || isAmbiguousCreateError(createErr) {
				continue
			}
			return 0, createErr
		}
		if current.Limit >= limit {
			return current.Limit, nil
		}
		if err := c.updateQuota(ctx, fileID, limit, current.ETag); err != nil {
			if isAPIPreconditionFailed(err) {
				continue
			}
			return 0, err
		}
		return limit, nil
	}
	return 0, fmt.Errorf("ensure quota for %q: concurrent modifications did not settle", fileID)
}
