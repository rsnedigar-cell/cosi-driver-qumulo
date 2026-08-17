// Package csidriver implements dynamic Qumulo NFS and SMB volumes through
// the Kubernetes Container Storage Interface. It intentionally lives beside,
// rather than inside, the COSI driver because COSI only models object stores.
package csidriver

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

const (
	DefaultDriverName     = "file.qumulo.com"
	DefaultControllerAddr = "unix:///csi/csi.sock"
	DefaultNodeAddr       = "unix:///csi/csi.sock"
	DefaultRESTPort       = "8000"
	DefaultBasePath       = "/k8s-volumes"
	DefaultVersionFloor   = "7.2.0"
)

// Config is process-level CSI configuration. A deployment is deliberately
// bound to one Qumulo endpoint; accepting endpoints from arbitrary PV handles
// would turn controller and node pods into SSRF/mount proxies.
type Config struct {
	Name                   string
	Version                string
	Address                string
	Mode                   string
	NodeID                 string
	Endpoint               string
	DataServer             string
	RESTPort               string
	BasePath               string
	VersionFloor           string
	CredentialsDir         string
	CAFile                 string
	InsecureSkipTLSVerify  bool
	DefaultAllowedNetworks []string
	StateDir               string
	KubeletRoot            string
	HandleKeyFile          string
	HandleKey              []byte
}

func ConfigFromEnv(version string) Config {
	endpoint := strings.TrimSpace(os.Getenv("QUMULO_ENDPOINT"))
	dataServer := strings.TrimSpace(os.Getenv("QUMULO_DATA_SERVER"))
	if dataServer == "" {
		dataServer = endpointHost(endpoint)
	}
	return Config{
		Name:                   firstEnv("QUMULO_CSI_DRIVER_NAME", DefaultDriverName),
		Version:                version,
		Address:                firstEnv("CSI_ENDPOINT", DefaultControllerAddr),
		Mode:                   firstEnv("QUMULO_CSI_MODE", "controller"),
		NodeID:                 strings.TrimSpace(os.Getenv("NODE_ID")),
		Endpoint:               endpoint,
		DataServer:             dataServer,
		RESTPort:               firstEnv("QUMULO_REST_PORT", DefaultRESTPort),
		BasePath:               firstEnv("QUMULO_CSI_BASE_PATH", DefaultBasePath),
		VersionFloor:           firstEnv("QUMULO_VERSION_FLOOR", DefaultVersionFloor),
		CredentialsDir:         firstEnv("QUMULO_CREDENTIALS_DIR", "/etc/qumulo/credentials"),
		CAFile:                 strings.TrimSpace(os.Getenv("QUMULO_CA_FILE")),
		InsecureSkipTLSVerify:  envBool("QUMULO_INSECURE_SKIP_TLS_VERIFY", false),
		DefaultAllowedNetworks: splitList(os.Getenv("QUMULO_CSI_ALLOWED_NETWORKS")),
		StateDir:               firstEnv("QUMULO_CSI_STATE_DIR", "/var/lib/qumulo-csi"),
		KubeletRoot:            firstEnv("KUBELET_ROOT_DIR", "/var/lib/kubelet"),
		HandleKeyFile:          firstEnv("QUMULO_CSI_HANDLE_KEY_FILE", "/etc/qumulo/handle-key/key"),
	}
}

// LoadHandleKey loads the shared controller/node key used to authenticate
// opaque CSI volume handles. Keeping it in a dedicated Secret lets node pods
// verify handles without receiving Qumulo administrative credentials.
func (c *Config) LoadHandleKey() error {
	if len(c.HandleKey) >= 32 {
		return nil
	}
	if c.HandleKeyFile == "" {
		return fmt.Errorf("CSI handle signing key file is required")
	}
	raw, err := os.ReadFile(c.HandleKeyFile)
	if err != nil {
		return fmt.Errorf("read CSI handle signing key: %w", err)
	}
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) < 32 {
		return fmt.Errorf("CSI handle signing key must contain at least 32 bytes")
	}
	c.HandleKey = append([]byte(nil), raw...)
	return nil
}

