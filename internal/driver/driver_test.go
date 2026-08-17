package driver_test

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cosi "sigs.k8s.io/container-object-storage-interface/proto"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/config"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/driver"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/naming"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
	fakequmulo "github.com/rsnedigar-cell/cosi-driver-qumulo/test/fake_qumulo"
)

type staticSecrets map[string][]byte

func (s staticSecrets) GetSecret(context.Context, string, string) (map[string][]byte, error) {
	return s, nil
}

func harness(t *testing.T) (*driver.Driver, *fakequmulo.Server, map[string]string) {
	t.Helper()
	fq := fakequmulo.New()
	t.Cleanup(fq.Close)
	cfg := config.Driver{
		Name:            naming.DriverName,
		DefaultEndpoint: fq.URL,
		DefaultRESTPort: "",
		DefaultS3Port:   "9000",
		DefaultRegion:   "us-east-1",
		DefaultBasePath: "/k8s-buckets",
		VersionFloor:    "7.2.0",
		DriverNamespace: "qumulo-cosi",
	}
	sec := staticSecrets{"token": []byte(fq.Token)}
	d := driver.New(cfg, sec, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), driver.NewTestMetrics())
	params := map[string]string{
		"endpoint":                   fq.URL,
		"restPort":                   "",
		"credentialsSecretName":      "creds",
		"credentialsSecretNamespace": "qumulo-cosi",
		"insecureSkipTLSVerify":      "true",
		"basePath":                   "/k8s-buckets",
		"region":                     "us-east-1",
	}
	// httptest TLS server uses a random port in the URL; restPort empty keeps it.
	return d, fq, params
}

func TestIdentity(t *testing.T) {
	d, _, _ := harness(t)
	resp, err := d.DriverGetInfo(context.Background(), &cosi.DriverGetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Name != naming.DriverName {
		t.Fatalf("got %q", resp.Name)
	}
}

func TestCreateDeleteBucket_Idempotent(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	req := &cosi.DriverCreateBucketRequest{Name: "bc-photos-claim", Parameters: params}
	r1, err := d.DriverCreateBucket(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.BucketId == "" || r1.BucketInfo.GetS3().GetSignatureVersion() != cosi.S3SignatureVersion_S3V4 {
		t.Fatalf("%+v", r1)
	}
	r2, err := d.DriverCreateBucket(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.BucketId != r2.BucketId {
		t.Fatalf("idempotent mismatch %s vs %s", r1.BucketId, r2.BucketId)
	}
	if _, err := naming.ParseBucketID(r1.BucketId); err != nil {
		t.Fatal(err)
	}
	if len(fq.Buckets) != 1 {
		t.Fatalf("buckets=%d", len(fq.Buckets))
	}

	_, err = d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: r1.BucketId, DeleteContext: params})
	if err != nil {
		t.Fatal(err)
	}
	// second delete is success
	if _, err := d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: r1.BucketId, DeleteContext: params}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBucket_BucketPrefixApplied(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	params["bucketPrefix"] = "team-"
	resp, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-aabbcc", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, err := naming.ParseBucketID(resp.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id.BucketName, "team-") {
		t.Fatalf("bucketPrefix not applied: %q", id.BucketName)
	}
	if _, ok := fq.Buckets[id.BucketName]; !ok {
		t.Fatalf("prefixed bucket missing: %v", fq.Buckets)
	}
	// Idempotent retry with the same prefix resolves to the same bucket.
	again, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-aabbcc", Parameters: params})
	if err != nil || again.BucketId != resp.BucketId {
		t.Fatalf("prefixed create not idempotent: %v %q vs %q", err, again.GetBucketId(), resp.BucketId)
	}
}

func TestDelete_NotEmpty(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-full", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := naming.ParseBucketID(created.BucketId)
	fq.MarkNotEmpty(id.BucketName)
	_, err = d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: created.BucketId, DeleteContext: params})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v", err)
	}
}

func TestDelete_AbortUploadsThenSucceed(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-mpu", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := naming.ParseBucketID(created.BucketId)
	fq.SeedUpload(id.BucketName, "upload-1")
	if _, err := d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: created.BucketId, DeleteContext: params}); err != nil {
		t.Fatal(err)
	}
}

func TestDelete_RefusesReusedRootIdentity(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-reused-root", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, err := naming.ParseBucketID(created.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the original filesystem object being removed and its path
	// reused while a stale COSI delete is still retrying.
	fq.Files[id.RootPath] = "replacement-file-id"
	_, err = d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: created.BucketId, DeleteContext: params})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for reused root, got %v", err)
	}
	if _, ok := fq.Buckets[id.BucketName]; !ok {
		t.Fatal("identity mismatch deleted the replacement bucket")
	}
	_, err = d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
		BucketId: created.BucketId, Name: "stale-access", AuthenticationType: cosi.AuthenticationType_Key, Parameters: params,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for grant to reused root, got %v", err)
	}
	if len(fq.Users) != 0 {
		t.Fatal("identity mismatch created a user before rejecting stale grant")
	}
}

