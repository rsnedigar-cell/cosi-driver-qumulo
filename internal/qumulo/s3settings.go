package qumulo

import (
	"context"
	"fmt"
	"net/http"
)

type S3Settings struct {
	Enabled  bool   `json:"enabled"`
	BasePath string `json:"base_path"`
	HTTPS    bool   `json:"https,omitempty"`
	Port     int    `json:"port,omitempty"`
}

type VersionInfo struct {
	RevisionID string `json:"revision_id"`
}

func (c *Connection) Version(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.version != "" {
		v := c.version
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	var out VersionInfo
	_, err := c.DoJSON(ctx, http.MethodGet, "/v1/version", nil, nil, nil, &out)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.version = out.RevisionID
	c.mu.Unlock()
	return out.RevisionID, nil
}

func (c *Connection) S3Settings(ctx context.Context) (*S3Settings, error) {
	c.mu.Lock()
	if c.s3settings != nil {
		s := *c.s3settings
		c.mu.Unlock()
		return &s, nil
	}
	c.mu.Unlock()
	var out S3Settings
	_, err := c.DoJSON(ctx, http.MethodGet, "/v1/s3/settings", nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.s3settings = &out
	c.mu.Unlock()
	return &out, nil
}

func (c *Connection) RequireS3Enabled(ctx context.Context) (*S3Settings, error) {
	s, err := c.S3Settings(ctx)
	if err != nil {
		return nil, err
	}
	if !s.Enabled {
		return s, fmt.Errorf("S3 server is disabled on this Qumulo cluster; enable it with `qq s3_modify_settings --enable` before using the COSI driver")
	}
	return s, nil
}

func (c *Connection) EnsureVersion(ctx context.Context, floor string) (string, error) {
	rev, err := c.Version(ctx)
	if err != nil {
		return "", err
	}
	if err := CheckFloor(rev, floor); err != nil {
		return rev, err
	}
	return rev, nil
}
