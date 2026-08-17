package csidriver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
)

const volumeHandlePrefix = "qv1:"

type volumeHandle struct {
	Protocol        protocol `json:"protocol"`
	Endpoint        string   `json:"endpoint"`
	RESTPort        string   `json:"restPort"`
	Server          string   `json:"server"`
	FSPath          string   `json:"fsPath"`
	DirectoryID     string   `json:"directoryID"`
	ResourceID      string   `json:"resourceID"`
	ResourceName    string   `json:"resourceName"`
	SpecFingerprint string   `json:"specFingerprint"`
	Capacity        int64    `json:"capacityBytes,omitempty"`
	QuotaEnabled    bool     `json:"quotaEnabled"`
	DeleteData      bool     `json:"deleteData"`
	NFSVersion      string   `json:"nfsVersion,omitempty"`
	SMBEncrypted    bool     `json:"smbEncrypted,omitempty"`
}

func (h volumeHandle) encode(key []byte) (string, error) {
	if len(key) < 32 {
		return "", fmt.Errorf("CSI handle signing key must contain at least 32 bytes")
	}
	if err := h.validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("encode volume handle: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	signature := signVolumeHandle(payload, key)
	return volumeHandlePrefix + payload + "." + signature, nil
}

func decodeVolumeHandle(raw string, cfg Config) (volumeHandle, error) {
	if len(cfg.HandleKey) < 32 {
		return volumeHandle{}, fmt.Errorf("CSI handle signing key is unavailable")
	}
	if !strings.HasPrefix(raw, volumeHandlePrefix) {
		return volumeHandle{}, fmt.Errorf("invalid volume ID format")
	}
	encoded := strings.TrimPrefix(raw, volumeHandlePrefix)
	payloadPart, signature, ok := strings.Cut(encoded, ".")
	if !ok || signature == "" || !hmac.Equal([]byte(signature), []byte(signVolumeHandle(payloadPart, cfg.HandleKey))) {
		return volumeHandle{}, fmt.Errorf("volume ID signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return volumeHandle{}, fmt.Errorf("decode volume ID: %w", err)
	}
	var h volumeHandle
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return volumeHandle{}, fmt.Errorf("decode volume ID: %w", err)
	}
	if err := h.validate(); err != nil {
		return volumeHandle{}, err
	}
	if cfg.Endpoint != "" && !sameEndpoint(h.Endpoint, cfg.Endpoint) {
		return volumeHandle{}, fmt.Errorf("volume endpoint does not match this driver instance")
	}
	if cfg.DataServer != "" && !strings.EqualFold(endpointHost(h.Server), endpointHost(cfg.DataServer)) {
		return volumeHandle{}, fmt.Errorf("volume data server does not match this driver instance")
	}
	if cfg.Endpoint != "" {
		configuredPort, err := configuredRESTPort(cfg.Endpoint, cfg.RESTPort)
		if err != nil || h.RESTPort != configuredPort {
			return volumeHandle{}, fmt.Errorf("volume REST port does not match this driver instance")
		}
	}
	return h, nil
}

func signVolumeHandle(payload string, key []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(volumeHandlePrefix))
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h volumeHandle) validate() error {
	if h.Protocol != protocolNFS && h.Protocol != protocolSMB {
		return fmt.Errorf("volume protocol is invalid")
	}
	if h.Endpoint == "" || h.RESTPort == "" || h.Server == "" || h.FSPath == "" || h.DirectoryID == "" || h.ResourceID == "" || h.ResourceName == "" || h.SpecFingerprint == "" {
		return fmt.Errorf("volume ID is missing required fields")
	}
	fingerprint, err := hex.DecodeString(h.SpecFingerprint)
	if err != nil || len(fingerprint) != sha256.Size || strings.ToLower(h.SpecFingerprint) != h.SpecFingerprint {
		return fmt.Errorf("volume specification fingerprint is invalid")
	}
	if strings.ContainsAny(h.FSPath, "\x00\r\n") || !strings.HasPrefix(h.FSPath, "/") || h.FSPath == "/" || path.Clean(h.FSPath) != h.FSPath {
		return fmt.Errorf("volume filesystem path is unsafe")
	}
	if strings.ContainsAny(h.Endpoint+h.Server+h.ResourceName+h.RESTPort, "\x00\r\n") {
		return fmt.Errorf("volume ID contains control characters")
	}
	port, err := strconv.ParseUint(h.RESTPort, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("volume REST port is invalid")
	}
	configuredPort, err := configuredRESTPort(h.Endpoint, h.RESTPort)
	if err != nil || configuredPort != h.RESTPort {
		return fmt.Errorf("volume endpoint is invalid")
	}
	if err := validateDataServer(h.Server); err != nil {
		return fmt.Errorf("volume data server is invalid: %w", err)
	}
	if h.Protocol == protocolNFS {
		if !strings.HasPrefix(h.ResourceName, "/") || path.Clean(h.ResourceName) != h.ResourceName {
			return fmt.Errorf("NFS export path is unsafe")
		}
		if h.NFSVersion != "3" && h.NFSVersion != "4.1" {
			return fmt.Errorf("NFS version is invalid")
		}
	} else if strings.Trim(h.ResourceName, "/") != h.ResourceName || strings.Contains(h.ResourceName, "\\") {
		return fmt.Errorf("SMB share name is unsafe")
	}
	return nil
}

func volumeResourceName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "volume"
	}
	sum := sha256.Sum256([]byte(name))
	// Use a 128-bit suffix because Kubernetes-generated PVC names frequently
	// share a long prefix that must be truncated to fit backend name limits.
	// The original request name is also stored in the immutable specification
	// marker, so even a hypothetical digest collision cannot alias a volume.
	suffix := "-" + hex.EncodeToString(sum[:16])
	if len(base)+len(suffix) > 63 {
		base = strings.TrimRight(base[:63-len(suffix)], "-")
	}
	return base + suffix
}
