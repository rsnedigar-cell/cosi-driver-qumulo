// Package naming implements deterministic, idempotent bucket and account
// identifiers for the Qumulo COSI driver.
//
// Bucket IDs are versioned opaque strings (same idea as the CSI driver's
// volume IDs) so connection identity survives RPCs that only receive
// bucket_id / account_id:
//
//	q3:qumulo:<endpoint>:<restPort>:<bucketName>:<rootPath>:<rootFileID>:managed
//	q2:qumulo:<endpoint>:<restPort>:<bucketName>:<rootPath>:<rootFileID> (legacy decoder)
//	q1:qumulo:<endpoint>:<restPort>:<bucketName> (legacy decoder)
//	q3:qumulo:<endpoint>:<username>:<accessKeyID-prefix>:<authID>:<restoreMode>
//	q2:qumulo:<endpoint>:<username>:<accessKeyID-prefix>:<authID>
//	q1:qumulo:<endpoint>:<username>:<accessKeyID-prefix> (legacy decoder)
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"
)

const (
	// IDVersion is the opaque-ID format prefix. Bump and keep a decoder
	// for previous versions if the encoding ever changes.
	IDVersion = "q1"
	// BucketIDVersion adds immutable filesystem identity so purge retries
	// remain safe after the S3 bucket has already been unregistered.
	BucketIDVersion = "q2"
	// ManagedBucketIDVersion additionally records that the driver created the
	// fingerprinted root. Older handles are intentionally treated as retained
	// data because their ownership cannot be proven from the handle alone.
	ManagedBucketIDVersion = "q3"
	// AccountIDAuthVersion adds the immutable Qumulo auth id so revocation
	// does not depend on a same-named local user continuing to exist.
	AccountIDAuthVersion = "q2"
	// AccountIDVersion additionally records a POSIX mode that must be restored
	// when an explicitly enabled chmod grant fallback is revoked.
	AccountIDVersion = "q3"

	// DriverName is the COSI driverName that must appear on BucketClass
	// and BucketAccessClass objects.
	DriverName = "s3.qumulo.com"

	// S3ProtocolKey is the credentials map key the sidecar serializes
	// into BucketInfo.spec.secretS3.
	S3ProtocolKey = "s3"

	// Credential secret keys expected by the v0.2.2 sidecar / BucketInfo.
	SecretAccessKeyID     = "accessKeyID"
	SecretAccessSecretKey = "accessSecretKey"
	SecretEndpoint        = "endpoint"
	SecretRegion          = "region"

	DefaultRESTPort = "8000"
	DefaultS3Port   = "9000"
	DefaultRegion   = "us-east-1"

	minBucketLen = 3
	maxBucketLen = 63
	userPrefix   = "cosi-"
	userHashLen  = 32
	// Accept older 48-bit usernames during revoke while minting only 128-bit
	// identities for new grants.
	legacyUserHashLen = 12
	nameHashLen       = 32
)

// BucketID is a versioned opaque identifier for a provisioned bucket.
type BucketID struct {
	Endpoint   string
	RESTPort   string
	BucketName string
	RootPath   string
	RootFileID string
	Managed    bool
}

func (id BucketID) String() string {
	if id.RootPath != "" && id.RootFileID != "" {
		if id.Managed {
			return fmt.Sprintf("%s:qumulo:%s:%s:%s:%s:%s:managed",
				ManagedBucketIDVersion,
				url.QueryEscape(id.Endpoint),
				id.RESTPort,
				url.QueryEscape(id.BucketName),
				url.QueryEscape(id.RootPath),
				url.QueryEscape(id.RootFileID),
			)
		}
		return fmt.Sprintf("%s:qumulo:%s:%s:%s:%s:%s",
			BucketIDVersion,
			url.QueryEscape(id.Endpoint),
			id.RESTPort,
			url.QueryEscape(id.BucketName),
			url.QueryEscape(id.RootPath),
			url.QueryEscape(id.RootFileID),
		)
	}
	return fmt.Sprintf("%s:qumulo:%s:%s:%s", IDVersion, url.QueryEscape(id.Endpoint), id.RESTPort, id.BucketName)
}