func TestDelete_RejectsClassEndpointMismatch(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-endpoint-binding", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	wrong := make(map[string]string, len(params))
	for k, v := range params {
		wrong[k] = v
	}
	wrong["endpoint"] = "other-cluster.example"
	_, err = d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: created.BucketId, DeleteContext: wrong})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for endpoint mismatch, got %v", err)
	}
	if len(fq.Buckets) != 1 {
		t.Fatal("endpoint mismatch mutated the original cluster")
	}
}

func TestGrantRevoke_SecretKeysAndIdempotentRevoke(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-grant", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	params["accessMode"] = "rw"
	g1, err := d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
		BucketId:           created.BucketId,
		Name:               "photos-access",
		AuthenticationType: cosi.AuthenticationType_Key,
		Parameters:         params,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := g1.Credentials[naming.S3ProtocolKey].Secrets
	for _, k := range []string{naming.SecretAccessKeyID, naming.SecretAccessSecretKey, naming.SecretEndpoint, naming.SecretRegion} {
		if sec[k] == "" {
			t.Fatalf("missing secret %s: %#v", k, sec)
		}
	}
	if sec[naming.SecretRegion] != "us-east-1" {
		t.Fatalf("region %q", sec[naming.SecretRegion])
	}
	if _, err := naming.ParseAccountID(g1.AccountId); err != nil {
		t.Fatal(err)
	}
	// retry grant rotates keys and still returns a secret
	g2, err := d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
		BucketId:           created.BucketId,
		Name:               "photos-access",
		AuthenticationType: cosi.AuthenticationType_Key,
		Parameters:         params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g2.Credentials[naming.S3ProtocolKey].Secrets[naming.SecretAccessSecretKey] == "" {
		t.Fatal("rotated grant missing secret")
	}

	// policy has a statement, no Resource, and an auth_id principal (7.9.2.2)
	var stmts int
	for _, b := range fq.Buckets {
		if b.Policy != nil {
			stmts = len(b.Policy.Statement)
			for _, st := range b.Policy.Statement {
				if st.Resource != nil {
					t.Fatalf("policy Resource must be omitted: %#v", st.Resource)
				}
				if !strings.Contains(string(st.Principal), "auth_id:") {
					t.Fatalf("policy principal must use auth_id form: %s", st.Principal)
				}
			}
		}
	}
	if stmts != 1 {
		t.Fatalf("policy statements=%d", stmts)
	}
	foundUser := false
	for _, u := range fq.Users {
		foundUser = true
		if u.PrimaryGroup != "513" {
			t.Fatalf("primary_group=%q want 513", u.PrimaryGroup)
		}
	}
	if !foundUser {
		t.Fatal("expected driver-created user")
	}
	forged, err := naming.ParseAccountID(g1.AccountId)
	if err != nil {
		t.Fatal(err)
	}
	forged.Endpoint = "other-cluster.example"
	if _, err := d.DriverRevokeBucketAccess(ctx, &cosi.DriverRevokeBucketAccessRequest{
		BucketId:            created.BucketId,
		AccountId:           forged.String(),
		RevokeAccessContext: params,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("cross-cluster account id was not rejected: %v", err)
	}
	if len(fq.Users) != 1 {
		t.Fatal("rejected cross-cluster revoke mutated the target cluster")
	}

	if _, err := d.DriverRevokeBucketAccess(ctx, &cosi.DriverRevokeBucketAccessRequest{
		BucketId:            created.BucketId,
		AccountId:           g1.AccountId,
		RevokeAccessContext: params,
	}); err != nil {
		t.Fatal(err)
	}
	// revoke again is success
	if _, err := d.DriverRevokeBucketAccess(ctx, &cosi.DriverRevokeBucketAccessRequest{
		BucketId:            created.BucketId,
		AccountId:           g1.AccountId,
		RevokeAccessContext: params,
	}); err != nil {
		t.Fatal(err)
	}
	if len(fq.Users) != 0 {
		t.Fatalf("user leftover: %+v", fq.Users)
	}
}

func TestGrant_IAMRejected(t *testing.T) {
	d, _, params := harness(t)
	_, err := d.DriverGrantBucketAccess(context.Background(), &cosi.DriverGrantBucketAccessRequest{
		BucketId:           "q1:qumulo:h:8000:bkt",
		Name:               "a",
		AuthenticationType: cosi.AuthenticationType_IAM,
		Parameters:         params,
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("got %v", err)
	}
}

func TestGrant_PolicyETagRetry(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-etag", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	fq.PolicyConflictN.Store(2)
	if _, err := d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
		BucketId:           created.BucketId,
		Name:               "acc",
		AuthenticationType: cosi.AuthenticationType_Key,
		Parameters:         params,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCreate_StaticImportBindsUnmanagedHandle(t *testing.T) {
	d, fq, params := harness(t)
	// pre-create via REST
	conn, err := qumulo.NewConnection(qumulo.DialConfig{
		Endpoint:    fq.URL,
		Credentials: qumulo.Credentials{Token: fq.Token},
		TLS:         qumulo.TLSConfig{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	trueVal := true
	if _, err := conn.CreateBucket(context.Background(), qumulo.CreateBucketRequest{Name: "legacy-photos", Path: "/legacy/photos", CreateFSPath: &trueVal}); err != nil {
		t.Fatal(err)
	}
	params["existingBucketName"] = "legacy-photos"
	resp, err := d.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{Name: "ignored", Parameters: params})
	if err != nil {
		t.Fatalf("brownfield import failed: %v", err)
	}
	id, err := naming.ParseBucketID(resp.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	if id.BucketName != "legacy-photos" || id.RootPath != "/legacy/photos" || id.RootFileID == "" {
		t.Fatalf("import handle incomplete: %+v", id)
	}
	if id.Managed {
		t.Fatal("imported bucket handle must be unmanaged so deletes retain data")
	}
	if _, ok := fq.Buckets["legacy-photos"]; !ok || fq.Files["/legacy/photos"] == "" {
		t.Fatal("import mutated the brownfield bucket or data")
	}
	// Deleting the claim unregisters the bucket but always keeps the data,
	// even when the class requests purge.
	params["purgeOnDelete"] = "true"
	if _, err := d.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{BucketId: resp.BucketId, DeleteContext: params}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fq.Buckets["legacy-photos"]; ok {
		t.Fatal("imported bucket was not unregistered")
	}
	if fq.Files["/legacy/photos"] == "" {
		t.Fatal("imported bucket data was deleted")
	}
	if len(fq.TreeJobs) != 0 {
		t.Fatalf("purge must not tree-delete an imported root: %v", fq.TreeJobs)
	}
}

func TestDeletePreservesUnmarkedBrownfieldRootByDefault(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	conn, err := qumulo.NewConnection(qumulo.DialConfig{Endpoint: fq.URL, Credentials: qumulo.Credentials{Token: fq.Token}, TLS: qumulo.TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	createFS := true
	if _, err := conn.CreateBucket(ctx, qumulo.CreateBucketRequest{Name: "legacy-retain", Path: "/legacy/retain", CreateFSPath: &createFS}); err != nil {
		t.Fatal(err)
	}
	attrs, err := conn.FileAttributes(ctx, "/legacy/retain")
	if err != nil {
		t.Fatal(err)
	}
	id := naming.BucketID{Endpoint: conn.EndpointHost(), RESTPort: conn.RESTPort(), BucketName: "legacy-retain", RootPath: "/legacy/retain", RootFileID: attrs.ID}
	if _, err := d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: id.String(), DeleteContext: params}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fq.Buckets["legacy-retain"]; ok {
		t.Fatal("brownfield bucket was not unregistered")
	}
	if fq.Files["/legacy/retain"] != attrs.ID {
		t.Fatal("brownfield root was deleted by the default deleteRootDir setting")
	}
}

func TestCreate_QuotaAndVersioning(t *testing.T) {
	d, fq, params := harness(t)
	params["quotaLimit"] = "1073741824"
	params["versioning"] = "Enabled"
	resp, err := d.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{Name: "bc-gold", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := naming.ParseBucketID(resp.BucketId)
	b := fq.Buckets[id.BucketName]
	if b.Versioning != "Enabled" {
		t.Fatalf("versioning=%q", b.Versioning)
	}
	if len(fq.Quota) == 0 {
		t.Fatal("quota not set")
	}
}

func TestCreate_ObjectLockRequiresCore(t *testing.T) {
	d, fq, params := harness(t)
	fq.Version = "Qumulo Core 7.0.0"
	// lower the floor so we exercise the feature gate, not the global floor
	d = driver.New(config.Driver{
		Name:            naming.DriverName,
		DefaultEndpoint: fq.URL,
		DefaultRegion:   "us-east-1",
		DefaultBasePath: "/k8s-buckets",
		VersionFloor:    "5.3.3",
		DriverNamespace: "qumulo-cosi",
	}, staticSecrets{"token": []byte(fq.Token)}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), driver.NewTestMetrics())
	params["objectLockEnabled"] = "true"
	_, err := d.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{Name: "bc-lock", Parameters: params})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v", err)
	}
}

func TestS3Disabled(t *testing.T) {
	d, fq, params := harness(t)
	fq.S3Enabled = false
	_, err := d.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{Name: "bc-x", Parameters: params})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v", err)
	}
}

func TestPurgeOnDelete(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-purge", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	params["purgeOnDelete"] = "true"
	if _, err := d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: created.BucketId, DeleteContext: params}); err != nil {
		t.Fatal(err)
	}
	if len(fq.TreeJobs) == 0 {
		t.Fatal("expected tree-delete job")
	}
}

// Purge must tree-delete the bucket's REAL root (resolved from the cluster),
// not a path reconstructed from basePath+name — for an imported bucket those
// differ, and the reconstruction could target unrelated data.
// A purge through an unmanaged (non-q3) handle cannot prove the driver owns
// the root, so it downgrades to unregister-with-retention and converges —
// even for a bucket rooted at the filesystem root, the most dangerous case.
func TestPurgeUnmanagedHandleRetainsData(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "brownfield-root", path: "/legacy/data"},
		{name: "filesystem-root", path: "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, fq, params := harness(t)
			ctx := context.Background()
			conn, err := qumulo.NewConnection(qumulo.DialConfig{
				Endpoint:    fq.URL,
				Credentials: qumulo.Credentials{Token: fq.Token},
				TLS:         qumulo.TLSConfig{InsecureSkipVerify: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			trueVal := true
			bucket := "unmanaged-" + tc.name
			if _, err := conn.CreateBucket(ctx, qumulo.CreateBucketRequest{Name: bucket, Path: tc.path, CreateFSPath: &trueVal}); err != nil {
				t.Fatal(err)
			}
			attrs, err := conn.FileAttributes(ctx, tc.path)
			if err != nil {
				t.Fatal(err)
			}
			id := naming.BucketID{Endpoint: conn.EndpointHost(), RESTPort: conn.RESTPort(), BucketName: bucket, RootPath: tc.path, RootFileID: attrs.ID}.String()
			params["purgeOnDelete"] = "true"
			if _, err := d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: id, DeleteContext: params}); err != nil {
				t.Fatalf("unmanaged purge must converge via retention: %v", err)
			}
			if _, ok := fq.Buckets[bucket]; ok {
				t.Fatal("unmanaged delete did not unregister the bucket")
			}
			if fq.Files[tc.path] == "" {
				t.Fatal("unmanaged delete removed data it cannot prove it owns")
			}
			if len(fq.TreeJobs) != 0 {
				t.Fatalf("unmanaged purge must never tree-delete: %v", fq.TreeJobs)
			}
		})
	}
}