// Validate normalizes and validates the process-level trust boundary before
// the gRPC socket becomes available.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("CSI driver name is required")
	}
	if c.Mode != "controller" && c.Mode != "node" && c.Mode != "all" {
		return fmt.Errorf("invalid CSI service mode %q", c.Mode)
	}
	if len(c.HandleKey) < 32 {
		return fmt.Errorf("CSI handle signing key is unavailable")
	}
	if _, _, err := parseCSIAddress(c.Address); err != nil {
		return err
	}
	port, err := configuredRESTPort(c.Endpoint, c.RESTPort)
	if err != nil {
		return err
	}
	c.RESTPort = port
	if strings.TrimSpace(c.DataServer) == "" {
		c.DataServer = endpointHost(c.Endpoint)
	}
	if err := validateDataServer(c.DataServer); err != nil {
		return err
	}
	c.BasePath = path.Clean(strings.TrimSpace(c.BasePath))
	if !strings.HasPrefix(c.BasePath, "/") || c.BasePath == "/" {
		return fmt.Errorf("CSI base path must be an absolute non-root path")
	}
	if err := validateNetworkRules(c.DefaultAllowedNetworks); err != nil {
		return err
	}
	if (c.Mode == "node" || c.Mode == "all") && strings.TrimSpace(c.NodeID) == "" {
		return fmt.Errorf("node ID is required in node mode")
	}
	if c.Mode == "node" || c.Mode == "all" {
		kubeletRoot, stateDir := path.Clean(c.KubeletRoot), path.Clean(c.StateDir)
		if !strings.HasPrefix(kubeletRoot, "/") || kubeletRoot == "/" || !strings.HasPrefix(stateDir, "/") || stateDir == "/" {
			return fmt.Errorf("kubelet root and CSI state directory must be absolute, non-root Linux paths")
		}
	}
	return nil
}

type protocol string

const (
	protocolNFS protocol = "nfs"
	protocolSMB protocol = "smb"
)

type volumeOptions struct {
	RequestName          string
	Parameters           map[string]string
	Protocol             protocol
	Endpoint             string
	Server               string
	RESTPort             string
	FSPath               string
	ResourceName         string
	DirectoryMode        string
	AllowedNetworks      []string
	AllowAllHosts        bool
	QuotaEnabled         bool
	DeleteData           bool
	NFSExportPath        string
	NFSRequirePrivileged bool
	NFSVersion           string
	NFSRootSquash        bool
	NFSAnonymousUID      string
	NFSAnonymousGID      string
	SMBRequireEncryption bool
	SMBAccessBasedEnum   bool
	SMBAllowAllUsers     bool
	SMBTrustee           qumulo.Identity
	RequestedCapacity    int64
	CapacityLimit        int64
}

var modePattern = regexp.MustCompile(`^0[0-7]{3}$`)

var volumeParameterKeys = map[string]struct{}{
	"protocol": {}, "endpoint": {}, "basePath": {},
	"allowedNetworks": {}, "allowAllHosts": {}, "directoryMode": {},
	"quotaEnabled": {}, "deleteData": {}, "tenantID": {},
	"nfsVersion": {}, "nfsExportPrefix": {}, "nfsRequirePrivilegedPort": {},
	"nfsRootSquash": {}, "nfsAnonymousUID": {}, "nfsAnonymousGID": {},
	"smbRequireEncryption": {}, "smbAccessBasedEnumeration": {},
	"smbTrusteeAuthID": {}, "smbTrusteeDomain": {}, "smbTrusteeName": {},
	"allowAllSMBUsers": {},
}

// kubernetesReservedParameterPrefix marks parameters injected by Kubernetes
// CSI sidecars rather than written by the operator: secret references, and —
// when the external-provisioner runs with --extra-create-metadata — the
// pvc/name, pvc/namespace, and pv/name keys. They are never Qumulo volume
// options, so they are accepted and ignored instead of failing CreateVolume.
const kubernetesReservedParameterPrefix = "csi.storage.k8s.io/"

