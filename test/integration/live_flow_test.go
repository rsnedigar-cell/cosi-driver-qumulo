//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	cosi "sigs.k8s.io/container-object-storage-interface/proto"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/config"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/driver"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/naming"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

// TestLiveDriverFlow exercises the full driver lifecycle against a REAL
// Qumulo cluster: create → reconcile-on-retry → grant → key rotation →
// revoke (idempotent) → purge with real-path resolution → filesystem-root
// purge refusal. It is the live re-lock for the 2026-08 review fixes.
//
// Run (lab only):
//
//	QUMULO_HOST=<host> QUMULO_USERNAME=... QUMULO_PASSWORD=... \
//	QUMULO_INSECURE_SKIP_TLS_VERIFY=true \
//	go test -tags=integration -run TestLiveDriverFlow ./test/integration -v
//
// Optional lab-only knobs:
//
//	QUMULO_LIVE_SUFFIX           pin the safe work-dir/name suffix
//	QUMULO_LIVE_KEEP=1           stop after grant for a data-plane probe
//	QUMULO_LIVE_CREDENTIALS_FILE absolute 0600 output file used with KEEP;
//	                             credentials are never written to test logs
func TestLiveDriverFlow(t *testing.T) {
	host := os.Getenv("QUMULO_HOST")
	if host == "" {
		t.Skip("QUMULO_HOST not set")
	}
	if os.Getenv("QUMULO_TOKEN") == "" &&
		(os.Getenv("QUMULO_USERNAME") == "" || os.Getenv("QUMULO_PASSWORD") == "") {
		t.Skip("QUMULO_TOKEN or QUMULO_USERNAME/QUMULO_PASSWORD not set")
	}
	insecure := os.Getenv("QUMULO_INSECURE_SKIP_TLS_VERIFY") == "true"
	keep := os.Getenv("QUMULO_LIVE_KEEP") == "1"

	suffix := os.Getenv("QUMULO_LIVE_SUFFIX")
	if suffix == "" {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		suffix = hex.EncodeToString(b[:])
	}
	if !regexp.MustCompile(`^[a-z0-9]{4,24}$`).MatchString(suffix) {
		t.Fatalf("QUMULO_LIVE_SUFFIX %q must match [a-z0-9]{4,24}", suffix)
	}
	base := "/cosi-live-" + suffix
	flowClaimName := "bc-live-flow-" + suffix
	purgeClaimName := "bc-live-purge-" + suffix
	t.Logf("live work dir %s on %s", base, host)

	cfg := config.Driver{
		Name:            naming.DriverName,
		DefaultEndpoint: host,
		DefaultRegion:   "us-east-1",
		DefaultBasePath: base,
		VersionFloor:    "7.2.0",
		DriverNamespace: "live-test",
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d := driver.New(cfg, nil, log, driver.NewTestMetrics())
	ctx := context.Background()

	params := map[string]string{
		"endpoint": host,
		"basePath": base,
		"region":   "us-east-1",
	}
	if insecure {
		params["insecureSkipTLSVerify"] = "true"
	}
	clone := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range params {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	// Raw client for verification and cleanup.
	conn, err := qumulo.NewConnection(qumulo.DialConfig{
		Endpoint: host,
		Credentials: qumulo.Credentials{
			Token:    os.Getenv("QUMULO_TOKEN"),
			Username: os.Getenv("QUMULO_USERNAME"),
			Password: os.Getenv("QUMULO_PASSWORD"),
		},
		TLS:    qumulo.TLSConfig{InsecureSkipVerify: insecure},
		Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := conn.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cluster version %s", rev)

	// 1. Create with the complete immutable request specification.
	flowParams := clone(map[string]string{"quotaLimit": "1073741824"})
	created, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: flowClaimName, Parameters: flowParams})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, err := naming.ParseBucketID(created.BucketId)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created bucket %s", id.BucketName)

	// 2. An exact retry is idempotent. Different parameters are intentionally
	// ALREADY_EXISTS per the checked-in COSI contract rather than a mutation.
	if _, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{
		Name:       flowClaimName,
		Parameters: flowParams,
	}); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	t.Log("exact create retry preserved the fingerprinted bucket")

	// 3. Grant (the live-proven happy path must still pass with the ACL
	// fail-closed default, review finding 2).
	g1, err := d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
		BucketId:           created.BucketId,
		Name:               "live-access",
		AuthenticationType: cosi.AuthenticationType_Key,
		Parameters:         clone(nil),
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sec := g1.Credentials[naming.S3ProtocolKey].Secrets
	if sec[naming.SecretAccessKeyID] == "" || sec[naming.SecretAccessSecretKey] == "" {
		t.Fatalf("grant returned empty credentials")
	}
	t.Logf("granted access key %s… endpoint %s", naming.AccessKeyPrefix(sec[naming.SecretAccessKeyID]), sec[naming.SecretEndpoint])

	if keep {
		output := os.Getenv("QUMULO_LIVE_CREDENTIALS_FILE")
		if !filepath.IsAbs(output) {
			t.Fatal("QUMULO_LIVE_KEEP=1 requires an absolute QUMULO_LIVE_CREDENTIALS_FILE")
		}
		payload, err := json.MarshalIndent(map[string]string{
			"bucket":          id.BucketName,
			"endpoint":        sec[naming.SecretEndpoint],
			"accessKeyID":     sec[naming.SecretAccessKeyID],
			"accessSecretKey": sec[naming.SecretAccessSecretKey],
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, append(payload, '\n'), 0o600); err != nil {
			t.Fatalf("write live credentials file: %v", err)
		}
		t.Logf("KEEP: credentials written to %s with mode 0600", output)
		t.Log("KEEP: skipping revoke/cleanup; rerun with the same QUMULO_LIVE_SUFFIX and no QUMULO_LIVE_KEEP to clean up")
		return
	}

	// 4. Reproduce Core's deleted-user tombstone behavior: removing a local
	// identity does not remove its S3 keys. Re-grant must bind to the new auth
	// ID, and delayed revoke of the old handle must not damage the replacement.
	oldAccount, err := naming.ParseAccountID(g1.AccountId)
	if err != nil || oldAccount.AuthID == "" {
		t.Fatalf("grant did not return an auth-ID-bound account handle: %v", err)
	}
	if err := conn.DeleteUser(ctx, oldAccount.AuthID); err != nil {
		t.Fatalf("delete live user to create tombstone: %v", err)
	}
	g2, err := d.DriverGrantBucketAccess(ctx, &cosi.DriverGrantBucketAccessRequest{
		BucketId:           created.BucketId,
		Name:               "live-access",
		AuthenticationType: cosi.AuthenticationType_Key,
		Parameters:         clone(nil),
	})
	if err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	if g2.Credentials[naming.S3ProtocolKey].Secrets[naming.SecretAccessKeyID] == sec[naming.SecretAccessKeyID] {
		t.Fatal("re-grant did not rotate the access key")
	}
	newAccount, err := naming.ParseAccountID(g2.AccountId)
	if err != nil || newAccount.AuthID == "" || newAccount.AuthID == oldAccount.AuthID {
		t.Fatalf("re-grant did not bind a replacement identity: old=%q new=%q err=%v", oldAccount.AuthID, newAccount.AuthID, err)
	}
	if _, err := d.DriverRevokeBucketAccess(ctx, &cosi.DriverRevokeBucketAccessRequest{
		BucketId:            created.BucketId,
		AccountId:           g1.AccountId,
		RevokeAccessContext: clone(nil),
	}); err != nil {
		t.Fatalf("revoke tombstoned identity: %v", err)
	}
	liveReplacement, err := conn.GetUserByName(ctx, newAccount.Username)
	if err != nil || liveReplacement.ID != newAccount.AuthID {
		t.Fatalf("old revoke damaged replacement identity: user=%#v err=%v", liveReplacement, err)
	}
	newKeys, err := conn.ListAccessKeysByOwner(ctx, newAccount.Username, newAccount.AuthID)
	if err != nil || len(newKeys) == 0 {
		t.Fatalf("old revoke damaged replacement keys: keys=%d err=%v", len(newKeys), err)
	}
	t.Log("deleted-user tombstone and replacement identity isolation verified")

	// 5. Revoke the replacement twice; second must be a no-op success.
	for i := 0; i < 2; i++ {
		if _, err := d.DriverRevokeBucketAccess(ctx, &cosi.DriverRevokeBucketAccessRequest{
			BucketId:            created.BucketId,
			AccountId:           g2.AccountId,
			RevokeAccessContext: clone(nil),
		}); err != nil {
			t.Fatalf("revoke #%d: %v", i+1, err)
		}
	}
	t.Log("revoke idempotent")

	// 6. Purge must resolve the bucket's real path (review finding 1).
	purge, err := d.DriverCreateBucket(ctx, &cosi.DriverCreateBucketRequest{Name: purgeClaimName, Parameters: clone(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{
		BucketId:      purge.BucketId,
		DeleteContext: clone(map[string]string{"purgeOnDelete": "true"}),
	}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	pid, _ := naming.ParseBucketID(purge.BucketId)
	if _, err := conn.GetBucket(ctx, pid.BucketName); err == nil {
		t.Fatal("purged bucket still registered")
	}
	t.Log("purge removed bucket via its real root")

	// 7. Purge through an unmanaged (legacy q1) handle — here for a bucket
	// registered at "/", the most dangerous possible target — must converge
	// by unregistering with retention: no tree-delete, no data touched.
	guardName := "cosi-live-rootguard-" + suffix
	// Core 7.9.2.2 lock: POST with "path" requires "create_fs_path" present.
	noCreate := false
	if _, err := conn.CreateBucket(ctx, qumulo.CreateBucketRequest{Name: guardName, Path: "/", CreateFSPath: &noCreate}); err != nil {
		t.Fatalf("create root-guard bucket: %v", err)
	}
	gid := naming.BucketID{Endpoint: host, RESTPort: "8000", BucketName: guardName}
	if _, err := d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{
		BucketId:      gid.String(),
		DeleteContext: clone(map[string]string{"purgeOnDelete": "true"}),
	}); err != nil {
		t.Fatalf("legacy root purge must converge via retention: %v", err)
	}
	if _, gerr := conn.GetBucket(ctx, guardName); gerr == nil {
		t.Fatal("legacy purge must unregister the bucket")
	}
	// The work dir still resolving proves no tree-delete was submitted
	// against "/" — a tree-delete of the filesystem root would take this
	// directory (and everything else) with it.
	if _, aerr := conn.FileAttributes(ctx, base); aerr != nil {
		t.Fatalf("filesystem-root data was touched by legacy purge: %v", aerr)
	}
	t.Log("legacy root purge converged: unregistered with retention, no data touched")

	// 8. Normal delete of the main bucket, then remove the work dir.
	if _, err := d.DriverDeleteBucket(ctx, &cosi.DriverDeleteBucketRequest{BucketId: created.BucketId, DeleteContext: clone(nil)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if attrs, err := conn.FileAttributes(ctx, base); err == nil {
		if err := conn.TreeDelete(ctx, attrs.ID); err != nil {
			t.Logf("cleanup tree-delete %s: %v (remove manually)", base, err)
		}
	}
	t.Log("live flow complete; work dir cleaned")
}
