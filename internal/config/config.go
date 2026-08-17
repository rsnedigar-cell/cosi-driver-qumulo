package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/naming"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/policy"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

const (
	DefaultCredentialsDir = "/etc/qumulo/credentials"
	EnvEndpoint           = "QUMULO_ENDPOINT"
	EnvRESTPort           = "QUMULO_REST_PORT"
	EnvS3Port             = "QUMULO_S3_PORT"
	EnvRegion             = "QUMULO_REGION"
	EnvBasePath           = "QUMULO_BASE_PATH"
	EnvVersionFloor       = "QUMULO_VERSION_FLOOR"
	EnvInsecureTLS        = "QUMULO_INSECURE_SKIP_TLS_VERIFY"
	EnvCAFile             = "QUMULO_CA_FILE"
)

// Driver is process-level configuration.
type Driver struct {
	Name             string
	Address          string
	MetricsAddress   string
	DefaultEndpoint  string
	DefaultRESTPort  string
	DefaultS3Port    string
	DefaultRegion    string
	DefaultBasePath  string
	VersionFloor     string
	CredentialsDir   string
	InsecureSkipTLS  bool
	CAFile           string
	Kubeconfig       string
	SecretNamespaces []string // empty = driver's own namespace only
	DriverNamespace  string
}

func FromEnv() Driver {
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "qumulo-cosi"
	}
	insecure, _ := strconv.ParseBool(os.Getenv(EnvInsecureTLS))
	floor := os.Getenv(EnvVersionFloor)
	if floor == "" {
		floor = qumulo.DefaultFloor
	}
	rest := os.Getenv(EnvRESTPort)
	if rest == "" {
		rest = naming.DefaultRESTPort
	}
	s3p := os.Getenv(EnvS3Port)
	if s3p == "" {
		s3p = naming.DefaultS3Port
	}
	region := os.Getenv(EnvRegion)
	if region == "" {
		region = naming.DefaultRegion
	}
	base := os.Getenv(EnvBasePath)
	if base == "" {
		base = "/k8s-buckets"
	}
	allow := os.Getenv("QUMULO_SECRET_NAMESPACES")
	var extra []string
	if allow != "" {
		for _, s := range strings.Split(allow, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				extra = append(extra, s)
			}
		}
	}
	return Driver{
		Name:             naming.DriverName,
		Address:          "unix:///var/lib/cosi/cosi.sock",
		MetricsAddress:   ":8081",
		DefaultEndpoint:  os.Getenv(EnvEndpoint),
		DefaultRESTPort:  rest,
		DefaultS3Port:    s3p,
		DefaultRegion:    region,
		DefaultBasePath:  base,
		VersionFloor:     floor,
		CredentialsDir:   DefaultCredentialsDir,
		InsecureSkipTLS:  insecure,
		CAFile:           os.Getenv(EnvCAFile),
		DriverNamespace:  ns,
		SecretNamespaces: extra,
	}
}

// Class is the resolved per-RPC class parameters.
type Class struct {
	Endpoint                   string
	RESTPort                   string
	S3Port                     string
	Region                     string
	CredentialsSecretName      string
	CredentialsSecretNamespace string
	BasePath                   string
	BucketPrefix               string
	DeleteRootDir              bool
	PurgeOnDelete              bool
	QuotaLimit                 int64
	Versioning                 string
	ObjectLockEnabled          bool
	ExistingBucketName         string
	InsecureSkipTLSVerify      bool
	AccessMode                 string
	ACLFallbackChmod           bool
	CABundlePEM                []byte
}

func ParseClass(params map[string]string, d Driver) (Class, error) {
	if params == nil {
		params = map[string]string{}
	}
	for key := range params {
		if _, ok := classParameterKeys[key]; !ok {
			return Class{}, fmt.Errorf("unknown class parameter %q", key)
		}
	}
	return parseClassValidated(params, d)
}

// ParseClassForCleanup parses the parameter map replayed on DeleteBucket and
// RevokeBucketAccess. Cleanup must stay reachable even when the stored
// context carries keys a newer (or older) driver no longer recognizes, so
// unknown keys are dropped instead of failing the RPC — a Bucket must never
// become undeletable because of a parameter its class once accepted.
func ParseClassForCleanup(params map[string]string, d Driver) (Class, error) {
	filtered := map[string]string{}
	for key, value := range params {
		if _, ok := classParameterKeys[key]; ok {
			filtered[key] = value
		}
	}
	return parseClassValidated(filtered, d)
}