// A retried create must reconcile properties a previous attempt did not
// apply, instead of returning success on the bare existence of the bucket.
func TestCreate_RetryReconcilesPartialConfiguration(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	params["versioning"] = "Enabled"
	params["quotaLimit"] = "1048576"
	first, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-recon", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, err := naming.ParseBucketID(first.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a first attempt that created the bucket but lost later property
	// writes. The exact same request must repair those partial effects.
	fq.Buckets[id.BucketName].Versioning = ""
	delete(fq.Quota, id.RootFileID)
	resp, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-recon", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, _ = naming.ParseBucketID(resp.BucketId)
	if fq.Buckets[id.BucketName].Versioning != "Enabled" {
		t.Fatalf("versioning not reconciled: %q", fq.Buckets[id.BucketName].Versioning)
	}
	if len(fq.Quota) == 0 {
		t.Fatal("quota not reconciled")
	}

	// A retry carrying changed class parameters reconciles the bucket to the
	// newly requested state (the live-proven 0.1.x semantics): the operator
	// edited the BucketClass, and reconcile-to-requested is the contract.
	params["versioning"] = "Suspended"
	if _, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-recon", Parameters: params}); err != nil {
		t.Fatalf("changed-parameter retry must reconcile: %v", err)
	}
	if fq.Buckets[id.BucketName].Versioning != "Suspended" {
		t.Fatalf("changed-parameter retry did not reconcile versioning: %q", fq.Buckets[id.BucketName].Versioning)
	}
}

