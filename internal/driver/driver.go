package driver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cosi "sigs.k8s.io/container-object-storage-interface/proto"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/config"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/naming"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/policy"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

// Driver implements COSI Identity + Provisioner.
type Driver struct {
	cosi.UnimplementedIdentityServer
	cosi.UnimplementedProvisionerServer

	cfg     config.Driver
	log     *slog.Logger
	cache   *qumulo.Cache
	secrets config.SecretReader
	metrics *Metrics
}

func New(cfg config.Driver, secrets config.SecretReader, log *slog.Logger, metrics *Metrics) *Driver {
	if log == nil {
		log = slog.Default()
	}
	if metrics == nil {
		metrics = NewMetrics()
	}
	return &Driver{
		cfg:     cfg,
		log:     log,
		cache:   qumulo.NewCache(16),
		secrets: secrets,
		metrics: metrics,
	}
}

func (d *Driver) DriverGetInfo(_ context.Context, _ *cosi.DriverGetInfoRequest) (*cosi.DriverGetInfoResponse, error) {
	return &cosi.DriverGetInfoResponse{Name: d.cfg.Name}, nil
}

func (d *Driver) connect(ctx context.Context, class config.Class) (*qumulo.Connection, config.Class, error) {
	if class.Endpoint == "" {
		return nil, class, status.Error(codes.InvalidArgument, "no Qumulo endpoint: set BucketClass parameters.endpoint or QUMULO_ENDPOINT")
	}
	creds, ca, err := config.ResolveCredentials(ctx, class, d.cfg, d.secrets)
	if err != nil {
		return nil, class, status.Errorf(codes.Unauthenticated, "%s", err.Error())
	}
	key := qumulo.CacheKey(class.Endpoint, class.RESTPort, creds, ca, class.InsecureSkipTLSVerify)
	conn, err := d.cache.Get(key, func() (*qumulo.Connection, error) {
		dc := class.DialConfig(creds, ca, d.log)
		dc.Logger = d.log
		return qumulo.NewConnection(dc)
	})
	if err != nil {
		return nil, class, err
	}
	if _, err := conn.EnsureVersion(ctx, d.cfg.VersionFloor); err != nil {
		return nil, class, err
	}
	if _, err := conn.RequireS3Enabled(ctx); err != nil {
		return nil, class, err
	}
	return conn, class, nil
}

func (d *Driver) classFrom(params map[string]string) (config.Class, error) {
	class, err := config.ParseClass(params, d.cfg)
	if err != nil {
		return class, err
	}
	return d.bindClassToDriverEndpoint(class)
}

// classFromCleanup parses replayed delete/revoke contexts leniently: unknown
// stored parameters are dropped rather than failing the RPC, so cleanup can
// never be blocked by a key a different driver version once accepted.
func (d *Driver) classFromCleanup(params map[string]string) (config.Class, error) {
	class, err := config.ParseClassForCleanup(params, d.cfg)
	if err != nil {
		return class, err
	}
	return d.bindClassToDriverEndpoint(class)
}

func (d *Driver) bindClassToDriverEndpoint(class config.Class) (config.Class, error) {
	configuredEndpoint := strings.TrimSpace(d.cfg.DefaultEndpoint)
	if configuredEndpoint == "" {
		return class, status.Error(codes.FailedPrecondition, "QUMULO_ENDPOINT must be configured on the driver; per-class cluster selection is disabled")
	}
	configuredPort := strings.TrimSpace(d.cfg.DefaultRESTPort)
	if configuredPort == "" {
		configuredPort = naming.DefaultRESTPort
	}
	if !sameClusterEndpoint(class.Endpoint, class.RESTPort, configuredEndpoint, configuredPort) {
		return class, status.Errorf(codes.InvalidArgument, "class endpoint %q:%s does not match the driver-bound endpoint %q:%s", class.Endpoint, class.RESTPort, configuredEndpoint, configuredPort)
	}
	return class, nil
}

