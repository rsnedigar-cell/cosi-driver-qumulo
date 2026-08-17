package qumulo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout    = 30 * time.Second
	maxRetries        = 5
	retryBase         = 200 * time.Millisecond
	retryMax          = 5 * time.Second
	defaultRESTPort   = "8000"
	headerAuth        = "Authorization"
	headerContentType = "Content-Type"
	headerIfMatch     = "If-Match"
	headerETag        = "ETag"
)

// Credentials authenticates the driver to the Qumulo REST API.
type Credentials struct {
	Token    string
	Username string
	Password string
}

func (c Credentials) HasToken() bool { return strings.TrimSpace(c.Token) != "" }
func (c Credentials) HasPassword() bool {
	return strings.TrimSpace(c.Username) != "" && strings.TrimSpace(c.Password) != ""
}

// TLSConfig is per-connection TLS. Never mutates http.DefaultTransport.
type TLSConfig struct {
	InsecureSkipVerify bool
	CABundlePEM        []byte
	ServerName         string
}

// DialConfig identifies a cluster connection.
type DialConfig struct {
	Endpoint    string
	RESTPort    string
	Credentials Credentials
	TLS         TLSConfig
	UserAgent   string
	Timeout     time.Duration
	Logger      *slog.Logger
}

// Connection is a concurrent-safe Qumulo REST client.
type Connection struct {
	baseURL     *url.URL
	http        *http.Client
	creds       Credentials
	ua          string
	log         *slog.Logger
	timeout     time.Duration
	mu          sync.Mutex
	bearer      string
	loginFlight *loginCall
	version     string
	s3settings  *S3Settings
	// aclMu serializes filesystem-ACL read-modify-write cycles so
	// concurrent grants/revokes cannot drop each other's ACEs.
	aclMu sync.Mutex
}

type loginCall struct {
	done chan struct{}
	err  error
}

