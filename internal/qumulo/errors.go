package qumulo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Known error_class values. Integration tests against a live cluster should
// lock these; the unit fake uses the same constants.
const (
	ErrClassFSEntryExists       = "fs_entry_exists_error"
	ErrClassFSNoSuchEntry       = "fs_no_such_entry_error"
	ErrClassFSNotEmpty          = "fs_not_empty_error"
	ErrClassFSInvalidName       = "fs_invalid_name_error"
	ErrClassS3BucketExists      = "s3_bucket_already_exists_error"
	ErrClassS3BucketNotFound    = "s3_bucket_not_found_error"
	ErrClassS3UploadsInProgress = "s3_uploads_in_progress_error"
	ErrClassS3KeyLimit          = "s3_access_key_limit_reached_error"
	ErrClassAuthNoSuchUser      = "auth_no_such_user_error"
	ErrClassAuthUserExists      = "auth_user_already_exists_error"
	ErrClassAuthInvalidCreds    = "auth_invalid_credentials_error"
	ErrClassAuthNotAuthorized   = "auth_not_authorized_error"
	ErrClassRESTNotFound        = "rest_not_found_error"
	ErrClassRESTInvalidRequest  = "rest_invalid_request_error"
	ErrClassRESTPrecondition    = "rest_precondition_failed_error"
	ErrClassQuotaExists         = "fs_quota_already_exists_error"
)

// ErrResourceClaimConflict means an idempotent create found a same-name
// protocol resource owned by a different immutable specification. Callers
// must not patch that resource into the new request's shape.
var ErrResourceClaimConflict = errors.New("protocol resource is claimed by a different specification")

// APIError is a parsed Qumulo REST error envelope.
type APIError struct {
	StatusCode  int    `json:"-"`
	Description string `json:"description"`
	Module      string `json:"module"`
	ErrorClass  string `json:"error_class"`
	UserVisible bool   `json:"user_visible"`
	Stack       any    `json:"stack,omitempty"`
}

func (e *APIError) Error() string {
	if e == nil {
		return "qumulo: <nil>"
	}
	cls := e.ErrorClass
	if cls == "" {
		cls = "unknown"
	}
	if e.Description != "" {
		return fmt.Sprintf("qumulo %s (%d): %s", cls, e.StatusCode, e.Description)
	}
	return fmt.Sprintf("qumulo %s (%d)", cls, e.StatusCode)
}

func (e *APIError) IsClass(class string) bool {
	return e != nil && e.ErrorClass == class
}

func (e *APIError) IsNotFound() bool {
	if e == nil {
		return false
	}
	if e.StatusCode == http.StatusNotFound {
		return true
	}
	switch e.ErrorClass {
	case ErrClassFSNoSuchEntry, ErrClassS3BucketNotFound, ErrClassAuthNoSuchUser, ErrClassRESTNotFound:
		return true
	default:
		return false
	}
}

func (e *APIError) IsAlreadyExists() bool {
	if e == nil || e.IsNotEmpty() {
		return false
	}
	switch e.ErrorClass {
	case ErrClassFSEntryExists, ErrClassS3BucketExists, ErrClassAuthUserExists, ErrClassQuotaExists:
		return true
	default:
		return e.StatusCode == http.StatusConflict
	}
}

func (e *APIError) IsAuth() bool {
	if e == nil {
		return false
	}
	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		return true
	}
	switch e.ErrorClass {
	case ErrClassAuthInvalidCreds, ErrClassAuthNotAuthorized:
		return true
	default:
		return false
	}
}

func (e *APIError) IsNotEmpty() bool {
	if e == nil {
		return false
	}
	if e.ErrorClass == ErrClassFSNotEmpty || e.ErrorClass == ErrClassS3UploadsInProgress {
		return true
	}
	d := strings.ToLower(e.Description)
	return strings.Contains(d, "bucketnotempty") || strings.Contains(d, "not empty")
}

// AsAPIError unwraps an error into *APIError.
func AsAPIError(err error) (*APIError, bool) {
	var api *APIError
	if errors.As(err, &api) {
		return api, true
	}
	return nil, false
}

func parseAPIError(status int, body []byte) *APIError {
	api := &APIError{StatusCode: status}
	if len(body) > 0 {
		if err := json.Unmarshal(body, api); err != nil {
			api.Description = strings.TrimSpace(string(body))
		}
	}
	if api.ErrorClass == "" {
		switch status {
		case http.StatusNotFound:
			api.ErrorClass = ErrClassRESTNotFound
		case http.StatusUnauthorized, http.StatusForbidden:
			api.ErrorClass = ErrClassAuthNotAuthorized
		case http.StatusConflict:
			api.ErrorClass = ErrClassFSEntryExists
		case http.StatusPreconditionFailed:
			api.ErrorClass = ErrClassRESTPrecondition
		case http.StatusBadRequest:
			api.ErrorClass = ErrClassRESTInvalidRequest
		}
	}
	return api
}

func readBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b
}