// ParseBucketID decodes a versioned bucket_id.
func ParseBucketID(raw string) (BucketID, error) {
	if strings.HasPrefix(raw, ManagedBucketIDVersion+":qumulo:") {
		parts := strings.SplitN(raw, ":", 8)
		if len(parts) != 8 || parts[0] != ManagedBucketIDVersion || parts[1] != "qumulo" || parts[7] != "managed" {
			return BucketID{}, fmt.Errorf("invalid bucket_id %q (want %s:qumulo:<endpoint>:<restPort>:<bucketName>:<rootPath>:<rootFileID>:managed)", raw, ManagedBucketIDVersion)
		}
		for _, i := range []int{2, 3, 4, 5, 6} {
			if parts[i] == "" {
				return BucketID{}, fmt.Errorf("invalid bucket_id %q: empty field", raw)
			}
		}
		ep, err := url.QueryUnescape(parts[2])
		if err != nil {
			return BucketID{}, fmt.Errorf("invalid bucket_id endpoint encoding: %w", err)
		}
		name, err := url.QueryUnescape(parts[4])
		if err != nil {
			return BucketID{}, fmt.Errorf("invalid bucket_id bucket-name encoding: %w", err)
		}
		root, err := url.QueryUnescape(parts[5])
		if err != nil {
			return BucketID{}, fmt.Errorf("invalid bucket_id root-path encoding: %w", err)
		}
		fileID, err := url.QueryUnescape(parts[6])
		if err != nil {
			return BucketID{}, fmt.Errorf("invalid bucket_id root-file-id encoding: %w", err)
		}
		if !strings.HasPrefix(root, "/") {
			return BucketID{}, fmt.Errorf("invalid bucket_id %q: root path must be absolute", raw)
		}
		return BucketID{Endpoint: ep, RESTPort: parts[3], BucketName: name, RootPath: root, RootFileID: fileID, Managed: true}, nil
	}

	if strings.HasPrefix(raw, BucketIDVersion+":qumulo:") {
		parts := strings.SplitN(raw, ":", 7)
		if len(parts) != 7 || parts[0] != BucketIDVersion || parts[1] != "qumulo" {
			return BucketID{}, fmt.Errorf("invalid bucket_id %q (want %s:qumulo:<endpoint>:<restPort>:<bucketName>:<rootPath>:<rootFileID>)", raw, BucketIDVersion)
		}
		for _, i := range []int{2, 3, 4, 5, 6} {
			if parts[i] == "" {
				return BucketID{}, fmt.Errorf("invalid bucket_id %q: empty field", raw)
			}
		}
		ep, err := url.QueryUnescape(parts[2])
		if err != nil {
			return BucketID{}, fmt.Errorf("invalid bucket_id endpoint encoding: %w", err)
		}
		name, err := url.QueryUnescape(parts[4])
		if err != nil {
			return BucketID{}, fmt.Errorf("invalid bucket_id bucket-name encoding: %w", err)
		}
		root, err := url.QueryUnescape(parts[5])
		if err != nil {
			return BucketID{}, fmt.Errorf("invalid bucket_id root-path encoding: %w", err)
		}
		fileID, err := url.QueryUnescape(parts[6])
		if err != nil {
			return BucketID{}, fmt.Errorf("invalid bucket_id root-file-id encoding: %w", err)
		}
		if !strings.HasPrefix(root, "/") {
			return BucketID{}, fmt.Errorf("invalid bucket_id %q: root path must be absolute", raw)
		}
		return BucketID{Endpoint: ep, RESTPort: parts[3], BucketName: name, RootPath: root, RootFileID: fileID}, nil
	}

	parts := strings.SplitN(raw, ":", 5)
	if len(parts) != 5 || parts[0] != IDVersion || parts[1] != "qumulo" {
		return BucketID{}, fmt.Errorf("invalid bucket_id %q (want %s:qumulo:<endpoint>:<restPort>:<bucketName>)", raw, IDVersion)
	}
	if parts[2] == "" || parts[3] == "" || parts[4] == "" {
		return BucketID{}, fmt.Errorf("invalid bucket_id %q: empty field", raw)
	}
	ep, err := url.QueryUnescape(parts[2])
	if err != nil {
		return BucketID{}, fmt.Errorf("invalid bucket_id endpoint encoding: %w", err)
	}
	return BucketID{Endpoint: ep, RESTPort: parts[3], BucketName: parts[4]}, nil
}

// AccountID is a versioned opaque identifier for a granted access.
type AccountID struct {
	Endpoint     string
	Username     string
	AccessKeyPfx string
	AuthID       string
	RestoreMode  string
}

func (id AccountID) String() string {
	if id.AuthID != "" && id.RestoreMode != "" {
		return fmt.Sprintf("%s:qumulo:%s:%s:%s:%s:%s",
			AccountIDVersion,
			url.QueryEscape(id.Endpoint),
			url.QueryEscape(id.Username),
			url.QueryEscape(id.AccessKeyPfx),
			url.QueryEscape(id.AuthID),
			url.QueryEscape(id.RestoreMode),
		)
	}
	if id.AuthID != "" {
		return fmt.Sprintf("%s:qumulo:%s:%s:%s:%s",
			AccountIDAuthVersion,
			url.QueryEscape(id.Endpoint),
			url.QueryEscape(id.Username),
			url.QueryEscape(id.AccessKeyPfx),
			url.QueryEscape(id.AuthID),
		)
	}
	return fmt.Sprintf("%s:qumulo:%s:%s:%s", IDVersion, url.QueryEscape(id.Endpoint), id.Username, id.AccessKeyPfx)
}

