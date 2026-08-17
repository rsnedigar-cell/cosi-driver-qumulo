package csidriver

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

type backendConnector interface {
	Connect(context.Context, string, string, map[string]string) (storageBackend, error)
}

type qumuloConnector struct {
	cfg   Config
	cache *qumulo.Cache
	log   *slog.Logger
}

func newQumuloConnector(cfg Config, log *slog.Logger) *qumuloConnector {
	if log == nil {
		log = slog.Default()
	}
	return &qumuloConnector{cfg: cfg, cache: qumulo.NewCache(16), log: log}
}

func (c *qumuloConnector) Connect(_ context.Context, endpoint, restPort string, secrets map[string]string) (storageBackend, error) {
	if !sameEndpoint(endpoint, c.cfg.Endpoint) {
		return nil, fmt.Errorf("volume endpoint does not match this driver instance")
	}
	creds, ca, err := c.controllerAuth(secrets)
	if err != nil {
		return nil, err
	}

	key := qumulo.CacheKey(endpoint, restPort, creds, ca, c.cfg.InsecureSkipTLSVerify)
	conn, err := c.cache.Get(key, func() (*qumulo.Connection, error) {
		return qumulo.NewConnection(qumulo.DialConfig{
			Endpoint:    endpoint,
			RESTPort:    restPort,
			Credentials: creds,
			TLS: qumulo.TLSConfig{
				InsecureSkipVerify: c.cfg.InsecureSkipTLSVerify,
				CABundlePEM:        ca,
			},
			UserAgent: "qumulo-csi-driver/" + c.cfg.Version,
			Logger:    c.log,
		})
	})
	if err != nil {
		return nil, err
	}
	return &qumuloBackend{conn: conn}, nil
}

// controllerAuth chooses request-scoped or configured authentication as one
// unit. In particular, a request CA can never be combined with the mounted
// administrative credential: doing so would let an incomplete per-RPC Secret
// replace the trust root used to transmit a more privileged fallback token.
func (c *qumuloConnector) controllerAuth(secrets map[string]string) (qumulo.Credentials, []byte, error) {
	tokenPresent := hasAnyKey(secrets, "qumuloToken", "token", "accessToken")
	usernamePresent := hasAnyKey(secrets, "qumuloUsername")
	passwordPresent := hasAnyKey(secrets, "qumuloPassword")
	caPresent := hasAnyKey(secrets, "ca.crt", "ca.pem")
	requestCreds := qumulo.Credentials{
		Token:    firstValue(secrets, "qumuloToken", "token", "accessToken"),
		Username: firstValue(secrets, "qumuloUsername"),
		Password: firstValue(secrets, "qumuloPassword"),
	}
	requestCA := []byte(firstValue(secrets, "ca.crt", "ca.pem"))
	requestMaterial := tokenPresent || usernamePresent || passwordPresent || caPresent
	if err := validatePresentValues(secrets,
		"qumuloToken", "token", "accessToken", "qumuloUsername", "qumuloPassword", "ca.crt", "ca.pem"); err != nil {
		return qumulo.Credentials{}, nil, fmt.Errorf("request Secret: %w", err)
	}
	if usernamePresent != passwordPresent {
		return qumulo.Credentials{}, nil, fmt.Errorf("request Secret username and password must either both be present or both be absent")
	}
	if tokenPresent && (usernamePresent || passwordPresent) {
		return qumulo.Credentials{}, nil, fmt.Errorf("request Secret cannot combine a Qumulo token with username/password")
	}
	if requestMaterial && !requestCreds.HasToken() && !requestCreds.HasPassword() {
		return qumulo.Credentials{}, nil, fmt.Errorf("request Secret must contain a token or both Qumulo username and password; a CA alone cannot select configured credentials")
	}

	if requestCreds.HasToken() || requestCreds.HasPassword() {
		if caPresent {
			return requestCreds, requestCA, nil
		}
		configuredCA, err := c.configuredCA()
		if err != nil {
			return qumulo.Credentials{}, nil, err
		}
		return requestCreds, configuredCA, nil
	}

	configuredCA, err := c.configuredCA()
	if err != nil {
		return qumulo.Credentials{}, nil, err
	}
	creds, mountedMaterial, err := credentialsFromDir(c.cfg.CredentialsDir)
	if err != nil {
		return qumulo.Credentials{}, nil, err
	}
	if mountedMaterial {
		return creds, configuredCA, nil
	}
	creds = qumulo.Credentials{
		Token:    strings.TrimSpace(os.Getenv("QUMULO_TOKEN")),
		Username: strings.TrimSpace(os.Getenv("QUMULO_USERNAME")),
		Password: strings.TrimSpace(os.Getenv("QUMULO_PASSWORD")),
	}
	hasToken := creds.HasToken()
	hasUsername := creds.Username != ""
	hasPassword := creds.Password != ""
	if hasUsername != hasPassword {
		return qumulo.Credentials{}, nil, fmt.Errorf("QUMULO_USERNAME and QUMULO_PASSWORD must either both be set or both be unset")
	}
	if hasToken && (hasUsername || hasPassword) {
		return qumulo.Credentials{}, nil, fmt.Errorf("environment credentials cannot combine QUMULO_TOKEN with QUMULO_USERNAME/QUMULO_PASSWORD")
	}
	if !creds.HasToken() && !creds.HasPassword() {
		return qumulo.Credentials{}, nil, fmt.Errorf("Qumulo controller credentials are missing")
	}
	return creds, configuredCA, nil
}