// ACL failures always fail closed. The former chmod-0777 fallback is rejected
// because a lost successful response could strand a world-writable bucket
// without delivering the original mode needed for restoration.
func TestGrant_ACLFailure(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		fallback string
	}{
		{"fail-closed-default", "rw", ""},
		{"fallback-rejected-rw", "rw", "true"},
		{"fallback-rejected-ro", "ro", "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, fq, params := harness(t)
			ctx := context.Background()
			created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-acl-" + tc.name, Parameters: params})
			if err != nil {
				t.Fatal(err)
			}
			fq.FailACLPut = true
			params["accessMode"] = tc.mode
			if tc.fallback != "" {
				params["aclFallbackChmod"] = tc.fallback
			}
			_, err = d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
				BucketId:           created.BucketId,
				Name:               "acc",
				AuthenticationType: cosi.AuthenticationType_Key,
				Parameters:         params,
			})
			if err == nil {
				t.Fatal("expected grant to fail closed")
			}
			id, _ := naming.ParseBucketID(created.BucketId)
			mode := fq.Modes["/k8s-buckets/"+id.BucketName]
			if mode == "0777" {
				t.Fatal("unexpected 0777 fallback")
			}
		})
	}
}

// A retried purge after the bucket record is already gone must be an
// idempotent success — on live Core, GetBucket's GET-by-name leg always
// 405s, so this exercises the synthesized-not-found path in GetBucket
// (2026-08 verification finding: the stale 405 used to wedge this forever).
func TestPurge_RetryAfterUnregisterSucceeds(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-purge-retry", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	params["purgeOnDelete"] = "true"
	req := &cosi.DriverDeleteBucketRequest{BucketId: created.BucketId, DeleteContext: params}
	if _, err := d.DriverDeleteBucket(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DriverDeleteBucket(ctx, req); err != nil {
		t.Fatalf("retried purge after unregister must be idempotent success, got %v", err)
	}
	if len(fq.TreeJobs) != 1 {
		t.Fatalf("retried purge must not submit a second tree-delete: %v", fq.TreeJobs)
	}
}

// If unregister succeeds but submitting tree-delete fails, the q2 bucket ID
// carries enough immutable root identity for the next RPC to finish safely.
func TestPurge_RetryFinishesAfterPartialFailure(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-purge-partial", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, err := naming.ParseBucketID(created.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	if id.RootPath == "" || id.RootFileID == "" {
		t.Fatalf("created bucket ID lacks durable root identity: %+v", id)
	}

	params["purgeOnDelete"] = "true"
	req := &cosi.DriverDeleteBucketRequest{BucketId: created.BucketId, DeleteContext: params}
	fq.FailTreeDeleteN.Store(1)
	if _, err := d.DriverDeleteBucket(ctx, req); err == nil {
		t.Fatal("expected injected tree-delete failure")
	}
	if _, ok := fq.Buckets[id.BucketName]; ok {
		t.Fatal("first attempt should have unregistered the bucket")
	}
	if len(fq.TreeJobs) != 0 {
		t.Fatalf("failed submission unexpectedly queued a job: %v", fq.TreeJobs)
	}

	if _, err := d.DriverDeleteBucket(ctx, req); err != nil {
		t.Fatalf("retry did not resume durable purge: %v", err)
	}
	if len(fq.TreeJobs) != 1 || fq.TreeJobs[0] != id.RootFileID {
		t.Fatalf("retry targeted wrong file id: got %v want %q", fq.TreeJobs, id.RootFileID)
	}
}

