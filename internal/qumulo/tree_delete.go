package qumulo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	treeDeleteJobsPath     = "/v1/tree-delete/jobs/"
	treeDeletePollInterval = time.Second
)

type treeDeleteJob struct {
	ID string `json:"id"`
}

// treeDeleteJobStatus models both the documented Core response and terminal
// fields returned by Core releases that explicitly include a job status.
// The documented last_error_message is null while the job is healthy and
// non-null when an error caused it to abort.
type treeDeleteJobStatus struct {
	ID               string  `json:"id"`
	Status           string  `json:"status,omitempty"`
	LastErrorMessage *string `json:"last_error_message"`
	Aborted          bool    `json:"aborted,omitempty"`
}

// TreeDelete submits a recursive-delete job and does not return success until
// Core reports that the job is gone. Per the Qumulo API, GET returning 404 is
// the durable completion signal; a successful POST only means the job was
// accepted.
func (c *Connection) TreeDelete(ctx context.Context, fileID string) error {
	return c.treeDelete(ctx, fileID, treeDeletePollInterval)
}

func (c *Connection) treeDelete(ctx context.Context, fileID string, pollInterval time.Duration) error {
	if fileID == "" {
		return fmt.Errorf("tree-delete file ID is required")
	}
	if pollInterval <= 0 {
		pollInterval = treeDeletePollInterval
	}

	// Resume an already-running job before attempting to create another one.
	// A 404 is ambiguous before submission (the job either never existed or
	// already finished), so it only tells us that a new POST is required.
	status, found, err := c.getTreeDeleteJob(ctx, fileID)
	if err != nil {
		return fmt.Errorf("inspect tree-delete job for file ID %q: %w", fileID, err)
	}
	if found {
		return c.waitForTreeDelete(ctx, fileID, status, pollInterval)
	}

	_, err = c.DoJSON(ctx, http.MethodPost, treeDeleteJobsPath, nil, nil, treeDeleteJob{ID: fileID}, nil)
	if err == nil {
		return c.waitForTreeDelete(ctx, fileID, nil, pollInterval)
	}

	if api, ok := AsAPIError(err); ok {
		if api.IsNotFound() {
			// The target disappeared between the preflight GET and POST.
			return nil
		}
		if api.IsAlreadyExists() {
			// A concurrent or prior caller already submitted this file ID.
			// We still have to observe that job's durable completion.
			return c.waitForTreeDelete(ctx, fileID, nil, pollInterval)
		}
	}
	if !isAmbiguousCreateError(err) {
		return err
	}

	// POST transport errors and 5xx responses can arrive after Core committed
	// the request. Continue only if the job is observable; a 404 here cannot
	// distinguish a very fast completion from a request that never committed,
	// so returning the original error lets the higher-level operation retry
	// without falsely claiming that data was deleted.
	status, found, reconcileErr := c.getTreeDeleteJob(ctx, fileID)
	if reconcileErr != nil {
		return fmt.Errorf("reconcile ambiguous tree-delete submission for file ID %q: %w (submit error: %v)", fileID, reconcileErr, err)
	}
	if !found {
		return fmt.Errorf("tree-delete submission for file ID %q has unknown outcome: %w", fileID, err)
	}
	return c.waitForTreeDelete(ctx, fileID, status, pollInterval)
}

func (c *Connection) getTreeDeleteJob(ctx context.Context, fileID string) (*treeDeleteJobStatus, bool, error) {
	var out treeDeleteJobStatus
	_, err := c.DoJSON(ctx, http.MethodGet, treeDeleteJobsPath+url.PathEscape(fileID), nil, nil, nil, &out)
	if err != nil {
		if api, ok := AsAPIError(err); ok && api.IsNotFound() {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &out, true, nil
}

func (c *Connection) waitForTreeDelete(ctx context.Context, fileID string, initial *treeDeleteJobStatus, pollInterval time.Duration) error {
	status := initial
	for {
		if status == nil {
			var found bool
			var err error
			status, found, err = c.getTreeDeleteJob(ctx, fileID)
			if err != nil {
				return fmt.Errorf("poll tree-delete job for file ID %q: %w", fileID, err)
			}
			if !found {
				return nil
			}
		}

		if err := treeDeleteTerminalError(fileID, status); err != nil {
			return err
		}
		status = nil

		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func treeDeleteTerminalError(fileID string, status *treeDeleteJobStatus) error {
	if status.LastErrorMessage != nil {
		message := strings.TrimSpace(*status.LastErrorMessage)
		if message == "" {
			message = "Core did not provide an error message"
		}
		return fmt.Errorf("tree-delete job for file ID %q aborted: %s", fileID, message)
	}
	if status.Aborted || strings.EqualFold(strings.TrimSpace(status.Status), "aborted") {
		return fmt.Errorf("tree-delete job for file ID %q aborted", fileID)
	}
	return nil
}