func parseVolumeOptions(cfg Config, name string, params map[string]string) (volumeOptions, error) {
	for key := range params {
		if strings.HasPrefix(key, kubernetesReservedParameterPrefix) {
			continue
		}
		if _, ok := volumeParameterKeys[key]; !ok {
			return volumeOptions{}, fmt.Errorf("unknown CSI volume parameter %q", key)
		}
	}
	proto := protocol(strings.ToLower(strings.TrimSpace(params["protocol"])))
	if proto != protocolNFS && proto != protocolSMB {
		return volumeOptions{}, fmt.Errorf("parameter protocol must be nfs or smb")
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return volumeOptions{}, fmt.Errorf("Qumulo endpoint is required")
	}
	restPort, err := configuredRESTPort(cfg.Endpoint, cfg.RESTPort)
	if err != nil {
		return volumeOptions{}, err
	}
	if supplied := strings.TrimSpace(params["endpoint"]); supplied != "" {
		suppliedPort, suppliedErr := configuredRESTPort(supplied, restPort)
		if suppliedErr != nil {
			return volumeOptions{}, fmt.Errorf("parameter endpoint %q is invalid: %w", supplied, suppliedErr)
		}
		if !sameEndpoint(supplied, cfg.Endpoint) || suppliedPort != restPort {
			return volumeOptions{}, fmt.Errorf("parameter endpoint %q does not match this driver's configured endpoint and REST port", supplied)
		}
	}

	resourceName := volumeResourceName(name)
	baseInput := cfg.BasePath
	if raw, ok := params["basePath"]; ok && raw != "" {
		baseInput = raw
	}
	base, err := validatedVolumePath("basePath", baseInput, false)
	if err != nil {
		return volumeOptions{}, err
	}

	mode := strings.TrimSpace(params["directoryMode"])
	if mode == "" {
		mode = "0777"
	}
	if !modePattern.MatchString(mode) {
		return volumeOptions{}, fmt.Errorf("directoryMode %q must be a four-digit octal mode", mode)
	}

	allowAll, err := strictBool(params, "allowAllHosts", false)
	if err != nil {
		return volumeOptions{}, err
	}
	allowed := splitList(params["allowedNetworks"])
	if allowAll && len(allowed) > 0 {
		return volumeOptions{}, fmt.Errorf("allowAllHosts cannot be combined with allowedNetworks")
	}
	if len(allowed) == 0 && !allowAll {
		allowed = append([]string(nil), cfg.DefaultAllowedNetworks...)
	}
	if len(allowed) == 0 && !allowAll {
		return volumeOptions{}, fmt.Errorf("allowedNetworks is required; set allowAllHosts=true only for an intentionally public storage network")
	}
	if err := validateNetworkRules(allowed); err != nil {
		return volumeOptions{}, err
	}

	quota, err := strictBool(params, "quotaEnabled", true)
	if err != nil {
		return volumeOptions{}, err
	}
	deleteData, err := strictBool(params, "deleteData", true)
	if err != nil {
		return volumeOptions{}, err
	}
	requirePrivileged, err := strictBool(params, "nfsRequirePrivilegedPort", false)
	if err != nil {
		return volumeOptions{}, err
	}
	requireEncryption, err := strictBool(params, "smbRequireEncryption", true)
	if err != nil {
		return volumeOptions{}, err
	}
	accessBasedEnum, err := strictBool(params, "smbAccessBasedEnumeration", true)
	if err != nil {
		return volumeOptions{}, err
	}
	rootSquash, err := strictBool(params, "nfsRootSquash", true)
	if err != nil {
		return volumeOptions{}, err
	}
	allowAllSMBUsers, err := strictBool(params, "allowAllSMBUsers", false)
	if err != nil {
		return volumeOptions{}, err
	}

	if raw := strings.TrimSpace(params["tenantID"]); raw != "" {
		return volumeOptions{}, fmt.Errorf("tenantID requires Qumulo's preview v3 share APIs and is not supported by this stable v2 driver")
	}

	nfsPrefixInput := "/k8s"
	if raw, ok := params["nfsExportPrefix"]; ok && raw != "" {
		nfsPrefixInput = raw
	}
	nfsPrefix, err := validatedVolumePath("nfsExportPrefix", nfsPrefixInput, true)
	if err != nil {
		return volumeOptions{}, err
	}
	nfsVersion := strings.TrimSpace(params["nfsVersion"])
	if nfsVersion == "" {
		nfsVersion = "4.1"
	}
	if nfsVersion != "3" && nfsVersion != "4.1" {
		return volumeOptions{}, fmt.Errorf("nfsVersion %q is unsupported; use 3 or 4.1", nfsVersion)
	}
	anonymousUID := firstParameter(params, "nfsAnonymousUID", "65534")
	anonymousGID := firstParameter(params, "nfsAnonymousGID", "65534")
	if proto == protocolNFS && rootSquash {
		if _, err := strconv.ParseUint(anonymousUID, 10, 32); err != nil {
			return volumeOptions{}, fmt.Errorf("nfsAnonymousUID %q must be an unsigned 32-bit integer", anonymousUID)
		}
		if _, err := strconv.ParseUint(anonymousGID, 10, 32); err != nil {
			return volumeOptions{}, fmt.Errorf("nfsAnonymousGID %q must be an unsigned 32-bit integer", anonymousGID)
		}
	}

	var smbTrustee qumulo.Identity
	if proto == protocolSMB {
		authID := strings.TrimSpace(params["smbTrusteeAuthID"])
		domain := strings.TrimSpace(params["smbTrusteeDomain"])
		trusteeName := strings.TrimSpace(params["smbTrusteeName"])
		if strings.ContainsAny(authID+domain+trusteeName, "\x00\r\n") {
			return volumeOptions{}, fmt.Errorf("SMB trustee contains a control character")
		}
		if allowAllSMBUsers {
			if authID != "" || domain != "" || trusteeName != "" {
				return volumeOptions{}, fmt.Errorf("allowAllSMBUsers cannot be combined with an SMB trustee")
			}
			smbTrustee = qumulo.Identity{Domain: "WORLD", Name: "Everyone"}
		} else if authID != "" {
			if domain != "" || trusteeName != "" {
				return volumeOptions{}, fmt.Errorf("smbTrusteeAuthID cannot be combined with smbTrusteeDomain or smbTrusteeName")
			}
			smbTrustee = qumulo.Identity{AuthID: authID}
		} else {
			if domain == "" || trusteeName == "" {
				return volumeOptions{}, fmt.Errorf("SMB volumes require smbTrusteeAuthID or both smbTrusteeDomain and smbTrusteeName; use allowAllSMBUsers=true only for an intentionally shared identity boundary")
			}
			smbTrustee = qumulo.Identity{Domain: domain, Name: trusteeName}
		}
	}

	server := strings.TrimSpace(cfg.DataServer)
	if server == "" {
		server = endpointHost(cfg.Endpoint)
	}
	if server == "" {
		return volumeOptions{}, fmt.Errorf("Qumulo data server is required")
	}
	if err := validateDataServer(server); err != nil {
		return volumeOptions{}, err
	}

	return volumeOptions{
		RequestName:          name,
		Parameters:           cloneStringMap(params),
		Protocol:             proto,
		Endpoint:             cfg.Endpoint,
		Server:               server,
		RESTPort:             restPort,
		FSPath:               path.Join(base, string(proto), resourceName),
		ResourceName:         resourceName,
		DirectoryMode:        mode,
		AllowedNetworks:      allowed,
		AllowAllHosts:        allowAll,
		QuotaEnabled:         quota,
		DeleteData:           deleteData,
		NFSExportPath:        path.Join(nfsPrefix, resourceName),
		NFSRequirePrivileged: requirePrivileged,
		NFSVersion:           nfsVersion,
		NFSRootSquash:        rootSquash,
		NFSAnonymousUID:      anonymousUID,
		NFSAnonymousGID:      anonymousGID,
		SMBRequireEncryption: requireEncryption,
		SMBAccessBasedEnum:   accessBasedEnum,
		SMBAllowAllUsers:     allowAllSMBUsers,
		SMBTrustee:           smbTrustee,
	}, nil
}