// Revocation must keep the user (and return an error) until its filesystem
// ACE is actually removed; otherwise a retry can no longer identify the ACE.
func TestRevoke_ACLFailureIsRetryable(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-revoke-acl", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	granted, err := d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
		BucketId:           created.BucketId,
		Name:               "access",
		AuthenticationType: cosi.AuthenticationType_Key,
		Parameters:         params,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &cosi.DriverRevokeBucketAccessRequest{
		BucketId:            created.BucketId,
		AccountId:           granted.AccountId,
		RevokeAccessContext: params,
	}
	fq.FailACLPut = true
	if _, err := d.DriverRevokeBucketAccess(ctx, req); err == nil {
		t.Fatal("expected revoke to fail while ACL PUT fails")
	}
	if len(fq.Users) != 1 {
		t.Fatalf("failed revoke deleted the user needed for retry: users=%d", len(fq.Users))
	}

	fq.FailACLPut = false
	if _, err := d.DriverRevokeBucketAccess(ctx, req); err != nil {
		t.Fatalf("revoke retry failed: %v", err)
	}
	if len(fq.Users) != 0 {
		t.Fatalf("successful retry left user behind: users=%d", len(fq.Users))
	}
}

func TestRevoke_StaleAccountDoesNotTouchReplacementIdentity(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-stale-revoke", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	grantReq := &cosi.DriverGrantBucketAccessRequest{
		BucketId:           created.BucketId,
		Name:               "access",
		AuthenticationType: cosi.AuthenticationType_Key,
		Parameters:         params,
	}
	first, err := d.DriverGrantBucketAccess(ctx, grantReq)
	if err != nil {
		t.Fatal(err)
	}
	oldAccount, err := naming.ParseAccountID(first.AccountId)
	if err != nil || oldAccount.AuthID == "" {
		t.Fatalf("first account lacks immutable auth id: %+v err=%v", oldAccount, err)
	}
	conn, err := qumulo.NewConnection(qumulo.DialConfig{
		Endpoint:    fq.URL,
		Credentials: qumulo.Credentials{Token: fq.Token},
		TLS:         qumulo.TLSConfig{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.DeleteUserByID(ctx, oldAccount.AuthID); err != nil {
		t.Fatal(err)
	}

	second, err := d.DriverGrantBucketAccess(ctx, grantReq)
	if err != nil {
		t.Fatal(err)
	}
	newAccount, err := naming.ParseAccountID(second.AccountId)
	if err != nil {
		t.Fatal(err)
	}
	if newAccount.AuthID == "" || newAccount.AuthID == oldAccount.AuthID {
		t.Fatalf("same-name recreation did not get a new auth id: old=%q new=%q", oldAccount.AuthID, newAccount.AuthID)
	}

	staleRevoke := &cosi.DriverRevokeBucketAccessRequest{
		BucketId: created.BucketId, AccountId: first.AccountId, RevokeAccessContext: params,
	}
	if _, err := d.DriverRevokeBucketAccess(ctx, staleRevoke); err != nil {
		t.Fatal(err)
	}
	if len(fq.Users) != 1 {
		t.Fatalf("stale revoke deleted replacement user: users=%d", len(fq.Users))
	}
	keys, err := conn.ListAccessKeysByOwner(ctx, newAccount.Username, newAccount.AuthID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("stale revoke deleted replacement key: keys=%v err=%v", keys, err)
	}
	bid, _ := naming.ParseBucketID(created.BucketId)
	statements := fq.Buckets[bid.BucketName].Policy.Statement
	if len(statements) != 1 || !strings.Contains(string(statements[0].Principal), "auth_id:"+newAccount.AuthID) {
		t.Fatalf("stale revoke changed replacement policy: %+v", statements)
	}
	acl := fq.ACLs[bid.RootPath]
	if acl == nil || !containsTrustee(acl.ACES, newAccount.AuthID) || containsTrustee(acl.ACES, oldAccount.AuthID) {
		t.Fatalf("stale revoke changed the wrong ACL entries: %+v", acl)
	}
}

// Legacy (pre-0.2.0) handles lack immutable identities. Revoking through one
// must converge with the same name-scoped cleanup the 0.1.0 driver performed
// live, and stay idempotent — an upgraded install must be able to tear down
// its old BucketAccess objects (2026-08 review: blanket refusal wedged the
// sidecar's retries forever).
func TestLegacyHandleRevokeConverges(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-q1-revoke", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	granted, err := d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
		BucketId: created.BucketId, Name: "access", AuthenticationType: cosi.AuthenticationType_Key, Parameters: params,
	})
	if err != nil {
		t.Fatal(err)
	}
	bid, err := naming.ParseBucketID(created.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	aid, err := naming.ParseAccountID(granted.AccountId)
	if err != nil {
		t.Fatal(err)
	}
	legacyAccount := naming.AccountID{
		Endpoint: aid.Endpoint, Username: aid.Username, AccessKeyPfx: aid.AccessKeyPfx,
	}.String()

	req := &cosi.DriverRevokeBucketAccessRequest{
		BucketId: created.BucketId, AccountId: legacyAccount, RevokeAccessContext: params,
	}
	if _, err := d.DriverRevokeBucketAccess(ctx, req); err != nil {
		t.Fatalf("legacy revoke must converge: %v", err)
	}
	if len(fq.Users) != 0 {
		t.Fatalf("legacy revoke left the grant user behind: %d", len(fq.Users))
	}
	if statements := fq.Buckets[bid.BucketName].Policy.Statement; len(statements) != 0 {
		t.Fatalf("legacy revoke left policy statements: %+v", statements)
	}
	if acl := fq.ACLs[bid.RootPath]; acl != nil && containsTrustee(acl.ACES, aid.AuthID) {
		t.Fatalf("legacy revoke left the ACE behind: %+v", acl)
	}
	// Retried revoke after everything is gone is still success.
	if _, err := d.DriverRevokeBucketAccess(ctx, req); err != nil {
		t.Fatalf("legacy revoke retry must be idempotent: %v", err)
	}
}

// Deleting through a legacy bucket handle unregisters the bucket but always
// retains its data, and terminates on retry.
func TestLegacyHandleDeleteUnregistersAndRetainsData(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-q1-delete", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	bid, err := naming.ParseBucketID(created.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	legacyBucket := naming.BucketID{Endpoint: bid.Endpoint, RESTPort: bid.RESTPort, BucketName: bid.BucketName}.String()
	req := &cosi.DriverDeleteBucketRequest{BucketId: legacyBucket, DeleteContext: params}
	if _, err := d.DriverDeleteBucket(ctx, req); err != nil {
		t.Fatalf("legacy delete must converge: %v", err)
	}
	if _, ok := fq.Buckets[bid.BucketName]; ok {
		t.Fatal("legacy delete did not unregister the bucket")
	}
	if fq.Files[bid.RootPath] == "" {
		t.Fatal("legacy delete removed data it cannot prove it owns")
	}
	if len(fq.TreeJobs) != 0 {
		t.Fatalf("legacy delete must never tree-delete: %v", fq.TreeJobs)
	}
	if _, err := d.DriverDeleteBucket(ctx, req); err != nil {
		t.Fatalf("legacy delete retry must be idempotent: %v", err)
	}
}

// Granting through a legacy bucket handle resolves the root by name (the
// live-proven 0.1.0 behavior) so upgraded installs keep granting.
func TestLegacyHandleGrantResolvesRootByName(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-q1-grant", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	bid, err := naming.ParseBucketID(created.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	legacyBucket := naming.BucketID{Endpoint: bid.Endpoint, RESTPort: bid.RESTPort, BucketName: bid.BucketName}.String()
	granted, err := d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
		BucketId: legacyBucket, Name: "access", AuthenticationType: cosi.AuthenticationType_Key, Parameters: params,
	})
	if err != nil {
		t.Fatalf("legacy grant must succeed: %v", err)
	}
	sec := granted.Credentials[naming.S3ProtocolKey].Secrets
	if sec[naming.SecretAccessKeyID] == "" || sec[naming.SecretAccessSecretKey] == "" {
		t.Fatalf("legacy grant returned no credentials: %v", sec)
	}
	acl := fq.ACLs[bid.RootPath]
	if acl == nil || len(acl.ACES) == 0 {
		t.Fatalf("legacy grant did not add a filesystem ACE at %q", bid.RootPath)
	}
}