// NewConnection dials a cluster. TLS is verified by default.
func NewConnection(cfg DialConfig) (*Connection, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("qumulo endpoint is required")
	}
	if !cfg.Credentials.HasToken() && !cfg.Credentials.HasPassword() {
		return nil, fmt.Errorf("qumulo credentials: provide an access token or username/password")
	}
	port := cfg.RESTPort
	if port == "" {
		port = defaultRESTPort
	}
	raw := cfg.Endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("qumulo endpoint has no hostname")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("qumulo endpoint must not contain credentials, a path, query, or fragment")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		ip := net.ParseIP(u.Hostname())
		loopback := strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
		if !strings.EqualFold(u.Scheme, "http") || !loopback {
			return nil, fmt.Errorf("qumulo endpoint must use HTTPS (HTTP is allowed only for loopback tests)")
		}
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	effectivePort := u.Port()
	portNumber, err := strconv.ParseUint(effectivePort, 10, 16)
	if err != nil || portNumber == 0 {
		return nil, fmt.Errorf("invalid Qumulo REST port %q", effectivePort)
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.TLS.InsecureSkipVerify, //nolint:gosec // explicit escape hatch
		ServerName:         cfg.TLS.ServerName,
	}
	if len(cfg.TLS.CABundlePEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.TLS.CABundlePEM) {
			return nil, fmt.Errorf("failed to parse CA bundle PEM")
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.TLS.InsecureSkipVerify {
		lg := cfg.Logger
		if lg == nil {
			lg = slog.Default()
		}
		lg.Warn("qumulo TLS verification is DISABLED (insecureSkipTLSVerify=true); this is a security risk")
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsCfg,
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = "cosi-driver-qumulo/0.2.0"
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	c := &Connection{
		baseURL: u,
		http:    &http.Client{Transport: transport, Timeout: timeout},
		creds:   cfg.Credentials,
		ua:      ua,
		log:     log,
		timeout: timeout,
	}
	if cfg.Credentials.HasToken() {
		c.bearer = strings.TrimSpace(cfg.Credentials.Token)
	}
	return c, nil
}

func (c *Connection) EndpointHost() string { return c.baseURL.Hostname() }
func (c *Connection) RESTPort() string     { return c.baseURL.Port() }
func (c *Connection) BaseURL() string      { return c.baseURL.String() }

// DoJSON performs a JSON REST call with retry, context, and 401 re-login.
//
// Automatic retries are limited to operations whose request can be replayed
// without creating a second resource or acting on a name that may have been
// reused. In particular, POST and DELETE are never replayed after a transport
// error or a 5xx response: either can mean that Core committed the request but
// the client did not receive the response. The higher-level driver reconciles
// those ambiguous outcomes on the next COSI RPC, where it can re-check durable
// identity before acting again. A 401 is the sole exception because the
// rejected request was not authorized; one replay after refreshing the
// session is safe.
func (c *Connection) DoJSON(ctx context.Context, method, pth string, query url.Values, headers http.Header, in, out any) (http.Header, error) {
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
	}
	u := *c.baseURL
	// File refs are a single segment with slashes percent-encoded
	// (`/v1/files/%2Fk8s-buckets%2Fname/info/attributes`). Set Path +
	// RawPath so url.URL.String() does not turn %2F into %252F.
	if unesc, err := url.PathUnescape(pth); err == nil && unesc != pth {
		if !strings.HasPrefix(unesc, "/") {
			unesc = "/" + unesc
			pth = "/" + pth
		}
		u.Path = unesc
		u.RawPath = pth
	} else {
		u.Path = pth
		if !strings.HasPrefix(u.Path, "/") {
			u.Path = "/" + u.Path
		}
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}

	var lastErr error
	relogged := false
	retryable := replaySafeMethod(method)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.ua)
		if in != nil {
			req.Header.Set(headerContentType, "application/json")
		}
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if err := c.authorize(ctx, req); err != nil {
			return nil, err
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("qumulo request: %w", err)
			c.log.Debug("qumulo transport error", "method", method, "path", pth, "err", err, "attempt", attempt)
			if !retryable {
				return nil, lastErr
			}
			continue
		}
		respBody := readBody(resp)
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusUnauthorized && c.creds.HasPassword() && !relogged:
			if err := c.relogin(ctx); err != nil {
				return resp.Header, err
			}
			// A 401 is known not to have committed the request, so even a POST
			// may be replayed once after refreshing the session.
			relogged = true
			lastErr = parseAPIError(resp.StatusCode, respBody)
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = parseAPIError(resp.StatusCode, respBody)
			c.log.Debug("qumulo retryable status", "status", resp.StatusCode, "class", lastErr.(*APIError).ErrorClass, "attempt", attempt)
			if !retryable {
				return resp.Header, lastErr
			}
			continue
		case resp.StatusCode >= 400:
			return resp.Header, parseAPIError(resp.StatusCode, respBody)
		default:
			if out != nil && len(respBody) > 0 {
				if err := json.Unmarshal(respBody, out); err != nil {
					return resp.Header, fmt.Errorf("decode response from %s: %w", pth, err)
				}
			}
			return resp.Header, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("qumulo request failed after retries")
	}
	return nil, lastErr
}

// replaySafeMethod reflects the semantics of the Qumulo calls made by this
// client. PATCH calls only assign concrete values (mode, versioning, or lock
// configuration), so replaying them is idempotent even though generic HTTP
// PATCH is not necessarily so. DELETE is deliberately single-attempt: an
// ambiguous name-based delete must return to the caller so it can verify that
// the target name has not been reused before trying again.
func replaySafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

type pageMetadata struct {
	Next *string `json:"next"`
}

// nextPageAfter extracts the opaque continuation token from the URI returned
// in paging.next. Qumulo's API deliberately exposes the next page as a URI so
// clients do not depend on the token format. We consume only its `after`
// parameter and continue against the original endpoint; a server-supplied
// host or path is never followed.
func nextPageAfter(next *string, seen map[string]struct{}) (after string, hasNext bool, err error) {
	if next == nil {
		return "", false, nil
	}
	nextURI := strings.TrimSpace(*next)
	u, err := url.Parse(nextURI)
	if err != nil {
		return "", false, fmt.Errorf("parse paging.next URI %q: %w", nextURI, err)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", false, fmt.Errorf("parse paging.next query %q: %w", nextURI, err)
	}
	after = query.Get("after")
	if after == "" {
		return "", false, fmt.Errorf("paging.next URI %q has no after cursor", nextURI)
	}
	if _, duplicate := seen[after]; duplicate {
		return "", false, fmt.Errorf("pagination made no progress: repeated after cursor %q", after)
	}
	seen[after] = struct{}{}
	return after, true, nil
}

