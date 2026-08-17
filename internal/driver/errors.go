package driver

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if api, ok := qumulo.AsAPIError(err); ok {
		switch {
		case api.IsAuth():
			return status.Errorf(codes.Unauthenticated, "Qumulo auth failed (%s): %s; check the driver Secret and the service-account role", api.ErrorClass, api.Description)
		case api.IsAlreadyExists():
			return status.Errorf(codes.AlreadyExists, "%s", api.Error())
		case api.IsNotEmpty():
			return status.Errorf(codes.FailedPrecondition, "%s", api.Error())
		case api.StatusCode >= 500 || api.StatusCode == 429:
			return status.Errorf(codes.Unavailable, "%s", api.Error())
		case api.StatusCode == 400:
			return status.Errorf(codes.InvalidArgument, "%s", api.Error())
		default:
			return status.Errorf(codes.Internal, "%s", api.Error())
		}
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, msg)
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, msg)
	case strings.Contains(msg, "S3 server is disabled"):
		return status.Error(codes.FailedPrecondition, msg)
	case strings.Contains(msg, "below the driver floor"):
		return status.Error(codes.FailedPrecondition, msg)
	case strings.Contains(msg, "requires Qumulo Core"):
		return status.Error(codes.InvalidArgument, msg)
	case strings.Contains(msg, "unknown accessMode"),
		strings.Contains(msg, "quotaLimit"),
		strings.Contains(msg, "versioning"),
		strings.Contains(msg, "bucket name"),
		strings.Contains(msg, "basePath"),
		strings.Contains(msg, "restPort"),
		strings.Contains(msg, "s3Port"),
		strings.Contains(msg, "region"),
		strings.Contains(msg, "unknown class parameter"),
		strings.Contains(msg, "is not a valid boolean"):
		return status.Error(codes.InvalidArgument, msg)
	case strings.Contains(strings.ToLower(msg), "connection refused"),
		strings.Contains(strings.ToLower(msg), "timeout"),
		strings.Contains(strings.ToLower(msg), "no such host"),
		strings.Contains(msg, "qumulo request"):
		return status.Error(codes.Unavailable, msg)
	default:
		return status.Error(codes.Internal, msg)
	}
}
