package qumulo

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
)

const (
	// SnapshotNamePrefix is the driver-owned suffix prefix. Qumulo composes
	// the visible .snapshot name as "<id>_<name_suffix>".
	SnapshotNamePrefix = "csi-"
	snapshotFilterAll  = "all"
)

// Snapshot is GET/POST /v3/snapshots/.
//
// Pending live lock: list responses are modeled as {entries, paging} with a
// required filter query (documented). id is a JSON number.
type Snapshot struct {
	ID           qint    `json:"id"`
	Name         string  `json:"name"`
	Timestamp    string  `json:"timestamp"`
	SourceFileID string  `json:"source_file_id"`
	PolicyID     *qint   `json:"policy_id"`
	Expiration   *string `json:"expiration"`
	InDelete     bool    `json:"in_delete"`
}

func (s Snapshot) IDString() string {
	return strconv.Itoa(int(s.ID))
}

// NameSuffix is the caller-supplied suffix recovered from the visible name.
func (s Snapshot) NameSuffix() string {
	name := s.Name
	if i := strings.Index(name, "_"); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

type snapshotListResponse struct {
	Entries []Snapshot   `json:"entries"`
	Paging  pageMetadata `json:"paging"`
}

type createSnapshotRequest struct {
	NameSuffix   string `json:"name_suffix"`
	SourceFileID string `json:"source_file_id"`
	Expiration   string `json:"expiration"`
}

// CreateSnapshot takes an instantaneous directory snapshot.
func (c *Connection) CreateSnapshot(ctx context.Context, sourceFileID, nameSuffix string) (*Snapshot, error) {
	if strings.TrimSpace(sourceFileID) == "" {
		return nil, fmt.Errorf("snapshot source file id is required")
	}
	if strings.TrimSpace(nameSuffix) == "" {
		return nil, fmt.Errorf("snapshot name suffix is required")
	}
	var out Snapshot
	_, err := c.DoJSON(ctx, http.MethodPost, "/v3/snapshots/", nil, nil, createSnapshotRequest{
		NameSuffix:   nameSuffix,
		SourceFileID: sourceFileID,
		Expiration:   "",
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.ID == 0 {
		return nil, fmt.Errorf("Qumulo returned no snapshot id")
	}
	return &out, nil
}

// GetSnapshot returns one snapshot by numeric id.
func (c *Connection) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("snapshot id is required")
	}
	var out Snapshot
	_, err := c.DoJSON(ctx, http.MethodGet, "/v3/snapshots/"+url.PathEscape(id), nil, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSnapshots returns snapshots, optionally filtered to a source directory.
func (c *Connection) ListSnapshots(ctx context.Context, sourceFileID string) ([]Snapshot, error) {
	var out []Snapshot
	query := url.Values{"filter": []string{snapshotFilterAll}}
	seen := map[string]struct{}{}
	for {
		var page snapshotListResponse
		_, err := c.DoJSON(ctx, http.MethodGet, "/v3/snapshots/", query, nil, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, snap := range page.Entries {
			if sourceFileID != "" && snap.SourceFileID != sourceFileID {
				continue
			}
			out = append(out, snap)
		}
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

// FindSnapshotBySuffix returns a non-deleting snapshot with the given suffix
// on sourceFileID, if present.
func (c *Connection) FindSnapshotBySuffix(ctx context.Context, sourceFileID, nameSuffix string) (*Snapshot, error) {
	all, err := c.ListSnapshots(ctx, sourceFileID)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].InDelete {
			continue
		}
		if all[i].NameSuffix() == nameSuffix {
			return &all[i], nil
		}
	}
	return nil, nil
}

// DeleteSnapshot removes a snapshot. 404 is success.
func (c *Connection) DeleteSnapshot(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("snapshot id is required")
	}
	_, err := c.DoJSON(ctx, http.MethodDelete, "/v3/snapshots/"+url.PathEscape(id), nil, nil, nil, nil)
	if err != nil {
		if api, ok := AsAPIError(err); ok && api.IsNotFound() {
			return nil
		}
		return err
	}
	return nil
}

// HasDriverSnapshots reports whether any driver-owned snapshot still exists
// for the directory (including snapshots that are in the process of deletion).
func (c *Connection) HasDriverSnapshots(ctx context.Context, sourceFileID string) (bool, error) {
	all, err := c.ListSnapshots(ctx, sourceFileID)
	if err != nil {
		return false, err
	}
	for _, snap := range all {
		if strings.HasPrefix(snap.NameSuffix(), SnapshotNamePrefix) {
			return true, nil
		}
	}
	return false, nil
}

const snapshotCopyConcurrency = 4

type snapshotCopyJob struct {
	srcPath string
	dstPath string
	srcID   string
	mode    string
}

// CopySnapshotTree walks a snapshot (skipping .snapshot) and copies files
// server-side into destPath. Directories are created with the recorded mode.
// Restore cost is proportional to data size.
func (c *Connection) CopySnapshotTree(ctx context.Context, sourcePath, sourceDirID, snapshotID, destPath string) error {
	if strings.TrimSpace(snapshotID) == "" {
		return fmt.Errorf("snapshot id is required")
	}
	destPath, err := cleanFSPath(destPath)
	if err != nil {
		return err
	}
	rootRef := sourceDirID
	if rootRef == "" {
		rootRef = sourcePath
	}
	jobs := make(chan snapshotCopyJob, snapshotCopyConcurrency)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < snapshotCopyConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if err := c.copySnapshotFile(ctx, job, snapshotID); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}()
	}

	type walkItem struct {
		srcRef  string
		srcPath string
		dstPath string
	}
	queue := []walkItem{{srcRef: rootRef, srcPath: sourcePath, dstPath: destPath}}
	var copyErr error
loop:
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			copyErr = err
			break
		}
		select {
		case copyErr = <-errCh:
			break loop
		default:
		}
		item := queue[0]
		queue = queue[1:]
		entries, err := c.ListDirectoryEntries(ctx, item.srcRef, snapshotID)
		if err != nil {
			copyErr = err
			break
		}
		for _, entry := range entries {
			if entry.Name == "" || entry.Name == "." || entry.Name == ".." || entry.Name == ".snapshot" {
				continue
			}
			childSrc := entry.Path
			if childSrc == "" {
				childSrc = joinFSPath(item.srcPath, entry.Name)
			}
			childDst := joinFSPath(item.dstPath, entry.Name)
			switch entry.Type {
			case FileTypeDirectory:
				if _, err := c.EnsureDirectory(ctx, childDst, entry.Mode); err != nil {
					copyErr = err
					break loop
				}
				srcRef := entry.ID
				if srcRef == "" {
					srcRef = childSrc
				}
				queue = append(queue, walkItem{srcRef: srcRef, srcPath: childSrc, dstPath: childDst})
			case FileTypeFile:
				select {
				case jobs <- snapshotCopyJob{srcPath: childSrc, dstPath: childDst, srcID: entry.ID, mode: entry.Mode}:
				case copyErr = <-errCh:
					break loop
				case <-ctx.Done():
					copyErr = ctx.Err()
					break loop
				}
			default:
				c.skipSpecial(entry)
			}
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errCh:
		if copyErr == nil {
			copyErr = err
		}
	default:
	}
	return copyErr
}

func (c *Connection) skipSpecial(entry DirectoryEntry) {
	if c.log != nil {
		c.log.Warn("skipping non-file snapshot entry during restore",
			"name", entry.Name, "type", entry.Type, slog.String("path", entry.Path))
	}
}

func (c *Connection) copySnapshotFile(ctx context.Context, job snapshotCopyJob, snapshotID string) error {
	parent, name := path.Split(strings.TrimSuffix(job.dstPath, "/"))
	parent = strings.TrimSuffix(parent, "/")
	if parent == "" {
		parent = "/"
	}
	created, err := c.CreateFile(ctx, parent, name)
	if err != nil {
		if api, ok := AsAPIError(err); ok && api.IsClass(ErrClassFSEntryExists) {
			created, err = c.FileAttributes(ctx, job.dstPath)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	destRef := job.dstPath
	if created != nil && created.ID != "" {
		destRef = created.ID
	}
	if job.mode != "" && created != nil {
		_ = c.PatchFileMode(ctx, job.dstPath, job.mode)
	}
	src := job.srcPath
	if src == "" {
		src = job.srcID
	}
	return c.CopyFileFromSnapshot(ctx, destRef, src, snapshotID)
}