func (d *Driver) DriverCreateBucket(ctx context.Context, req *cosi.DriverCreateBucketRequest) (*cosi.DriverCreateBucketResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket name is required")
	}
	class, err := d.classFrom(req.GetParameters())
	if err != nil {
		return nil, toStatus(err)
	}
	conn, class, err := d.connect(ctx, class)
	if err != nil {
		return nil, toStatus(err)
	}

	if class.ExistingBucketName != "" {
		return d.importExistingBucket(ctx, conn, class)
	}

	name, err := naming.BucketName(class.BucketPrefix, req.GetName())
	if err != nil {
		return nil, toStatus(err)
	}

	if err := d.ensureFeatures(ctx, conn, class); err != nil {
		return nil, toStatus(err)
	}

	wantPath := naming.RootPath(class.BasePath, name)
	existing, gerr := conn.GetBucket(ctx, name)
	if gerr == nil && existing != nil {
		if pathsMatch(existing.Path, wantPath) {
			// Retried create: an earlier attempt may have created the bucket
			// and then failed on versioning/quota/lock. Reconcile every
			// requested property before reporting success.
			if err := d.ensureBucketConfig(ctx, conn, class, name, existing, wantPath); err != nil {
				return nil, toStatus(err)
			}
			d.log.Info("create bucket idempotent hit", "bucket", name, "path", existing.Path)
			return d.createOK(ctx, conn, class, name, existing)
		}
		return nil, status.Errorf(codes.AlreadyExists, "bucket %q already exists at path %q, expected %q", name, existing.Path, wantPath)
	}
	if gerr != nil && !notFound(gerr) {
		// Never create after an inconclusive existence check. A transport,
		// authorization, or server error is not evidence that the name is
		// free, and proceeding could collide with a foreign bucket.
		return nil, toStatus(gerr)
	}

	createFS := true
	private := true
	ol := class.ObjectLockEnabled
	creq := qumulo.CreateBucketRequest{
		Name:         name,
		Path:         wantPath,
		CreateFSPath: &createFS,
		Private:      &private,
	}
	if ol {
		creq.ObjectLockEnabled = &ol
	}

	created, err := conn.CreateBucket(ctx, creq)
	if err != nil {
		if api, ok := qumulo.AsAPIError(err); ok && api.IsAlreadyExists() {
			again, gerr := conn.GetBucket(ctx, name)
			if gerr != nil {
				return nil, toStatus(fmt.Errorf("reconcile bucket %q after create conflict: %w", name, gerr))
			}
			if again != nil && pathsMatch(again.Path, wantPath) {
				// Same reconcile obligation as the idempotent-hit path: a
				// prior attempt may have created the bucket and failed on a
				// later property step.
				if err := d.ensureBucketConfig(ctx, conn, class, name, again, wantPath); err != nil {
					return nil, toStatus(err)
				}
				return d.createOK(ctx, conn, class, name, again)
			}
			return nil, status.Errorf(codes.AlreadyExists, "bucket %q already exists with a different path", name)
		}
		// Older cores reject `private: true` — retry without it and PUT an empty-principal policy.
		if classPrivateUnsupported(err) {
			creq.Private = nil
			created, err = conn.CreateBucket(ctx, creq)
			if err != nil {
				if api, ok := qumulo.AsAPIError(err); ok && api.IsAlreadyExists() {
					again, gerr := conn.GetBucket(ctx, name)
					if gerr != nil {
						return nil, toStatus(fmt.Errorf("reconcile legacy bucket %q after create conflict: %w", name, gerr))
					}
					if again != nil && pathsMatch(again.Path, wantPath) {
						if err := d.ensureBucketConfig(ctx, conn, class, name, again, wantPath); err != nil {
							return nil, toStatus(err)
						}
						return d.createOK(ctx, conn, class, name, again)
					}
					return nil, status.Errorf(codes.AlreadyExists, "bucket %q already exists with a different path", name)
				}
				return nil, toStatus(err)
			}
			if lerr := d.lockDownLegacy(ctx, conn, name); lerr != nil {
				// Keep the registered bucket so the next idempotent create can
				// find the exact path and retry the lockdown. Unregistering here
				// can orphan the newly-created filesystem path and make every
				// later create fail with an fs-entry-exists conflict.
				return nil, toStatus(fmt.Errorf("apply deny-by-default policy on legacy Core: %w", lerr))
			}
		} else {
			return nil, toStatus(err)
		}
	}
	if created != nil && created.Name != "" {
		name = created.Name
	}

	if err := d.ensureBucketConfig(ctx, conn, class, name, created, wantPath); err != nil {
		return nil, toStatus(err)
	}

	d.log.Info("created bucket", "bucket", name, "path", wantPath, "endpoint", class.Endpoint)
	return d.createOK(ctx, conn, class, name, created)
}

// importExistingBucket binds a pre-existing (brownfield) bucket to the claim
// without creating or reconfiguring anything. The returned handle is
// unmanaged: delete unregisters the bucket but always retains its data, and
// purge downgrades to retention, because the driver cannot prove it owns an
// imported root.
func (d *Driver) importExistingBucket(ctx context.Context, conn *qumulo.Connection, class config.Class) (*cosi.DriverCreateBucketResponse, error) {
	name := class.ExistingBucketName
	if err := naming.ValidateBucketName(name); err != nil {
		return nil, toStatus(err)
	}
	b, err := conn.GetBucket(ctx, name)
	if err != nil {
		if notFound(err) {
			return nil, status.Errorf(codes.NotFound, "existingBucketName %q not found on cluster %s", name, class.Endpoint)
		}
		return nil, toStatus(err)
	}
	if b == nil || b.Path == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "existing bucket %q has no filesystem root", name)
	}
	attrs, err := conn.FileAttributes(ctx, b.Path)
	if err != nil {
		return nil, toStatus(fmt.Errorf("resolve imported bucket root identity: %w", err))
	}
	if attrs.ID == "" {
		return nil, status.Errorf(codes.Internal, "imported bucket %q root %q has an empty file id", name, b.Path)
	}
	d.log.Info("imported existing bucket", "bucket", name, "path", b.Path)
	id := naming.BucketID{
		Endpoint:   conn.EndpointHost(),
		RESTPort:   conn.RESTPort(),
		BucketName: name,
		RootPath:   b.Path,
		RootFileID: attrs.ID,
		Managed:    false,
	}
	return &cosi.DriverCreateBucketResponse{
		BucketId: id.String(),
		BucketInfo: &cosi.Protocol{
			Type: &cosi.Protocol_S3{
				S3: &cosi.S3{
					Region:           class.Region,
					SignatureVersion: cosi.S3SignatureVersion_S3V4,
				},
			},
		},
	}, nil
}

