package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

type memSecrets map[string]map[string][]byte

func (m memSecrets) GetSecret(_ context.Context, ns, name string) (map[string][]byte, error) {
	return m[ns+"/"+name], nil
}

func TestParseClass_Defaults(t *testing.T) {
	d := Driver{
		DefaultEndpoint: "q.example",
		DefaultRESTPort: "8000",
		DefaultS3Port:   "9000",
		DefaultRegion:   "us-east-1",
		DefaultBasePath: "/k8s-buckets",
		DriverNamespace: "qumulo-cosi",
	}
	c, err := ParseClass(nil, d)
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "q.example" || !c.DeleteRootDir || c.PurgeOnDelete || c.AccessMode != "rw" {
		t.Fatalf("%+v", c)
	}
}

func TestParseClass_ObjectLockImpliesVersioning(t *testing.T) {
	c, err := ParseClass(map[string]string{"objectLockEnabled": "true"}, Driver{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.ObjectLockEnabled || c.Versioning != "Enabled" {
		t.Fatalf("%+v", c)
	}
}

// A malformed boolean must be an error, never a silent default:
// deleteRootDir defaults to true, so a typo would otherwise enable
// data deletion contrary to operator intent.
func TestParseClass_InvalidBoolRejected(t *testing.T) {
	for _, key := range []string{"deleteRootDir", "purgeOnDelete", "objectLockEnabled", "insecureSkipTLSVerify", "aclFallbackChmod"} {
		if _, err := ParseClass(map[string]string{key: "flase"}, Driver{}); err == nil {
			t.Fatalf("%s=flase must be rejected", key)
		}
	}
	c, err := ParseClass(map[string]string{"deleteRootDir": " false "}, Driver{})
	if err != nil {
		t.Fatal(err)
	}
	if c.DeleteRootDir {
		t.Fatal("whitespace-padded false must parse as false")
	}
}

func TestParseClass_ACLFallbackChmodAccepted(t *testing.T) {
	c, err := ParseClass(map[string]string{"aclFallbackChmod": "true"}, Driver{})
	if err != nil {
		t.Fatalf("aclFallbackChmod is a documented escape hatch and must parse: %v", err)
	}
	if !c.ACLFallbackChmod {
		t.Fatal("aclFallbackChmod=true not reflected in class")
	}
}

func TestParseClassForCleanup_DropsUnknownKeys(t *testing.T) {
	// Delete/revoke replay stored contexts; a key some other driver version
	// accepted must never make cleanup unreachable.
	c, err := ParseClassForCleanup(map[string]string{
		"someFutureParameter": "x",
		"purgeOnDelete":       "false",
	}, Driver{})
	if err != nil {
		t.Fatalf("cleanup parse must tolerate unknown keys: %v", err)
	}
	if c.PurgeOnDelete {
		t.Fatal("known keys must still parse in cleanup mode")
	}
	// The strict path still rejects the same input.
	if _, err := ParseClass(map[string]string{"someFutureParameter": "x"}, Driver{}); err == nil {
		t.Fatal("strict parse must keep rejecting unknown keys")
	}
}

func TestParseClass_RejectsUnsafeOrUnusableValues(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]string
	}{
		{name: "zero quota", params: map[string]string{"quotaLimit": "0"}},
		{name: "negative quota", params: map[string]string{"quotaLimit": "-1"}},
		{name: "relative base", params: map[string]string{"basePath": "relative/path"}},
		{name: "root base", params: map[string]string{"basePath": "/"}},
		{name: "unclean base", params: map[string]string{"basePath": "/safe/../other"}},
		{name: "control in base", params: map[string]string{"basePath": "/safe\nother"}},
		{name: "trailing control in base", params: map[string]string{"basePath": "/safe\n"}},
		{name: "zero REST port", params: map[string]string{"restPort": "0"}},
		{name: "large REST port", params: map[string]string{"restPort": "65536"}},
		{name: "nonnumeric S3 port", params: map[string]string{"s3Port": "nine-thousand"}},
		{name: "control in region", params: map[string]string{"region": "us-east-1\nsecret"}},
		{name: "trailing control in region", params: map[string]string{"region": "us-east-1\n"}},
		{name: "unknown access mode", params: map[string]string{"accessMode": "owner"}},
		{name: "unknown parameter typo", params: map[string]string{"deleteRootDr": "false"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseClass(tc.params, Driver{}); err == nil {
				t.Fatalf("ParseClass(%v) unexpectedly succeeded", tc.params)
			}
		})
	}
}