func parseClassValidated(params map[string]string, d Driver) (Class, error) {
	regionInput := d.DefaultRegion
	if raw, ok := params["region"]; ok {
		regionInput = raw
	}
	if regionInput == "" {
		regionInput = naming.DefaultRegion
	}
	if strings.IndexFunc(regionInput, unicode.IsControl) >= 0 {
		return Class{}, fmt.Errorf("region %q contains control characters", regionInput)
	}
	basePathInput := d.DefaultBasePath
	if raw, ok := params["basePath"]; ok {
		basePathInput = raw
	}
	if basePathInput == "" {
		basePathInput = "/k8s-buckets"
	}
	if strings.IndexFunc(basePathInput, unicode.IsControl) >= 0 {
		return Class{}, fmt.Errorf("basePath %q contains control characters", basePathInput)
	}
	c := Class{
		Endpoint:                   first(params["endpoint"], d.DefaultEndpoint),
		RESTPort:                   first(params["restPort"], d.DefaultRESTPort, naming.DefaultRESTPort),
		S3Port:                     first(params["s3Port"], d.DefaultS3Port, naming.DefaultS3Port),
		Region:                     strings.TrimSpace(regionInput),
		CredentialsSecretName:      params["credentialsSecretName"],
		CredentialsSecretNamespace: first(params["credentialsSecretNamespace"], d.DriverNamespace),
		BasePath:                   strings.TrimSpace(basePathInput),
		BucketPrefix:               params["bucketPrefix"],
		ExistingBucketName:         params["existingBucketName"],
		AccessMode:                 first(params["accessMode"], "rw"),
		Versioning:                 params["versioning"],
	}
	var err error
	if c.RESTPort, err = validatedPort("restPort", c.RESTPort); err != nil {
		return c, err
	}
	if c.S3Port, err = validatedPort("s3Port", c.S3Port); err != nil {
		return c, err
	}
	if c.BasePath, err = validatedBasePath(c.BasePath); err != nil {
		return c, err
	}
	if c.AccessMode, err = policy.NormalizeMode(c.AccessMode); err != nil {
		return c, err
	}
	// Booleans parse strictly: a malformed value is an error, never a silent
	// default. deleteRootDir defaults to true, so a typo like "flase" must
	// not quietly enable data deletion.
	if c.DeleteRootDir, err = parseBool(params, "deleteRootDir", true); err != nil {
		return c, err
	}
	if c.PurgeOnDelete, err = parseBool(params, "purgeOnDelete", false); err != nil {
		return c, err
	}
	if c.ObjectLockEnabled, err = parseBool(params, "objectLockEnabled", false); err != nil {
		return c, err
	}
	if c.InsecureSkipTLSVerify, err = parseBool(params, "insecureSkipTLSVerify", d.InsecureSkipTLS); err != nil {
		return c, err
	}
	if c.ACLFallbackChmod, err = parseBool(params, "aclFallbackChmod", false); err != nil {
		return c, err
	}
	if q := strings.TrimSpace(params["quotaLimit"]); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil {
			return c, fmt.Errorf("quotaLimit %q: %w", q, err)
		}
		if n <= 0 {
			return c, fmt.Errorf("quotaLimit %q must be greater than zero when set", q)
		}
		c.QuotaLimit = n
	}
	if c.ObjectLockEnabled && c.Versioning == "" {
		c.Versioning = "Enabled"
	}
	if c.Versioning != "" && c.Versioning != "Enabled" && c.Versioning != "Suspended" && c.Versioning != "Unversioned" {
		return c, fmt.Errorf("versioning %q: want Enabled, Suspended, or Unversioned", c.Versioning)
	}
	return c, nil
}

var classParameterKeys = map[string]struct{}{
	"endpoint": {}, "restPort": {}, "s3Port": {}, "region": {},
	"credentialsSecretName": {}, "credentialsSecretNamespace": {},
	"basePath": {}, "bucketPrefix": {}, "existingBucketName": {},
	"deleteRootDir": {}, "purgeOnDelete": {}, "quotaLimit": {},
	"versioning": {}, "objectLockEnabled": {}, "insecureSkipTLSVerify": {},
	"accessMode": {}, "aclFallbackChmod": {},
}

func validatedPort(name, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%s %q must be a numeric TCP port from 1 through 65535", name, value)
		}
	}
	n, err := strconv.ParseUint(value, 10, 16)
	if err != nil || n == 0 {
		return "", fmt.Errorf("%s %q must be a numeric TCP port from 1 through 65535", name, value)
	}
	return strconv.FormatUint(n, 10), nil
}