// ensureBucketConfig applies every class-requested property to an existing
// driver-created bucket. It runs on fresh creates and on idempotent retries
// so a bucket is never reported Ready with a requested setting missing.
// Every step is idempotent; any failure fails the create (the sidecar
// retries, and the retry reconciles again).
func (d *Driver) ensureBucketConfig(ctx context.Context, conn *qumulo.Connection, class config.Class, name string, current *qumulo.Bucket, expectedPath string) error {
	// Re-assert deny-by-default whenever a driver-created bucket has no
	// policy statements. This closes both the legacy private-at-create
	// fallback window and an ambiguous POST outcome where Core committed the
	// non-private create but the client never received its response. Never
	// touch a policy carrying grant or operator-managed statements.
	if err := conn.MutateBucketPolicy(ctx, name, func(p *qumulo.Policy) error {
		if len(p.Statement) > 0 {
			return qumulo.ErrPolicyUnchanged
		}
		*p = *qumulo.EmptyPolicy()
		return nil
	}); err != nil {
		return fmt.Errorf("assert deny-by-default bucket policy: %w", err)
	}
	if class.Versioning == "Enabled" || class.Versioning == "Suspended" {
		if current == nil || current.Versioning != class.Versioning {
			v := class.Versioning
			if _, err := conn.PatchBucket(ctx, name, qumulo.PatchBucketRequest{Versioning: &v}); err != nil {
				return fmt.Errorf("apply versioning %s: %w", v, err)
			}
		}
	}
	if class.ObjectLockEnabled && (current == nil || !current.LockConfig.Enabled) {
		lc := qumulo.LockConfig{Enabled: true}
		if _, err := conn.PatchBucket(ctx, name, qumulo.PatchBucketRequest{LockConfig: &lc}); err != nil {
			return fmt.Errorf("apply object lock: %w", err)
		}
	}
	if class.QuotaLimit > 0 {
		path := ""
		if current != nil {
			path = current.Path
		}
		if path == "" {
			path = expectedPath
		}
		if err := d.applyQuota(ctx, conn, path, class.QuotaLimit); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) createOK(ctx context.Context, conn *qumulo.Connection, class config.Class, name string, current *qumulo.Bucket) (*cosi.DriverCreateBucketResponse, error) {
	root := ""
	if current != nil {
		root = current.Path
	}
	if root == "" {
		b, err := conn.GetBucket(ctx, name)
		if err != nil {
			return nil, toStatus(fmt.Errorf("resolve bucket root for durable bucket id: %w", err))
		}
		root = b.Path
	}
	if root == "" {
		return nil, status.Errorf(codes.Internal, "bucket %q has an empty filesystem root", name)
	}
	attrs, err := conn.FileAttributes(ctx, root)
	if err != nil {
		return nil, toStatus(fmt.Errorf("resolve bucket root identity for durable bucket id: %w", err))
	}
	if attrs.ID == "" {
		return nil, status.Errorf(codes.Internal, "bucket %q root %q has an empty file id", name, root)
	}
	id := naming.BucketID{
		Endpoint:   conn.EndpointHost(),
		RESTPort:   conn.RESTPort(),
		BucketName: name,
		RootPath:   root,
		RootFileID: attrs.ID,
		Managed:    true,
	}
	return &cosi.DriverCreateBucketResponse{
		BucketId: id.String(),
		BucketInfo: &cosi.Protocol{
			Type: &cosi.Protocol_S3{
				S3: &cosi.S3{
					Region:           class.Region,
					SignatureVersion: cosi.S3SignatureVersion_S3V4,
				},
			},
		},
	}, nil
}

func (d *Driver) DriverDeleteBucket(ctx context.Context, req *cosi.DriverDeleteBucketRequest) (*cosi.DriverDeleteBucketResponse, error) {
	if req.GetBucketId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket_id is required")
	}
	id, err := naming.ParseBucketID(req.GetBucketId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	class, err := d.classFromCleanup(req.GetDeleteContext())
	if err != nil {
		return nil, toStatus(err)
	}
	class, err = bindClassToBucketID(class, id)
	if err != nil {
		return nil, err
	}
	conn, class, err := d.connect(ctx, class)
	if err != nil {
		return nil, toStatus(err)
	}
	if !id.Managed {
		// Handles that do not prove the provisioner created the directory
		// (pre-0.2.0 q1/q2, and brownfield imports) always retain data.
		class.DeleteRootDir = false
		class.PurgeOnDelete = false
	}

	name := id.BucketName
	if id.RootPath == "" || id.RootFileID == "" {
		// Legacy (pre-0.2.0) handle: only a mutable bucket name. Unregister
		// the bucket but always retain its data — the handle cannot prove
		// ownership of the root, and a same-name replacement must never lose
		// data through a stale handle. Convergent by design: absence is the
		// idempotent success case, so retried deletes terminate.
		if _, gerr := conn.GetBucket(ctx, name); gerr != nil {
			if notFound(gerr) {
				return &cosi.DriverDeleteBucketResponse{}, nil
			}
			return nil, toStatus(gerr)
		}
		d.log.Warn("deleting via legacy bucket handle: unregistering bucket and retaining its root directory", "bucket", name)
		if err := conn.DeleteBucket(ctx, name, false); err != nil && !notFound(err) {
			return nil, toStatus(err)
		}
		return &cosi.DriverDeleteBucketResponse{}, nil
	}
	if class.PurgeOnDelete {
		// q2 bucket IDs retain the immutable root path + file id captured at
		// create time. That lets a retry safely finish tree-delete after the
		// bucket record has already been unregistered, without reconstructing
		// a potentially wrong path from mutable class parameters.
		b, gerr := conn.GetBucket(ctx, name)
		if gerr != nil {
			if notFound(gerr) {
				if err := validatePurgeRoot(id.RootPath); err != nil {
					return nil, err
				}
				attrs, aerr := conn.FileAttributes(ctx, id.RootPath)
				if aerr != nil {
					if notFound(aerr) {
						// The earlier tree-delete completed (possibly after its
						// response was lost). The retry is now complete.
						return &cosi.DriverDeleteBucketResponse{}, nil
					}
					return nil, toStatus(aerr)
				}
				if attrs.ID != id.RootFileID {
					return nil, status.Errorf(codes.FailedPrecondition, "refusing resumed purge for bucket %q: root %q now has file id %q, expected %q", name, id.RootPath, attrs.ID, id.RootFileID)
				}
				d.log.Warn("purgeOnDelete: resuming tree-delete after bucket unregister", "bucket", name, "path", id.RootPath, "fileID", id.RootFileID)
				if err := conn.TreeDelete(ctx, id.RootFileID); err != nil {
					return nil, toStatus(err)
				}
				return &cosi.DriverDeleteBucketResponse{}, nil
			}
			return nil, toStatus(gerr)
		}
		root := b.Path
		if err := validatePurgeRoot(root); err != nil {
			return nil, err
		}
		if id.RootPath != "" && !pathsMatch(root, id.RootPath) {
			return nil, status.Errorf(codes.FailedPrecondition, "refusing purge for bucket %q: live root %q differs from recorded root %q", name, root, id.RootPath)
		}
		attrs, aerr := conn.FileAttributes(ctx, root)
		if aerr != nil && !notFound(aerr) {
			return nil, toStatus(aerr)
		}
		if attrs != nil && id.RootFileID != "" && attrs.ID != id.RootFileID {
			return nil, status.Errorf(codes.FailedPrecondition, "refusing purge for bucket %q: live root file id %q differs from recorded id %q", name, attrs.ID, id.RootFileID)
		}
		d.log.Warn("purgeOnDelete: unregistering bucket and tree-deleting its root", "bucket", name, "path", root)
		if err := conn.DeleteBucket(ctx, name, false); err != nil && !notFound(err) {
			return nil, toStatus(err)
		}
		if attrs == nil {
			return &cosi.DriverDeleteBucketResponse{}, nil
		}
		if err := conn.TreeDelete(ctx, attrs.ID); err != nil {
			return nil, toStatus(err)
		}
		return &cosi.DriverDeleteBucketResponse{}, nil
	}

	// A q2 handle binds destructive retries to the exact filesystem object
	// created for this bucket. If the bucket name/path was reused after an
	// earlier delete, do not delete the replacement bucket.
	if id.RootPath != "" && id.RootFileID != "" {
		b, gerr := conn.GetBucket(ctx, name)
		if gerr != nil {
			if notFound(gerr) {
				return &cosi.DriverDeleteBucketResponse{}, nil
			}
			return nil, toStatus(gerr)
		}
		if !pathsMatch(b.Path, id.RootPath) {
			return nil, status.Errorf(codes.FailedPrecondition, "refusing delete for bucket %q: live root %q differs from recorded root %q", name, b.Path, id.RootPath)
		}
		attrs, aerr := conn.FileAttributes(ctx, b.Path)
		if aerr != nil {
			return nil, toStatus(fmt.Errorf("verify bucket root identity before delete: %w", aerr))
		}
		if attrs.ID != id.RootFileID {
			return nil, status.Errorf(codes.FailedPrecondition, "refusing delete for bucket %q: live root file id %q differs from recorded id %q", name, attrs.ID, id.RootFileID)
		}
	}

	if class.DeleteRootDir {
		if _, err := conn.AbortAllUploads(ctx, name); err != nil && !notFound(err) {
			d.log.Warn("abort uploads before delete", "bucket", name, "err", err)
		}
	}
	err = conn.DeleteBucket(ctx, name, class.DeleteRootDir)
	if err == nil || notFound(err) {
		return &cosi.DriverDeleteBucketResponse{}, nil
	}
	if api, ok := qumulo.AsAPIError(err); ok && api.IsNotEmpty() {
		if class.DeleteRootDir {
			// retry once after aborting MPUs
			if _, aerr := conn.AbortAllUploads(ctx, name); aerr == nil {
				if rerr := conn.DeleteBucket(ctx, name, true); rerr == nil || notFound(rerr) {
					return &cosi.DriverDeleteBucketResponse{}, nil
				}
			}
		}
		return nil, status.Errorf(codes.FailedPrecondition, "bucket %q root is not empty (or has in-progress uploads). Empty the bucket or set purgeOnDelete=true on the BucketClass. (%s)", name, api.Error())
	}
	return nil, toStatus(err)
}

func validatePurgeRoot(root string) error {
	if strings.TrimRight(root, "/") == "" {
		return status.Errorf(codes.FailedPrecondition, "refusing purgeOnDelete: bucket root %q is the filesystem root", root)
	}
	if !strings.HasPrefix(root, "/") {
		return status.Errorf(codes.FailedPrecondition, "refusing purgeOnDelete: bucket root %q is not an absolute filesystem path", root)
	}
	return nil
}

func (d *Driver) DriverGrantBucketAccess(ctx context.Context, req *cosi.DriverGrantBucketAccessRequest) (*cosi.DriverGrantBucketAccessResponse, error) {
	if req.GetBucketId() == "" || req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket_id and name are required")
	}
	switch req.GetAuthenticationType() {
	case cosi.AuthenticationType_Key:
		// ok
	case cosi.AuthenticationType_IAM:
		return nil, status.Error(codes.Unimplemented, "IAM authentication is not supported; use authenticationType: Key")
	default:
		return nil, status.Error(codes.InvalidArgument, "authentication_type must be Key (IAM is not implemented)")
	}

	id, err := naming.ParseBucketID(req.GetBucketId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	class, err := d.classFrom(req.GetParameters())
	if err != nil {
		return nil, toStatus(err)
	}
	class, err = bindClassToBucketID(class, id)
	if err != nil {
		return nil, err
	}
	conn, class, err := d.connect(ctx, class)
	if err != nil {
		return nil, toStatus(err)
	}
	var root string
	if id.RootPath == "" || id.RootFileID == "" {
		// Legacy (pre-0.2.0) handle: no immutable root identity recorded.
		// Fall back to the live-proven 0.1.0 behavior and grant against the
		// bucket's currently registered root, resolved by name, so upgraded
		// installs keep working. New handles get the verified path below.
		b, gerr := conn.GetBucket(ctx, id.BucketName)
		if gerr != nil {
			return nil, toStatus(gerr)
		}
		if b == nil || b.Path == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "bucket %q has no filesystem root", id.BucketName)
		}
		root = b.Path
		d.log.Warn("granting via legacy bucket handle; root resolved by name", "bucket", id.BucketName, "path", root)
	} else {
		root, err = verifiedBucketRoot(ctx, conn, id)
		if err != nil {
			return nil, err
		}
	}

	username := naming.Username(req.GetBucketId(), req.GetName())
	pass, err := randomPassword()
	if err != nil {
		return nil, toStatus(err)
	}
	local, err := conn.CreateUser(ctx, username, pass)
	if err != nil && !alreadyExists(err) {
		return nil, toStatus(fmt.Errorf("create local user %s: %w", username, err))
	}
	if local == nil || local.ID == "" {
		local, err = conn.GetUserByName(ctx, username)
		if err != nil {
			return nil, toStatus(fmt.Errorf("lookup local user %s: %w", username, err))
		}
	}

	// Rotate driver-owned keys so a retried Grant always returns a usable
	// secret. Reference the user by auth_id, never by name: Core 7.9.2.2
	// name resolution can hit the tombstone of a previously deleted
	// same-named user (cred_invalid_local_user_error).
	if err := d.rotateKeys(ctx, conn, username, local.ID); err != nil {
		return nil, toStatus(err)
	}
	key, err := conn.CreateAccessKey(ctx, qumulo.Identity{AuthID: local.ID})
	if err != nil {
		// A transport error can arrive after Core committed the POST but before
		// its one-time secret reached us. Remove any unverifiable credential so
		// a failed Grant never leaves an unknown active key behind.
		cleanupCtx, cleanupCancel := accessKeyCleanupContext(ctx)
		cleanupErr := d.rotateKeys(cleanupCtx, conn, username, local.ID)
		cleanupCancel()
		if cleanupErr != nil {
			d.log.Error("clean up access keys after failed create", "user", username, "err", cleanupErr)
		}
		return nil, toStatus(fmt.Errorf("create access key: %w", err))
	}
	if key == nil || key.AccessKeyID == "" || key.SecretAccessKey == "" {
		cleanupCtx, cleanupCancel := accessKeyCleanupContext(ctx)
		cleanupErr := d.rotateKeys(cleanupCtx, conn, username, local.ID)
		cleanupCancel()
		if cleanupErr != nil {
			d.log.Error("clean up incomplete access-key response", "user", username, "err", cleanupErr)
		}
		return nil, status.Error(codes.Internal, "Qumulo returned an incomplete access key")
	}
	grantComplete := false
	defer func() {
		if !grantComplete {
			cleanupCtx, cleanupCancel := accessKeyCleanupContext(ctx)
			defer cleanupCancel()
			if cleanupErr := conn.DeleteAccessKey(cleanupCtx, key.AccessKeyID); cleanupErr != nil && !notFound(cleanupErr) {
				d.log.Error("clean up access key after failed grant", "user", username, "access_key_prefix", naming.AccessKeyPrefix(key.AccessKeyID), "err", cleanupErr)
			}
		}
	}()

	sid := naming.PolicySID(username)
	err = conn.MutateBucketPolicy(ctx, id.BucketName, func(p *qumulo.Policy) error {
		return policy.UpsertStatement(p, sid, local.ID, class.AccessMode, id.BucketName)
	})
	if err != nil {
		return nil, toStatus(fmt.Errorf("upsert bucket policy: %w", err))
	}

	grantResult, err := conn.GrantDirectoryAccess(ctx, root, local.ID, class.AccessMode, class.ACLFallbackChmod)
	if err != nil {
		return nil, toStatus(fmt.Errorf("grant filesystem access on %s: %w", root, err))
	}

	acct := naming.AccountID{
		Endpoint:     conn.BaseURL(),
		Username:     username,
		AccessKeyPfx: naming.AccessKeyPrefix(key.AccessKeyID),
		AuthID:       local.ID,
		RestoreMode:  grantResult.RestoreMode,
	}
	s3ep := naming.S3Endpoint(class.Endpoint, class.S3Port)
	d.log.Info("granted bucket access", "bucket", id.BucketName, "user", username, "mode", class.AccessMode, "endpoint", s3ep)
	grantComplete = true

	return &cosi.DriverGrantBucketAccessResponse{
		AccountId: acct.String(),
		Credentials: map[string]*cosi.CredentialDetails{
			naming.S3ProtocolKey: {
				Secrets: map[string]string{
					naming.SecretAccessKeyID:     key.AccessKeyID,
					naming.SecretAccessSecretKey: key.SecretAccessKey,
					naming.SecretEndpoint:        s3ep,
					naming.SecretRegion:          class.Region,
				},
			},
		},
	}, nil
}