func containsTrustee(aces []qumulo.ACE, trustee string) bool {
	for _, ace := range aces {
		if ace.Trustee == trustee {
			return true
		}
	}
	return false
}

// A malformed boolean is an operator error: INVALID_ARGUMENT, not Internal.
func TestInvalidBoolIsInvalidArgument(t *testing.T) {
	d, _, params := harness(t)
	params["deleteRootDir"] = "flase"
	_, err := d.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{Name: "bc-bool", Parameters: params})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestGrantRejectsInvalidClassBeforeCreatingCredentials(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "access mode", key: "accessMode", value: "owner"},
		{name: "S3 port", key: "s3Port", value: "not-a-port"},
		{name: "region control", key: "region", value: "us-east-1\nforged"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, fq, params := harness(t)
			ctx := context.Background()
			created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-invalid-grant", Parameters: params})
			if err != nil {
				t.Fatal(err)
			}
			id, err := naming.ParseBucketID(created.BucketId)
			if err != nil {
				t.Fatal(err)
			}
			params[tc.key] = tc.value
			_, err = d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
				BucketId: created.BucketId, Name: "invalid", AuthenticationType: cosi.AuthenticationType_Key, Parameters: params,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
			if len(fq.Users) != 0 {
				t.Fatalf("invalid class created credentials: users=%d", len(fq.Users))
			}
			if got := len(fq.Buckets[id.BucketName].Policy.Statement); got != 0 {
				t.Fatalf("invalid class changed bucket policy: statements=%d", got)
			}
			if acl := fq.ACLs[id.RootPath]; acl != nil && len(acl.ACES) != 0 {
				t.Fatalf("invalid class changed bucket ACL: %+v", acl)
			}
		})
	}
}

