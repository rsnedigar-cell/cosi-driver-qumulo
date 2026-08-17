package qumulo

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
)

type createDirectoryRequest struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

// CreateDirectory creates one directory directly below parent. Callers that
// need parent creation should use EnsureDirectory.
func (c *Connection) CreateDirectory(ctx context.Context, parent, name string) (*FileAttributes, error) {
	if err := validateDirectoryName(name); err != nil {
		return nil, err
	}
	var out FileAttributes
	_, err := c.DoJSON(ctx, http.MethodPost, filePath(parent, "/entries/"), nil, nil, createDirectoryRequest{
		Name:   name,
		Action: "CREATE_DIRECTORY",
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.Path == "" {
		out.Path = joinFSPath(parent, name)
	}
	if out.Name == "" {
		out.Name = name
	}
	return &out, nil
}

// EnsureDirectory creates every missing component of an absolute filesystem
// path. It reconciles an ambiguous create by reading the component again, so
// callers can safely retry even though the underlying create API is a POST.
// When mode is non-empty, the final directory is reconciled to that mode.
func (c *Connection) EnsureDirectory(ctx context.Context, fsPath, mode string) (*FileAttributes, error) {
	clean, err := cleanFSPath(fsPath)
	if err != nil {
		return nil, err
	}
	if clean == "/" {
		attrs, err := c.FileAttributes(ctx, clean)
		if err != nil {
			return nil, err
		}
		return attrs, nil
	}

	current := "/"
	var attrs *FileAttributes
	for _, component := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		next := joinFSPath(current, component)
		attrs, err = c.FileAttributes(ctx, next)
		if err == nil {
			if err := requireDirectory(attrs, next); err != nil {
				return nil, err
			}
			current = next
			continue
		}
		api, ok := AsAPIError(err)
		if !ok || !api.IsNotFound() {
			return nil, fmt.Errorf("inspect directory %s: %w", next, err)
		}

		attrs, err = c.CreateDirectory(ctx, current, component)
		if err != nil {
			// A concurrent creator, or a committed request whose response was
			// lost, is reconciled by an authoritative read.
			created, getErr := c.FileAttributes(ctx, next)
			if getErr != nil {
				return nil, fmt.Errorf("create directory %s: %w", next, err)
			}
			attrs = created
		}
		if err := requireDirectory(attrs, next); err != nil {
			return nil, err
		}
		current = next
	}

	if attrs == nil {
		return nil, fmt.Errorf("ensure directory %s returned no attributes", clean)
	}
	if mode != "" && attrs.Mode != mode {
		if err := c.PatchFileMode(ctx, clean, mode); err != nil {
			return nil, fmt.Errorf("set directory mode on %s: %w", clean, err)
		}
		attrs.Mode = mode
	}
	return attrs, nil
}

func requireDirectory(attrs *FileAttributes, fsPath string) error {
	if attrs == nil || attrs.ID == "" {
		return fmt.Errorf("filesystem path %s returned no stable identity", fsPath)
	}
	if attrs.Type != "" && attrs.Type != "FS_FILE_TYPE_DIRECTORY" {
		return fmt.Errorf("filesystem path %s is not a directory", fsPath)
	}
	return nil
}

func cleanFSPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("filesystem path %q must be absolute", raw)
	}
	if strings.ContainsAny(raw, "\x00\r\n") {
		return "", fmt.Errorf("filesystem path contains a control character")
	}
	clean := path.Clean(raw)
	if clean == "." || !strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("invalid filesystem path %q", raw)
	}
	return clean, nil
}

func validateDirectoryName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00\r\n") {
		return fmt.Errorf("invalid directory name %q", name)
	}
	return nil
}

func joinFSPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return strings.TrimRight(parent, "/") + "/" + name
}