func verifiedBucketRoot(ctx context.Context, conn *qumulo.Connection, id naming.BucketID) (string, error) {
	b, err := conn.GetBucket(ctx, id.BucketName)
	if err != nil {
		return "", toStatus(err)
	}
	if b == nil || b.Path == "" {
		return "", status.Errorf(codes.FailedPrecondition, "bucket %q has no filesystem root", id.BucketName)
	}
	if id.RootPath == "" || id.RootFileID == "" {
		return "", status.Errorf(codes.FailedPrecondition, "legacy bucket_id for bucket %q lacks immutable root identity", id.BucketName)
	}
	if !pathsMatch(b.Path, id.RootPath) {
		return "", status.Errorf(codes.FailedPrecondition, "bucket %q root %q differs from recorded root %q", id.BucketName, b.Path, id.RootPath)
	}
	attrs, err := conn.FileAttributes(ctx, b.Path)
	if err != nil {
		return "", toStatus(err)
	}
	if attrs.ID != id.RootFileID {
		return "", status.Errorf(codes.FailedPrecondition, "bucket %q root file id %q differs from recorded id %q", id.BucketName, attrs.ID, id.RootFileID)
	}
	return b.Path, nil
}

func (d *Driver) DriverRevokeBucketAccess(ctx context.Context, req *cosi.DriverRevokeBucketAccessRequest) (*cosi.DriverRevokeBucketAccessResponse, error) {
	if req.GetBucketId() == "" || req.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket_id and account_id are required")
	}
	bid, err := naming.ParseBucketID(req.GetBucketId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	aid, err := naming.ParseAccountID(req.GetAccountId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	class, err := d.classFromCleanup(req.GetRevokeAccessContext())
	if err != nil {
		return nil, toStatus(err)
	}
	class, err = bindClassToBucketID(class, bid)
	if err != nil {
		return nil, err
	}
	if !sameClusterEndpoint(aid.Endpoint, bid.RESTPort, bid.Endpoint, bid.RESTPort) {
		return nil, status.Errorf(codes.InvalidArgument, "account_id endpoint %q does not match bucket_id endpoint %q:%s", aid.Endpoint, bid.Endpoint, bid.RESTPort)
	}
	conn, class, err := d.connect(ctx, class)
	if err != nil {
		return nil, toStatus(err)
	}
	if bid.RootPath == "" || bid.RootFileID == "" || aid.AuthID == "" {
		return d.revokeLegacy(ctx, conn, bid, aid)
	}

	// q2 account IDs bind revocation to the immutable auth identity. A local
	// username can be deleted and recreated; a delayed revoke must never
	// delete the replacement user's keys, policy, ACE, or identity.
	authID := aid.AuthID
	var targetUser *qumulo.LocalUser
	liveUser, uerr := conn.GetUserByName(ctx, aid.Username)
	if uerr == nil && liveUser != nil {
		if liveUser.ID == authID {
			targetUser = liveUser
		} else {
			d.log.Warn("revoke found a replacement user with the same name; leaving it untouched", "user", aid.Username, "recordedAuthID", authID, "liveAuthID", liveUser.ID)
		}
	} else if uerr != nil && !notFound(uerr) {
		// An inconclusive identity lookup must not be treated as a missing
		// user: doing so can delete policy/keys while silently skipping the
		// filesystem ACE, leaving access behind with no retryable owner.
		return nil, toStatus(fmt.Errorf("lookup local user %s for revoke: %w", aid.Username, uerr))
	}

	keys, err := conn.ListAccessKeysByOwner(ctx, aid.Username, authID)
	if err != nil && !notFound(err) {
		return nil, toStatus(err)
	}
	for _, k := range keys {
		if err := conn.DeleteAccessKey(ctx, k.AccessKeyID); err != nil && !notFound(err) {
			return nil, toStatus(err)
		}
	}

	// Resolve the bucket/root before touching bucket-scoped authorization.
	// q2 handles require both path and file-id to match; if the name or path
	// has been reused, cleanup continues for the old credentials but leaves
	// the replacement bucket untouched.
	policyTarget := true
	root := ""
	if bid.RootPath != "" && bid.RootFileID != "" {
		root = bid.RootPath
		b, gerr := conn.GetBucket(ctx, bid.BucketName)
		if gerr != nil {
			if notFound(gerr) {
				policyTarget = false
			} else {
				return nil, toStatus(gerr)
			}
		} else if b == nil || !pathsMatch(b.Path, bid.RootPath) {
			policyTarget = false
		}
		attrs, aerr := conn.FileAttributes(ctx, bid.RootPath)
		if aerr != nil {
			if notFound(aerr) {
				root = ""
				policyTarget = false
			} else {
				return nil, toStatus(aerr)
			}
		} else if attrs.ID != bid.RootFileID {
			root = ""
			policyTarget = false
			d.log.Warn("revoke found a replacement filesystem object; leaving its policy and ACL untouched", "bucket", bid.BucketName, "path", bid.RootPath, "recordedFileID", bid.RootFileID, "liveFileID", attrs.ID)
		}
	}

	if policyTarget {
		sid := naming.PolicySID(aid.Username)
		if err := conn.MutateBucketPolicy(ctx, bid.BucketName, func(p *qumulo.Policy) error {
			policy.RemoveStatementForAuthID(p, sid, authID)
			return nil
		}); err != nil && !notFound(err) {
			return nil, toStatus(err)
		}
	}

	if authID != "" && root != "" {
		if err := conn.RevokeDirectoryAccess(ctx, root, authID); err != nil && !notFound(err) {
			// Keep the user until the ACE is gone. Returning an error makes the
			// sidecar retry the otherwise-idempotent key and policy cleanup.
			return nil, toStatus(fmt.Errorf("revoke filesystem ACE for %s on %s: %w", aid.Username, root, err))
		}
		if aid.RestoreMode != "" {
			if err := conn.PatchFileMode(ctx, root, aid.RestoreMode); err != nil && !notFound(err) {
				return nil, toStatus(fmt.Errorf("restore filesystem mode %s on %s: %w", aid.RestoreMode, root, err))
			}
		}
	}

	if targetUser != nil && naming.IsDriverUser(aid.Username) {
		if err := conn.DeleteUserByID(ctx, targetUser.ID); err != nil && !notFound(err) {
			return nil, toStatus(err)
		}
	}
	d.log.Info("revoked bucket access", "bucket", bid.BucketName, "user", aid.Username)
	return &cosi.DriverRevokeBucketAccessResponse{}, nil
}

// revokeLegacy converges revocation for pre-0.2.0 handles that lack
// immutable identities, using the same name-scoped best-effort cleanup the
// 0.1.0 driver performed live. Every step treats "already gone" as success
// so the sidecar's retries terminate. Usernames are deterministic per
// (bucket_id, access name), so name-scoped deletion cannot hit a grant made
// through a different handle.
func (d *Driver) revokeLegacy(ctx context.Context, conn *qumulo.Connection, bid naming.BucketID, aid naming.AccountID) (*cosi.DriverRevokeBucketAccessResponse, error) {
	user, uerr := conn.GetUserByName(ctx, aid.Username)
	if uerr != nil && !notFound(uerr) {
		return nil, toStatus(fmt.Errorf("lookup local user %s for legacy revoke: %w", aid.Username, uerr))
	}
	if user != nil {
		keys, kerr := conn.ListAccessKeysByOwner(ctx, aid.Username, user.ID)
		if kerr != nil && !notFound(kerr) {
			return nil, toStatus(kerr)
		}
		for _, k := range keys {
			if err := conn.DeleteAccessKey(ctx, k.AccessKeyID); err != nil && !notFound(err) {
				return nil, toStatus(err)
			}
		}
	}
	b, gerr := conn.GetBucket(ctx, bid.BucketName)
	if gerr != nil && !notFound(gerr) {
		return nil, toStatus(gerr)
	}
	if gerr == nil && b != nil {
		sid := naming.PolicySID(aid.Username)
		if err := conn.MutateBucketPolicy(ctx, bid.BucketName, func(p *qumulo.Policy) error {
			policy.RemoveStatement(p, sid)
			return nil
		}); err != nil && !notFound(err) {
			return nil, toStatus(err)
		}
		if user != nil && b.Path != "" {
			if err := conn.RevokeDirectoryAccess(ctx, b.Path, user.ID); err != nil && !notFound(err) {
				return nil, toStatus(fmt.Errorf("revoke filesystem ACE for %s on %s: %w", aid.Username, b.Path, err))
			}
		}
	}
	if user != nil && naming.IsDriverUser(aid.Username) {
		if err := conn.DeleteUserByID(ctx, user.ID); err != nil && !notFound(err) {
			return nil, toStatus(err)
		}
	}
	d.log.Info("revoked bucket access via legacy handle", "bucket", bid.BucketName, "user", aid.Username)
	return &cosi.DriverRevokeBucketAccessResponse{}, nil
}

func (d *Driver) rotateKeys(ctx context.Context, conn *qumulo.Connection, username, authID string) error {
	keys, err := conn.ListAccessKeysByOwner(ctx, username, authID)
	if err != nil && !notFound(err) {
		return err
	}
	for _, k := range keys {
		if err := conn.DeleteAccessKey(ctx, k.AccessKeyID); err != nil && !notFound(err) {
			return err
		}
	}
	return nil
}

const accessKeyCleanupTimeout = 30 * time.Second

func accessKeyCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), accessKeyCleanupTimeout)
}