func TestParseClass_CanonicalizesPortsAndAccessMode(t *testing.T) {
	c, err := ParseClass(map[string]string{
		"restPort": "08000", "s3Port": "09000", "accessMode": "ReadOnly",
	}, Driver{})
	if err != nil {
		t.Fatal(err)
	}
	if c.RESTPort != "8000" || c.S3Port != "9000" || c.AccessMode != "ro" {
		t.Fatalf("class was not canonicalized: %+v", c)
	}
}

func TestResolveCredentials_SecretNamespaceGuard(t *testing.T) {
	d := Driver{DriverNamespace: "qumulo-cosi"}
	class := Class{CredentialsSecretName: "creds", CredentialsSecretNamespace: "kube-system"}
	_, _, err := ResolveCredentials(context.Background(), class, d, memSecrets{})
	if err == nil {
		t.Fatal("expected namespace refusal")
	}
}

func TestResolveCredentials_FromSecret(t *testing.T) {
	d := Driver{DriverNamespace: "qumulo-cosi"}
	class := Class{CredentialsSecretName: "creds", CredentialsSecretNamespace: "qumulo-cosi"}
	sec := memSecrets{"qumulo-cosi/creds": {"token": []byte("tok-1")}}
	c, _, err := ResolveCredentials(context.Background(), class, d, sec)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "tok-1" {
		t.Fatalf("%+v", c)
	}
}

func TestResolveCredentials_SecretUsesConfiguredCAFailClosed(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "cluster-ca.pem")
	wantCA := []byte("test-ca-bundle")
	if err := os.WriteFile(caPath, wantCA, 0o600); err != nil {
		t.Fatal(err)
	}
	class := Class{CredentialsSecretName: "creds", CredentialsSecretNamespace: "qumulo-cosi"}
	sec := memSecrets{"qumulo-cosi/creds": {"token": []byte("tok-1")}}

	_, gotCA, err := ResolveCredentials(context.Background(), class, Driver{DriverNamespace: "qumulo-cosi", CAFile: caPath}, sec)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotCA) != string(wantCA) {
		t.Fatalf("CA bundle = %q, want %q", gotCA, wantCA)
	}

	missing := filepath.Join(dir, "missing-ca.pem")
	_, _, err = ResolveCredentials(context.Background(), class, Driver{DriverNamespace: "qumulo-cosi", CAFile: missing}, sec)
	if err == nil || !strings.Contains(err.Error(), "read CA file") {
		t.Fatalf("missing explicitly configured CA must fail closed, got %v", err)
	}

	empty := filepath.Join(dir, "empty-ca.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = ResolveCredentials(context.Background(), class, Driver{DriverNamespace: "qumulo-cosi", CAFile: empty}, sec)
	if err == nil || !strings.Contains(err.Error(), "file is empty") {
		t.Fatalf("empty explicitly configured CA must fail closed, got %v", err)
	}
}

func TestResolveCredentials_SecretCAOverridesConfiguredCAFile(t *testing.T) {
	class := Class{CredentialsSecretName: "creds", CredentialsSecretNamespace: "qumulo-cosi"}
	sec := memSecrets{"qumulo-cosi/creds": {
		"token":  []byte("tok-1"),
		"ca.crt": []byte("secret-ca"),
	}}
	_, ca, err := ResolveCredentials(context.Background(), class, Driver{
		DriverNamespace: "qumulo-cosi",
		CAFile:          filepath.Join(t.TempDir(), "missing-ca.pem"),
	}, sec)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != "secret-ca" {
		t.Fatalf("CA bundle = %q, want secret CA", ca)
	}
}

func TestResolveCredentials_EmptySecretCAFailsClosed(t *testing.T) {
	class := Class{CredentialsSecretName: "creds", CredentialsSecretNamespace: "qumulo-cosi"}
	for _, key := range []string{"ca.crt", "ca.pem"} {
		t.Run(key, func(t *testing.T) {
			sec := memSecrets{"qumulo-cosi/creds": {
				"token": []byte("tok-1"), key: []byte(" \n\t"),
			}}
			_, _, err := ResolveCredentials(context.Background(), class, Driver{DriverNamespace: "qumulo-cosi"}, sec)
			if err == nil || !strings.Contains(err.Error(), "present but empty") {
				t.Fatalf("empty %s must fail closed, got %v", key, err)
			}
		})
	}
}