func TestCreateRejectsEndpointOutsideProcessBinding(t *testing.T) {
	d, fq, params := harness(t)
	params["endpoint"] = "other-cluster.example"
	_, err := d.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{Name: "bc-other-cluster", Parameters: params})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for cross-cluster create, got %v", err)
	}
	if len(fq.Buckets) != 0 || len(fq.Files) != 0 {
		t.Fatalf("cross-cluster create mutated bound backend: buckets=%d files=%d", len(fq.Buckets), len(fq.Files))
	}
}

func TestCreateRejectsUnsafeClassBeforeBackendMutation(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero quota", key: "quotaLimit", value: "0"},
		{name: "negative quota", key: "quotaLimit", value: "-1"},
		{name: "root base path", key: "basePath", value: "/"},
		{name: "unclean base path", key: "basePath", value: "/safe/../other"},
		{name: "invalid REST port", key: "restPort", value: "65536"},
		{name: "unknown destructive typo", key: "deleteRootDr", value: "false"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, fq, params := harness(t)
			params[tc.key] = tc.value
			_, err := d.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{Name: "bc-invalid-class", Parameters: params})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
			if len(fq.Buckets) != 0 || len(fq.Files) != 0 {
				t.Fatalf("invalid class mutated backend: buckets=%d files=%d", len(fq.Buckets), len(fq.Files))
			}
		})
	}
}

func TestCreateRejectsRESTPortOutsideProcessBinding(t *testing.T) {
	d := driver.New(config.Driver{
		Name: naming.DriverName, DefaultEndpoint: "qumulo.example", DefaultRESTPort: "8000",
		DefaultS3Port: "9000", DefaultBasePath: "/k8s-buckets", DriverNamespace: "qumulo-cosi",
	}, nil, nil, driver.NewTestMetrics())
	_, err := d.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{
		Name: "bc-other-port", Parameters: map[string]string{"endpoint": "qumulo.example", "restPort": "8001"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for cross-port create, got %v", err)
	}
}

// The AlreadyExists fallback (pre-create lookup missed, CreateBucket 409s)
// must reconcile class properties exactly like the other success exits.
func TestCreate_ConflictFallbackReconciles(t *testing.T) {
	d, fq, params := harness(t)
	ctx := context.Background()
	params["versioning"] = "Enabled"
	if _, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-conflict", Parameters: params}); err != nil {
		t.Fatal(err)
	}
	fq.Buckets["bc-conflict"].Versioning = ""
	// Hide the bucket from the pre-create lookup so the retry goes
	// through CreateBucket → 409 → fallback.
	fq.HideListN.Store(1)
	resp, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-conflict", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := naming.ParseBucketID(resp.BucketId)
	if fq.Buckets[id.BucketName].Versioning != "Enabled" {
		t.Fatalf("conflict-fallback exit skipped reconcile: versioning=%q", fq.Buckets[id.BucketName].Versioning)
	}
}

// On legacy Cores (no private-at-create), a retried create must re-assert
// the deny-by-default policy if it is absent, and must never wipe a policy
// that carries statements.
func TestCreate_LegacyCoreAssertsDenyPolicy(t *testing.T) {
	fq := fakequmulo.New()
	t.Cleanup(fq.Close)
	fq.Version = "Qumulo Core 7.1.0"
	fq.RejectPrivate = true
	d := driver.New(config.Driver{
		Name:            naming.DriverName,
		DefaultEndpoint: fq.URL,
		DefaultRegion:   "us-east-1",
		DefaultBasePath: "/k8s-buckets",
		VersionFloor:    "5.3.3",
		DriverNamespace: "qumulo-cosi",
	}, staticSecrets{"token": []byte(fq.Token)}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), driver.NewTestMetrics())
	params := map[string]string{
		"endpoint":                   fq.URL,
		"credentialsSecretName":      "creds",
		"credentialsSecretNamespace": "qumulo-cosi",
		"insecureSkipTLSVerify":      "true",
		"basePath":                   "/k8s-buckets",
	}
	ctx := context.Background()
	resp, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-legacy", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := naming.ParseBucketID(resp.BucketId)
	b := fq.Buckets[id.BucketName]
	etagAfterCreate := b.ETag
	if etagAfterCreate < 2 {
		t.Fatalf("legacy create must have PUT the deny-by-default policy (etag=%d)", etagAfterCreate)
	}
	// Simulate the crash window: policy lost its lockdown (zero statements).
	// A retried create must re-assert it (ETag advances).
	b.Policy = qumulo.EmptyPolicy()
	if _, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-legacy", Parameters: params}); err != nil {
		t.Fatal(err)
	}
	if b.ETag <= etagAfterCreate {
		t.Fatalf("retried create on legacy Core must re-assert the deny policy (etag %d -> %d)", etagAfterCreate, b.ETag)
	}
	// A policy with statements (e.g. grants) must never be wiped.
	b.Policy.Statement = append(b.Policy.Statement, qumulo.PolicyStatement{Sid: "cosi-someone", Effect: "Allow", Action: []string{"s3:GetObject"}})
	etagWithGrant := b.ETag
	if _, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-legacy", Parameters: params}); err != nil {
		t.Fatal(err)
	}
	if b.ETag != etagWithGrant {
		t.Fatalf("reconcile must not rewrite a policy that has statements (etag %d -> %d)", etagWithGrant, b.ETag)
	}
	if len(b.Policy.Statement) != 1 || b.Policy.Statement[0].Sid != "cosi-someone" {
		t.Fatalf("grant statement was clobbered: %+v", b.Policy.Statement)
	}
}