func (d *Driver) applyQuota(ctx context.Context, conn *qumulo.Connection, path string, limit int64) error {
	attrs, err := conn.FileAttributes(ctx, path)
	if err != nil {
		return fmt.Errorf("lookup bucket root for quota: %w", err)
	}
	if err := conn.CreateQuota(ctx, attrs.ID, limit); err != nil {
		return fmt.Errorf("set quota: %w", err)
	}
	return nil
}

func (d *Driver) ensureFeatures(ctx context.Context, conn *qumulo.Connection, class config.Class) error {
	rev, err := conn.Version(ctx)
	if err != nil {
		return err
	}
	need := map[string]bool{}
	if class.ObjectLockEnabled {
		need[qumulo.FeatureObjectLock] = true
		need[qumulo.FeatureVersioning] = true
	}
	if class.Versioning == "Enabled" || class.Versioning == "Suspended" {
		need[qumulo.FeatureVersioning] = true
	}
	for feat := range need {
		ok, min, err := qumulo.Supports(rev, feat)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("class requests %s, which requires Qumulo Core %s (cluster is %s)", feat, min, rev)
		}
	}
	return nil
}

func (d *Driver) lockDownLegacy(ctx context.Context, conn *qumulo.Connection, bucket string) error {
	// Empty-principal deny-by-default stand-in: a policy with no Allow
	// statements. Creator + RBAC still have access via Core defaults.
	return conn.PutBucketPolicy(ctx, bucket, qumulo.EmptyPolicy(), "")
}