func validatedVolumePath(name, value string, allowRoot bool) (string, error) {
	if value == "" || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s %q must be a clean absolute POSIX path", name, value)
	}
	clean := path.Clean(value)
	if !strings.HasPrefix(value, "/") || clean != value || !allowRoot && clean == "/" {
		kind := "absolute"
		if !allowRoot {
			kind = "absolute non-root"
		}
		return "", fmt.Errorf("%s %q must be a clean %s POSIX path", name, value, kind)
	}
	return clean, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func strictBool(params map[string]string, key string, def bool) (bool, error) {
	raw, ok := params[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("parameter %s=%q is not a valid boolean", key, raw)
	}
	return b, nil
}

func validateNetworkRules(rules []string) error {
	for _, rule := range rules {
		if rule == "" || len(rule) > 255 || strings.ContainsAny(rule, "\x00\r\n\t ,") {
			return fmt.Errorf("invalid allowedNetworks entry %q", rule)
		}
	}
	return nil
}

func sameEndpoint(a, b string) bool {
	return strings.EqualFold(endpointHost(a), endpointHost(b))
}

func endpointHost(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimRight(raw, "/")
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(raw, "[]")
}

func configuredRESTPort(endpoint, fallback string) (string, error) {
	raw := strings.TrimSpace(endpoint)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid Qumulo endpoint %q", endpoint)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("Qumulo endpoint must use HTTPS")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("Qumulo endpoint must not contain credentials, a path, query, or fragment")
	}
	port := u.Port()
	if port == "" {
		port = strings.TrimSpace(fallback)
	}
	if port == "" {
		port = DefaultRESTPort
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return "", fmt.Errorf("Qumulo REST port %q is invalid", port)
	}
	return port, nil
}

func validateDataServer(server string) error {
	raw := strings.TrimSpace(server)
	if raw == "" || strings.ContainsAny(raw, "/\\\x00\r\n\t ") || strings.Contains(raw, "://") || strings.HasPrefix(raw, "-") {
		return fmt.Errorf("Qumulo data server %q is invalid", server)
	}
	host := strings.Trim(raw, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	if strings.Contains(host, ":") || len(host) > 253 {
		return fmt.Errorf("Qumulo data server %q must be a hostname or IP address without a port", server)
	}
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("Qumulo data server %q is invalid", server)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("Qumulo data server %q is invalid", server)
			}
		}
	}
	return nil
}

func splitList(raw string) []string {
	var out []string
	for _, item := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func firstEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func firstParameter(params map[string]string, key, fallback string) string {
	if value := strings.TrimSpace(params[key]); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return b
}