// ParseAccountID decodes a versioned account_id.
func ParseAccountID(raw string) (AccountID, error) {
	if strings.HasPrefix(raw, AccountIDVersion+":qumulo:") {
		parts := strings.SplitN(raw, ":", 7)
		if len(parts) != 7 || parts[0] != AccountIDVersion || parts[1] != "qumulo" {
			return AccountID{}, fmt.Errorf("invalid account_id %q (want %s:qumulo:<endpoint>:<username>:<accessKeyPrefix>:<authID>:<restoreMode>)", raw, AccountIDVersion)
		}
		for _, i := range []int{2, 3, 5, 6} {
			if parts[i] == "" {
				return AccountID{}, fmt.Errorf("invalid account_id %q: empty field", raw)
			}
		}
		decoded := make([]string, 5)
		for out, in := range []int{2, 3, 4, 5, 6} {
			value, err := url.QueryUnescape(parts[in])
			if err != nil {
				return AccountID{}, fmt.Errorf("invalid account_id field encoding: %w", err)
			}
			decoded[out] = value
		}
		if !validRestoreMode(decoded[4]) {
			return AccountID{}, fmt.Errorf("invalid account_id restore mode %q", decoded[4])
		}
		return AccountID{Endpoint: decoded[0], Username: decoded[1], AccessKeyPfx: decoded[2], AuthID: decoded[3], RestoreMode: decoded[4]}, nil
	}

	if strings.HasPrefix(raw, AccountIDAuthVersion+":qumulo:") {
		parts := strings.SplitN(raw, ":", 6)
		if len(parts) != 6 || parts[0] != AccountIDAuthVersion || parts[1] != "qumulo" {
			return AccountID{}, fmt.Errorf("invalid account_id %q (want %s:qumulo:<endpoint>:<username>:<accessKeyPrefix>:<authID>)", raw, AccountIDAuthVersion)
		}
		for _, i := range []int{2, 3, 5} {
			if parts[i] == "" {
				return AccountID{}, fmt.Errorf("invalid account_id %q: empty field", raw)
			}
		}
		ep, err := url.QueryUnescape(parts[2])
		if err != nil {
			return AccountID{}, fmt.Errorf("invalid account_id endpoint encoding: %w", err)
		}
		username, err := url.QueryUnescape(parts[3])
		if err != nil {
			return AccountID{}, fmt.Errorf("invalid account_id username encoding: %w", err)
		}
		keyPrefix, err := url.QueryUnescape(parts[4])
		if err != nil {
			return AccountID{}, fmt.Errorf("invalid account_id access-key encoding: %w", err)
		}
		authID, err := url.QueryUnescape(parts[5])
		if err != nil {
			return AccountID{}, fmt.Errorf("invalid account_id auth-id encoding: %w", err)
		}
		return AccountID{Endpoint: ep, Username: username, AccessKeyPfx: keyPrefix, AuthID: authID}, nil
	}

	parts := strings.SplitN(raw, ":", 5)
	if len(parts) != 5 || parts[0] != IDVersion || parts[1] != "qumulo" {
		return AccountID{}, fmt.Errorf("invalid account_id %q (want %s:qumulo:<endpoint>:<username>:<accessKeyPrefix>)", raw, IDVersion)
	}
	if parts[2] == "" || parts[3] == "" {
		return AccountID{}, fmt.Errorf("invalid account_id %q: empty field", raw)
	}
	ep, err := url.QueryUnescape(parts[2])
	if err != nil {
		return AccountID{}, fmt.Errorf("invalid account_id endpoint encoding: %w", err)
	}
	return AccountID{Endpoint: ep, Username: parts[3], AccessKeyPfx: parts[4]}, nil
}

func validRestoreMode(mode string) bool {
	if len(mode) != 4 || mode[0] != '0' {
		return false
	}
	for _, r := range mode[1:] {
		if r < '0' || r > '7' {
			return false
		}
	}
	return true
}

// AccessKeyPrefix returns a non-secret prefix of an access key id for encoding
// into account_id. Never include the secret key.
func AccessKeyPrefix(accessKeyID string) string {
	if len(accessKeyID) <= 8 {
		return accessKeyID
	}
	return accessKeyID[:8]
}