func notFound(err error) bool {
	if api, ok := qumulo.AsAPIError(err); ok {
		return api.IsNotFound()
	}
	return false
}

func alreadyExists(err error) bool {
	if api, ok := qumulo.AsAPIError(err); ok {
		return api.IsAlreadyExists()
	}
	return false
}

func classPrivateUnsupported(err error) bool {
	if api, ok := qumulo.AsAPIError(err); ok {
		if api.StatusCode == http.StatusBadRequest || api.ErrorClass == qumulo.ErrClassRESTInvalidRequest {
			msg := strings.ToLower(api.Description + " " + api.ErrorClass)
			return strings.Contains(msg, "private") || strings.Contains(msg, "unknown field") || strings.Contains(msg, "unexpected")
		}
	}
	return false
}

func pathsMatch(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

func bindClassToBucketID(class config.Class, id naming.BucketID) (config.Class, error) {
	if class.Endpoint == "" {
		class.Endpoint = id.Endpoint
	}
	if class.RESTPort == "" {
		class.RESTPort = id.RESTPort
	}
	if !sameClusterEndpoint(class.Endpoint, class.RESTPort, id.Endpoint, id.RESTPort) {
		return class, status.Errorf(codes.InvalidArgument, "class endpoint %q:%s does not match bucket_id endpoint %q:%s", class.Endpoint, class.RESTPort, id.Endpoint, id.RESTPort)
	}
	return class, nil
}

func sameClusterEndpoint(got, gotDefaultPort, want, wantPort string) bool {
	gotHost, gotPort := naming.HostPort(got, gotDefaultPort)
	wantHost, resolvedWantPort := naming.HostPort(want, wantPort)
	return strings.EqualFold(strings.TrimSuffix(gotHost, "."), strings.TrimSuffix(wantHost, ".")) && gotPort == resolvedWantPort
}

func randomPassword() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// Core 7.9.2.2 requires 3 of: lower, upper, digit, special.
	// Hex alone is only lower+digit and is rejected.
	return "Aa1!" + hex.EncodeToString(b[:]), nil
}