func (c *Connection) authorize(ctx context.Context, req *http.Request) error {
	c.mu.Lock()
	tok := c.bearer
	c.mu.Unlock()
	if tok == "" {
		if err := c.relogin(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		tok = c.bearer
		c.mu.Unlock()
	}
	if tok != "" {
		req.Header.Set(headerAuth, "Bearer "+tok)
	}
	return nil
}

func (c *Connection) relogin(ctx context.Context) error {
	if !c.creds.HasPassword() {
		return &APIError{StatusCode: http.StatusUnauthorized, ErrorClass: ErrClassAuthInvalidCreds, Description: "access token rejected and no username/password configured for re-login"}
	}
	c.mu.Lock()
	if c.loginFlight != nil {
		wait := c.loginFlight
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait.done:
			return wait.err
		}
	}
	flight := &loginCall{done: make(chan struct{})}
	c.loginFlight = flight
	c.mu.Unlock()

	err := c.doLogin(ctx)
	c.mu.Lock()
	flight.err = err
	close(flight.done)
	c.loginFlight = nil
	c.mu.Unlock()
	return err
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	BearerToken string `json:"bearer_token"`
}

func (c *Connection) doLogin(ctx context.Context) error {
	var out loginResponse
	// Login itself must not recurse into authorize.
	u := *c.baseURL
	u.Path = "/v1/session/login"
	body, err := json.Marshal(loginRequest{Username: c.creds.Username, Password: c.creds.Password})
	if err != nil {
		return fmt.Errorf("marshal login: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set(headerContentType, "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	b := readBody(resp)
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return parseAPIError(resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("decode login: %w", err)
	}
	if out.BearerToken == "" {
		return fmt.Errorf("login returned empty bearer_token")
	}
	c.mu.Lock()
	c.bearer = out.BearerToken
	c.mu.Unlock()
	c.log.Info("qumulo session login succeeded", "endpoint", c.baseURL.Host, "user", c.creds.Username)
	return nil
}

func sleepBackoff(ctx context.Context, attempt int) error {
	d := retryBase * time.Duration(1<<uint(attempt-1))
	if d > retryMax {
		d = retryMax
	}
	// jitter ±30%
	j := time.Duration(rand.Int63n(int64(d/3)+1)) - d/6
	d += j
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Cache is an LRU of Connections keyed by endpoint+credential fingerprint.
type Cache struct {
	mu      sync.Mutex
	max     int
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	conn *Connection
	seen time.Time
}

func NewCache(max int) *Cache {
	if max <= 0 {
		max = 16
	}
	return &Cache{max: max, entries: make(map[string]*cacheEntry)}
}

// CacheKey fingerprints everything that affects how a connection dials and
// authenticates. Rotating a password or CA bundle, or flipping TLS
// verification, must change the key so the driver stops using a stale
// cached connection.
func CacheKey(endpoint, restPort string, creds Credentials, caBundle []byte, insecureSkipVerify bool) string {
	insecure := "verify"
	if insecureSkipVerify {
		insecure = "insecure"
	}
	caSum := sha256.Sum256(caBundle)
	h := sha256.Sum256([]byte(strings.Join([]string{
		endpoint,
		restPort,
		creds.Token,
		creds.Username,
		creds.Password,
		hex.EncodeToString(caSum[:]),
		insecure,
	}, "\x00")))
	return hex.EncodeToString(h[:])
}

func (c *Cache) Get(key string, dial func() (*Connection, error)) (*Connection, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		e.seen = time.Now()
		conn := e.conn
		c.mu.Unlock()
		return conn, nil
	}
	c.mu.Unlock()
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		return e.conn, nil
	}
	if len(c.entries) >= c.max {
		var oldest string
		var t time.Time
		for k, e := range c.entries {
			if oldest == "" || e.seen.Before(t) {
				oldest, t = k, e.seen
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = &cacheEntry{conn: conn, seen: time.Now()}
	return conn, nil
}

// LoadCABundle reads a PEM file if path is non-empty.
func LoadCABundle(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	return os.ReadFile(path)
}

// Discard unused import guard: io is used via readBody in errors.go (same package).
var _ = io.EOF