// A failed legacy lockdown must leave the registered bucket in place so an
// idempotent retry can find the exact path and apply the policy. Deleting the
// registration here would orphan the filesystem path.
func TestCreate_LegacyLockdownFailureRecoversOnRetry(t *testing.T) {
	fq := fakequmulo.New()
	t.Cleanup(fq.Close)
	fq.Version = "Qumulo Core 7.1.0"
	fq.RejectPrivate = true
	d := driver.New(config.Driver{
		Name:            naming.DriverName,
		DefaultEndpoint: fq.URL,
		DefaultRegion:   "us-east-1",
		DefaultBasePath: "/k8s-buckets",
		VersionFloor:    "5.3.3",
		DriverNamespace: "qumulo-cosi",
	}, staticSecrets{"token": []byte(fq.Token)}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), driver.NewTestMetrics())
	params := map[string]string{
		"endpoint":                   fq.URL,
		"credentialsSecretName":      "creds",
		"credentialsSecretNamespace": "qumulo-cosi",
		"insecureSkipTLSVerify":      "true",
		"basePath":                   "/k8s-buckets",
	}
	ctx := context.Background()
	fq.FailPolicyPut = true
	if _, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-legacy-recover", Parameters: params}); err == nil {
		t.Fatal("expected injected lockdown failure")
	}
	if len(fq.Buckets) != 1 {
		t.Fatalf("failed lockdown should retain one registered bucket for reconciliation, got %d", len(fq.Buckets))
	}
	if len(fq.Files) != 1 {
		t.Fatalf("failed lockdown lost or duplicated the root path: files=%v", fq.Files)
	}

	fq.FailPolicyPut = false
	resp, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-legacy-recover", Parameters: params})
	if err != nil {
		t.Fatalf("retry failed to reconcile lockdown: %v", err)
	}
	id, err := naming.ParseBucketID(resp.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	if id.RootFileID == "" {
		t.Fatalf("reconciled response did not carry durable root identity: %+v", id)
	}
}

func TestCreate_LegacyConflictFallbackReconciles(t *testing.T) {
	fq := fakequmulo.New()
	t.Cleanup(fq.Close)
	fq.Version = "Qumulo Core 7.1.0"
	fq.RejectPrivate = true
	ctx := context.Background()
	conn, err := qumulo.NewConnection(qumulo.DialConfig{
		Endpoint:    fq.URL,
		Credentials: qumulo.Credentials{Token: fq.Token},
		TLS:         qumulo.TLSConfig{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := driver.New(config.Driver{
		Name: naming.DriverName, DefaultEndpoint: fq.URL, DefaultRegion: "us-east-1",
		DefaultBasePath: "/k8s-buckets", VersionFloor: "5.3.3", DriverNamespace: "qumulo-cosi",
	}, staticSecrets{"token": []byte(fq.Token)}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), driver.NewTestMetrics())
	params := map[string]string{
		"endpoint": fq.URL, "credentialsSecretName": "creds", "credentialsSecretNamespace": "qumulo-cosi",
		"insecureSkipTLSVerify": "true", "basePath": "/k8s-buckets", "versioning": "Enabled",
	}
	createFS := true
	if _, err := conn.CreateBucket(ctx, qumulo.CreateBucketRequest{
		Name: "bc-legacy-conflict", Path: naming.RootPath("/k8s-buckets", "bc-legacy-conflict"), CreateFSPath: &createFS,
	}); err != nil {
		t.Fatal(err)
	}
	// Miss the preflight listing, then exercise private-field rejection,
	// legacy POST conflict, and the conflict reconciliation exit.
	fq.HideListN.Store(1)
	resp, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: "bc-legacy-conflict", Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	id, err := naming.ParseBucketID(resp.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	if fq.Buckets[id.BucketName].Versioning != "Enabled" {
		t.Fatalf("legacy conflict path skipped reconciliation: %+v", fq.Buckets[id.BucketName].Bucket)
	}
}

func TestVersionParse(t *testing.T) {
	if err := qumulo.CheckFloor("Qumulo Core 7.9.2.1", "7.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := qumulo.CheckFloor("Qumulo Core 5.2.0", "5.3.3"); err == nil {
		t.Fatal("expected floor error")
	}
}

// Ensure we never accidentally use the default transport (CSI bug).
func TestHTTPClientIsolated(t *testing.T) {
	if http.DefaultTransport.(*http.Transport).TLSClientConfig != nil &&
		http.DefaultTransport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatal("DefaultTransport mutated")
	}
	_ = tls.VersionTLS12
}
