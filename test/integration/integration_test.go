//go:build integration

package integration

import (
	"context"
	"os"
	"testing"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

// Live-cluster lock for error_class constants. Run with:
//
//	QUMULO_HOST=https://cluster:8000 QUMULO_TOKEN=... go test -tags=integration ./test/integration
func TestLiveErrorClasses(t *testing.T) {
	host := os.Getenv("QUMULO_HOST")
	tok := os.Getenv("QUMULO_TOKEN")
	if host == "" || tok == "" {
		t.Skip("QUMULO_HOST / QUMULO_TOKEN not set")
	}
	insecure := os.Getenv("QUMULO_INSECURE_SKIP_TLS_VERIFY") == "true"
	conn, err := qumulo.NewConnection(qumulo.DialConfig{
		Endpoint:    host,
		Credentials: qumulo.Credentials{Token: tok},
		TLS:         qumulo.TLSConfig{InsecureSkipVerify: insecure},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rev, err := conn.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cluster version %s", rev)

	_, err = conn.GetBucket(ctx, "cosi-definitely-missing-bucket-xyz")
	api, ok := qumulo.AsAPIError(err)
	if !ok {
		t.Fatalf("expected APIError, got %T %v", err, err)
	}
	t.Logf("missing bucket error_class=%q status=%d desc=%q", api.ErrorClass, api.StatusCode, api.Description)
	switch api.ErrorClass {
	case qumulo.ErrClassS3BucketNotFound, qumulo.ErrClassFSNoSuchEntry, qumulo.ErrClassRESTNotFound:
		// locked
	default:
		t.Errorf("unexpected missing-bucket error_class %q — update constants", api.ErrorClass)
	}
}