// BucketName derives a Qumulo-legal S3 bucket name from a COSI Bucket name
// and an optional class prefix.
//
// Algorithm (deterministic, idempotent):
//
//	name = clamp63(prefix + sanitize(input))
//
// If sanitization changed anything or clamping truncated, append "-" + first
// 32 hex chars of SHA-256(original input) so retries and collisions stay
// deterministic. Amazon S3 rules minus periods: 3–63 chars, lowercase
// letters/digits/hyphens, must start and end with a letter or digit.
func BucketName(prefix, input string) (string, error) {
	sanitized := Sanitize(input)
	combined := prefix + sanitized
	changed := sanitized != strings.ToLower(input) || strings.Contains(input, ".")
	truncated := false
	if len(combined) > maxBucketLen {
		truncated = true
	}
	if changed || truncated {
		suffix := "-" + hashHex(input, nameHashLen)
		keep := maxBucketLen - len(suffix)
		if keep < minBucketLen {
			return "", fmt.Errorf("bucket prefix %q leaves no room for a legal name", prefix)
		}
		base := combined
		if len(base) > keep {
			base = base[:keep]
		}
		base = strings.TrimRight(base, "-")
		combined = base + suffix
	}
	combined = strings.Trim(combined, "-")
	if len(combined) < minBucketLen {
		return "", fmt.Errorf("bucket name %q is shorter than %d characters after sanitizing", combined, minBucketLen)
	}
	if len(combined) > maxBucketLen {
		return "", fmt.Errorf("bucket name %q exceeds %d characters", combined, maxBucketLen)
	}
	if err := ValidateBucketName(combined); err != nil {
		return "", err
	}
	return combined, nil
}

// Sanitize lowercases, maps invalid characters to '-', strips periods, and
// collapses / trims hyphens.
func Sanitize(input string) string {
	var b strings.Builder
	b.Grow(len(input))
	prevHyphen := false
	for _, r := range strings.ToLower(input) {
		switch {
		case r == '.':
			continue
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// ValidateBucketName enforces Qumulo's S3 naming rules (Amazon S3 minus periods).
func ValidateBucketName(name string) error {
	if len(name) < minBucketLen || len(name) > maxBucketLen {
		return fmt.Errorf("bucket name %q must be %d–%d characters", name, minBucketLen, maxBucketLen)
	}
	if strings.Contains(name, ".") {
		return fmt.Errorf("bucket name %q must not contain periods (Qumulo S3 restriction)", name)
	}
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			return fmt.Errorf("bucket name %q must be lowercase", name)
		}
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return fmt.Errorf("bucket name %q contains invalid character %q", name, r)
		}
		if (i == 0 || i == len(name)-1) && r == '-' {
			return fmt.Errorf("bucket name %q must start and end with a letter or digit", name)
		}
	}
	return nil
}

// Username derives the per-BucketAccess local user:
//
//	cosi- + first 32 hex chars of sha256(bucket_id + ":" + access_name)
func Username(bucketID, accessName string) string {
	return userPrefix + hashHex(bucketID+":"+accessName, userHashLen)
}

// IsDriverUser reports whether a local username was minted by this driver.
func IsDriverUser(username string) bool {
	if !strings.HasPrefix(username, userPrefix) {
		return false
	}
	rest := username[len(userPrefix):]
	if len(rest) != userHashLen && len(rest) != legacyUserHashLen {
		return false
	}
	for _, r := range rest {
		if !unicode.Is(unicode.ASCII_Hex_Digit, r) {
			return false
		}
	}
	return true
}

// PolicySID is the bucket-policy statement id for a driver-managed grant.
func PolicySID(username string) string {
	return userPrefix + username
}

// RootPath joins basePath and bucketName into a Qumulo filesystem path.
func RootPath(basePath, bucketName string) string {
	base := strings.TrimRight(basePath, "/")
	if base == "" {
		base = "/"
	}
	if base == "/" {
		return "/" + bucketName
	}
	return base + "/" + bucketName
}

// HostPort splits a host[:port] pair, defaulting the port.
func HostPort(endpoint, defaultPort string) (host, port string) {
	endpoint = strings.TrimSpace(endpoint)
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimRight(endpoint, "/")
	if host, p, err := net.SplitHostPort(endpoint); err == nil {
		return host, p
	}
	return endpoint, defaultPort
}

// RESTBaseURL builds https://host:restPort from a class/driver endpoint.
func RESTBaseURL(endpoint, restPort string) string {
	host, port := HostPort(endpoint, firstNonEmpty(restPort, DefaultRESTPort))
	if restPort != "" {
		port = restPort
	}
	return "https://" + net.JoinHostPort(host, port)
}

// S3Endpoint is the data-plane URL reported to applications.
// Qumulo's S3 listener is fixed at TCP 9000.
func S3Endpoint(endpoint, s3Port string) string {
	host, _ := HostPort(endpoint, DefaultRESTPort)
	port := firstNonEmpty(s3Port, DefaultS3Port)
	return "https://" + net.JoinHostPort(host, port)
}

// ParseURL is a small helper for tests and config validation.
func ParseURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

func hashHex(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