func TestResolveCredentials_RejectsAmbiguousOrPartialSecretAuth(t *testing.T) {
	class := Class{CredentialsSecretName: "creds", CredentialsSecretNamespace: "qumulo-cosi"}
	tests := []struct {
		name string
		data map[string][]byte
	}{
		{name: "combined", data: map[string][]byte{"token": []byte("tok"), "username": []byte("user"), "password": []byte("pass")}},
		{name: "token and partial password pair", data: map[string][]byte{"token": []byte("tok"), "username": []byte("user")}},
		{name: "partial password pair", data: map[string][]byte{"password": []byte("pass")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sec := memSecrets{"qumulo-cosi/creds": tc.data}
			if _, _, err := ResolveCredentials(context.Background(), class, Driver{DriverNamespace: "qumulo-cosi"}, sec); err == nil {
				t.Fatalf("ambiguous/partial Secret auth unexpectedly succeeded: %v", tc.data)
			}
		})
	}
}

func TestResolveCredentials_MountedCAReadErrorsFailClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok-dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory at the CA file path produces a portable ReadFile error on
	// both Unix and Windows; permission-bit tests are not reliable on Windows.
	if err := os.Mkdir(filepath.Join(dir, "ca.crt"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveCredentials(context.Background(), Class{}, Driver{CredentialsDir: dir}, nil)
	if err == nil || !strings.Contains(err.Error(), "ca.crt") {
		t.Fatalf("unreadable mounted CA must fail closed, got %v", err)
	}
}

func TestResolveCredentials_MountedCAIsOptionalWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok-dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, ca, err := ResolveCredentials(context.Background(), Class{}, Driver{CredentialsDir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "tok-dir" || len(ca) != 0 {
		t.Fatalf("credentials=%+v ca=%q", c, ca)
	}
}

func TestResolveCredentials_MountedCredentialSourceIsAtomic(t *testing.T) {
	t.Setenv("QUMULO_TOKEN", "more-privileged-env-token")
	t.Setenv("QUMULO_USERNAME", "")
	t.Setenv("QUMULO_PASSWORD", "")
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "empty token",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "token"), []byte(" \n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unreadable token",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(dir, "token"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "incomplete password pair",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "username"), []byte("mounted-user"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	// A secret carrying both a token and username/password is a benign
	// operator overlap: the token wins deterministically. Hard-failing here
	// wedged every RPC — including cleanup — on upgraded installs.
	t.Run("token wins over username and password", func(t *testing.T) {
		dir := t.TempDir()
		for name, value := range map[string]string{"token": "tok", "username": "user", "password": "pass"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		c, _, err := ResolveCredentials(context.Background(), Class{}, Driver{CredentialsDir: dir}, nil)
		if err != nil {
			t.Fatalf("token+password overlap must resolve to the token: %v", err)
		}
		if c.Token != "tok" || c.Username != "" || c.Password != "" {
			t.Fatalf("expected token-only credentials, got %+v", c)
		}
	})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			c, _, err := ResolveCredentials(context.Background(), Class{}, Driver{CredentialsDir: dir}, nil)
			if err == nil {
				t.Fatalf("invalid mounted source fell through to environment credentials: %+v", c)
			}
		})
	}
}

func TestResolveCredentials_RejectsAmbiguousOrPartialEnvironmentAuth(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		username string
		password string
	}{
		{name: "combined", token: "tok", username: "user", password: "pass"},
		{name: "token and partial password pair", token: "tok", username: "user"},
		{name: "partial password pair", password: "pass"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUMULO_TOKEN", tc.token)
			t.Setenv("QUMULO_USERNAME", tc.username)
			t.Setenv("QUMULO_PASSWORD", tc.password)
			if _, _, err := ResolveCredentials(context.Background(), Class{}, Driver{}, nil); err == nil {
				t.Fatal("ambiguous/partial environment auth unexpectedly succeeded")
			}
		})
	}
}

func TestResolveCredentials_MountedCAAppliesToEnvironmentCredentials(t *testing.T) {
	t.Setenv("QUMULO_TOKEN", "env-token")
	t.Setenv("QUMULO_USERNAME", "")
	t.Setenv("QUMULO_PASSWORD", "")
	dir := t.TempDir()
	wantCA := []byte("mounted-ca")
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), wantCA, 0o600); err != nil {
		t.Fatal(err)
	}
	c, ca, err := ResolveCredentials(context.Background(), Class{}, Driver{
		CredentialsDir: dir,
		CAFile:         filepath.Join(t.TempDir(), "missing-process-ca.pem"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "env-token" || string(ca) != string(wantCA) {
		t.Fatalf("credentials=%+v ca=%q, want environment token with mounted CA", c, ca)
	}
}

func TestCredsFromMap(t *testing.T) {
	c, _, err := credsFromMap(map[string][]byte{"username": []byte("u"), "password": []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasPassword() {
		t.Fatal(c)
	}
	var _ qumulo.Credentials = c
}