func validatedBasePath(value string) (string, error) {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("basePath %q contains control characters", value)
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("basePath %q must be an absolute POSIX path", value)
	}
	clean := path.Clean(value)
	if clean == "/" {
		return "", fmt.Errorf("basePath must not be the filesystem root")
	}
	if clean != value {
		return "", fmt.Errorf("basePath %q is not clean; use %q", value, clean)
	}
	return clean, nil
}

func parseBool(params map[string]string, key string, def bool) (bool, error) {
	v, ok := params[key]
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def, fmt.Errorf("parameter %s=%q is not a valid boolean (use \"true\" or \"false\")", key, v)
	}
	return b, nil
}

func first(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// SecretReader fetches a Kubernetes Secret's data.
type SecretReader interface {
	GetSecret(ctx context.Context, namespace, name string) (map[string][]byte, error)
}

// ResolveCredentials returns cluster credentials for a class, preferring
// a referenced Secret then the mounted default directory / env.
func ResolveCredentials(ctx context.Context, class Class, d Driver, secrets SecretReader) (qumulo.Credentials, []byte, error) {
	if class.CredentialsSecretName != "" {
		if secrets == nil {
			return qumulo.Credentials{}, nil, fmt.Errorf("credentialsSecretName set but no Kubernetes client configured")
		}
		ns := class.CredentialsSecretNamespace
		if !namespaceAllowed(ns, d) {
			return qumulo.Credentials{}, nil, fmt.Errorf("refusing to read secret %s/%s: namespace not allowed (driver namespace %q, extras %v)", ns, class.CredentialsSecretName, d.DriverNamespace, d.SecretNamespaces)
		}
		data, err := secrets.GetSecret(ctx, ns, class.CredentialsSecretName)
		if err != nil {
			return qumulo.Credentials{}, nil, fmt.Errorf("read credentials secret %s/%s: %w", ns, class.CredentialsSecretName, err)
		}
		c, ca, err := credsFromMap(data)
		if err != nil {
			return c, ca, err
		}
		ca, err = configuredCABundle(ca, d.CAFile)
		return c, ca, err
	}
	dir := d.CredentialsDir
	var mountedCA []byte
	if dir != "" {
		c, ca, err := credsFromDir(dir)
		if err != nil {
			return c, nil, fmt.Errorf("read credentials directory %q: %w", dir, err)
		}
		mountedCA = ca
		if c.HasToken() || c.HasPassword() {
			ca, err = configuredCABundle(ca, d.CAFile)
			return c, ca, err
		}
	}
	tok := os.Getenv("QUMULO_TOKEN")
	user := os.Getenv("QUMULO_USERNAME")
	pass := os.Getenv("QUMULO_PASSWORD")
	c := qumulo.Credentials{Token: tok, Username: user, Password: pass}
	hasUser := strings.TrimSpace(user) != ""
	hasPassword := strings.TrimSpace(pass) != ""
	if hasUser != hasPassword {
		return c, nil, fmt.Errorf("QUMULO_USERNAME and QUMULO_PASSWORD must either both be set or both be unset")
	}
	if c.HasToken() && (hasUser || hasPassword) {
		return c, nil, fmt.Errorf("environment credentials cannot combine QUMULO_TOKEN with QUMULO_USERNAME/QUMULO_PASSWORD")
	}
	ca, err := configuredCABundle(mountedCA, d.CAFile)
	if err != nil {
		return c, nil, err
	}
	if !c.HasToken() && !c.HasPassword() {
		return c, ca, fmt.Errorf("no Qumulo credentials: set class credentialsSecretName or mount %s or export QUMULO_TOKEN", dir)
	}
	return c, ca, nil
}

// configuredCABundle preserves a CA supplied alongside credentials and only
// falls back to the process-level CA file when those credentials do not carry
// one. An explicitly configured CA file must be readable and non-empty: silently
// falling back to the host trust store would connect under a different trust
// policy than the operator requested.
func configuredCABundle(ca []byte, path string) ([]byte, error) {
	if len(ca) > 0 || strings.TrimSpace(path) == "" {
		return ca, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA file %q: %w", path, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil, fmt.Errorf("read CA file %q: file is empty", path)
	}
	return b, nil
}

func namespaceAllowed(ns string, d Driver) bool {
	if ns == "" || ns == d.DriverNamespace {
		return true
	}
	for _, extra := range d.SecretNamespaces {
		if extra == ns || extra == "*" {
			return true
		}
	}
	return false
}

func credsFromMap(data map[string][]byte) (qumulo.Credentials, []byte, error) {
	c := qumulo.Credentials{
		Token:    stringFrom(data, "token", "accessToken", "bearer"),
		Username: stringFrom(data, "username", "user"),
		Password: stringFrom(data, "password"),
	}
	tokenPresent := hasAnyMapKey(data, "token", "accessToken", "bearer")
	usernamePresent := hasAnyMapKey(data, "username", "user")
	passwordPresent := hasAnyMapKey(data, "password")
	if usernamePresent != passwordPresent {
		return c, nil, fmt.Errorf("secret username and password must either both be present or both be absent")
	}
	if tokenPresent && (usernamePresent || passwordPresent) {
		return c, nil, fmt.Errorf("secret cannot combine token credentials with username/password")
	}
	for _, key := range []string{"ca.crt", "ca.pem"} {
		if raw, ok := data[key]; ok && strings.TrimSpace(string(raw)) == "" {
			return c, nil, fmt.Errorf("secret key %q is present but empty", key)
		}
	}
	ca, ok := data["ca.crt"]
	if !ok {
		ca = data["ca.pem"]
	}
	if !c.HasToken() && !c.HasPassword() {
		return c, ca, fmt.Errorf("secret has neither token nor username/password")
	}
	return c, ca, nil
}

func hasAnyMapKey(data map[string][]byte, keys ...string) bool {
	for _, key := range keys {
		if _, ok := data[key]; ok {
			return true
		}
	}
	return false
}

func stringFrom(data map[string][]byte, keys ...string) string {
	for _, k := range keys {
		if v, ok := data[k]; ok && len(v) > 0 {
			return strings.TrimSpace(string(v))
		}
	}
	return ""
}

func credsFromDir(dir string) (qumulo.Credentials, []byte, error) {
	read := func(name string) (string, bool, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			return "", false, nil
		}
		if err != nil {
			return "", true, fmt.Errorf("read %q: %w", filepath.Join(dir, name), err)
		}
		value := strings.TrimSpace(string(b))
		if value == "" {
			return "", true, fmt.Errorf("credential file %q is present but empty", filepath.Join(dir, name))
		}
		return value, true, nil
	}
	token, tokenPresent, err := read("token")
	if err != nil {
		return qumulo.Credentials{}, nil, err
	}
	accessToken, accessTokenPresent, err := read("accessToken")
	if err != nil {
		return qumulo.Credentials{}, nil, err
	}
	username, usernamePresent, err := read("username")
	if err != nil {
		return qumulo.Credentials{}, nil, err
	}
	password, passwordPresent, err := read("password")
	if err != nil {
		return qumulo.Credentials{}, nil, err
	}
	if usernamePresent != passwordPresent {
		return qumulo.Credentials{}, nil, fmt.Errorf("mounted username and password must either both be present or both be absent")
	}
	if (tokenPresent || accessTokenPresent) && (usernamePresent || passwordPresent) {
		// A secret carrying both is common (operators add a token beside an
		// existing username/password). The token wins deterministically;
		// failing here would wedge every RPC — including the delete/revoke
		// cleanup of already-provisioned resources — over a benign overlap.
		slog.Warn("mounted credentials contain both a token and username/password; using the token and ignoring username/password")
		username = ""
		password = ""
	}
	c := qumulo.Credentials{
		Token:    first(token, accessToken),
		Username: username,
		Password: password,
	}
	credentialMaterial := tokenPresent || accessTokenPresent || usernamePresent || passwordPresent
	if credentialMaterial && !c.HasToken() && !c.HasPassword() {
		return c, nil, fmt.Errorf("mounted credentials are incomplete")
	}
	caPath := filepath.Join(dir, "ca.crt")
	ca, err := os.ReadFile(caPath)
	if err != nil && !os.IsNotExist(err) {
		return c, nil, fmt.Errorf("read CA bundle %q: %w", caPath, err)
	}
	if err == nil && strings.TrimSpace(string(ca)) == "" {
		return c, nil, fmt.Errorf("read CA bundle %q: file is empty", caPath)
	}
	return c, ca, nil
}

func (c Class) DialConfig(creds qumulo.Credentials, ca []byte, log any) qumulo.DialConfig {
	return qumulo.DialConfig{
		Endpoint:    c.Endpoint,
		RESTPort:    c.RESTPort,
		Credentials: creds,
		TLS: qumulo.TLSConfig{
			InsecureSkipVerify: c.InsecureSkipTLSVerify,
			CABundlePEM:        ca,
		},
	}
}