func (c *qumuloConnector) configuredCA() ([]byte, error) {
	if c.cfg.CAFile != "" {
		ca, err := os.ReadFile(c.cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read Qumulo CA file: %w", err)
		}
		if strings.TrimSpace(string(ca)) == "" {
			return nil, fmt.Errorf("read Qumulo CA file: file is empty")
		}
		return ca, nil
	}
	if c.cfg.CredentialsDir != "" {
		ca, err := os.ReadFile(filepath.Join(c.cfg.CredentialsDir, "ca.crt"))
		if err == nil {
			if strings.TrimSpace(string(ca)) == "" {
				return nil, fmt.Errorf("read Qumulo CA from credentials directory: file is empty")
			}
			return ca, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read Qumulo CA from credentials directory: %w", err)
		}
	}
	return nil, nil
}

func credentialsFromDir(dir string) (qumulo.Credentials, bool, error) {
	if dir == "" {
		return qumulo.Credentials{}, false, nil
	}
	read := func(name string) (string, bool, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			return "", false, nil
		}
		if err != nil {
			return "", true, fmt.Errorf("read mounted credential %q: %w", filepath.Join(dir, name), err)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", true, fmt.Errorf("mounted credential %q is present but empty", filepath.Join(dir, name))
		}
		return value, true, nil
	}
	token, tokenPresent, err := read("token")
	if err != nil {
		return qumulo.Credentials{}, true, err
	}
	accessToken, accessTokenPresent, err := read("accessToken")
	if err != nil {
		return qumulo.Credentials{}, true, err
	}
	username, usernamePresent, err := read("username")
	if err != nil {
		return qumulo.Credentials{}, true, err
	}
	password, passwordPresent, err := read("password")
	if err != nil {
		return qumulo.Credentials{}, true, err
	}
	material := tokenPresent || accessTokenPresent || usernamePresent || passwordPresent
	if usernamePresent != passwordPresent {
		return qumulo.Credentials{}, true, fmt.Errorf("mounted username and password must either both be present or both be absent")
	}
	if (tokenPresent || accessTokenPresent) && (usernamePresent || passwordPresent) {
		return qumulo.Credentials{}, true, fmt.Errorf("mounted credentials cannot combine a token with username/password")
	}
	return qumulo.Credentials{
		Token:    firstNonEmpty(token, accessToken),
		Username: username,
		Password: password,
	}, material, nil
}

func firstValue(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func hasAnyKey(values map[string]string, keys ...string) bool {
	for _, key := range keys {
		if _, ok := values[key]; ok {
			return true
		}
	}
	return false
}

func validatePresentValues(values map[string]string, keys ...string) error {
	for _, key := range keys {
		if value, ok := values[key]; ok && strings.TrimSpace(value) == "" {
			return fmt.Errorf("key %q is present but empty", key)
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
