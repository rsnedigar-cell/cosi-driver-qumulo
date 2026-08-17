package csidriver

import (
	"strings"
	"testing"
)

var testHandleKey = []byte("0123456789abcdef0123456789abcdef")
var testSpecFingerprint = strings.Repeat("a", 64)

func TestConfigValidateNormalizesControllerBoundary(t *testing.T) {
	cfg := Config{
		Name: DefaultDriverName, Mode: "controller", Address: "unix:///csi/csi.sock",
		Endpoint: "https://q.example:8443", BasePath: "/k8s-volumes/", HandleKey: testHandleKey,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.RESTPort != "8443" || cfg.DataServer != "q.example" || cfg.BasePath != "/k8s-volumes" {
		t.Fatalf("unexpected normalized config: %#v", cfg)
	}
	cfg.Mode, cfg.NodeID, cfg.KubeletRoot, cfg.StateDir = "node", "node-a", "/", "/state"
	if err := cfg.Validate(); err == nil {
		t.Fatal("filesystem-root kubelet path was accepted")
	}
}

func TestParseVolumeOptionsRequiresRestrictedNetwork(t *testing.T) {
	cfg := Config{Endpoint: "q.example", DataServer: "q-data.example", RESTPort: "8000", BasePath: "/k8s"}
	if _, err := parseVolumeOptions(cfg, "pvc-a", map[string]string{"protocol": "nfs"}); err == nil {
		t.Fatal("expected allowedNetworks validation error")
	}
	opts, err := parseVolumeOptions(cfg, "pvc-a", map[string]string{
		"protocol":        "nfs",
		"allowedNetworks": "10.0.0.0/8,192.168.1.0/24",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Protocol != protocolNFS || opts.FSPath == "" || opts.NFSExportPath == "" {
		t.Fatalf("unexpected options: %#v", opts)
	}
	if !opts.NFSRootSquash || opts.NFSAnonymousUID != "65534" || opts.NFSAnonymousGID != "65534" {
		t.Fatalf("secure NFS defaults were not applied: %#v", opts)
	}
	if _, err := parseVolumeOptions(cfg, "pvc-a", map[string]string{
		"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "allowAllHosts": "true",
	}); err == nil {
		t.Fatal("contradictory restricted and allow-all network settings were accepted")
	}
	defaults := cfg
	defaults.DefaultAllowedNetworks = []string{"10.0.0.0/8"}
	all, err := parseVolumeOptions(defaults, "pvc-a", map[string]string{
		"protocol": "nfs", "allowAllHosts": "true",
	})
	if err != nil || !all.AllowAllHosts || len(all.AllowedNetworks) != 0 {
		t.Fatalf("explicit allow-all did not override process defaults cleanly: opts=%#v err=%v", all, err)
	}
}

func TestParseVolumeOptionsRejectsUnknownAndUnsafePathParameters(t *testing.T) {
	cfg := Config{Endpoint: "q.example", DataServer: "q-data.example", RESTPort: "8000", BasePath: "/k8s"}
	tests := []map[string]string{
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "deleteDat": "false"},
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "basePath": "/safe/../other"},
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "basePath": "/safe\n"},
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "nfsExportPrefix": "relative"},
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "nfsExportPrefix": "/safe/../other"},
		{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "nfsExportPrefix": "/safe\nother"},
	}
	for _, params := range tests {
		if _, err := parseVolumeOptions(cfg, "pvc-a", params); err == nil {
			t.Fatalf("unsafe parameters were accepted: %v", params)
		}
	}
}

func TestParseVolumeOptionsPreservesDocumentedCSISecretParameters(t *testing.T) {
	cfg := Config{Endpoint: "q.example", DataServer: "q-data.example", RESTPort: "8000", BasePath: "/k8s"}
	params := map[string]string{
		"protocol": "smb", "allowedNetworks": "10.0.0.0/8", "smbTrusteeDomain": "LOCAL", "smbTrusteeName": "k8s-smb",
		"csi.storage.k8s.io/node-publish-secret-name":      "qumulo-smb-credentials",
		"csi.storage.k8s.io/node-publish-secret-namespace": "storage",
	}
	opts, err := parseVolumeOptions(cfg, "pvc-a", params)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Parameters["csi.storage.k8s.io/node-publish-secret-name"] != "qumulo-smb-credentials" {
		t.Fatalf("documented CSI secret plumbing was not retained: %#v", opts.Parameters)
	}
}

func TestParseSMBOptionsRequiresExplicitTrustee(t *testing.T) {
	cfg := Config{Endpoint: "q.example", DataServer: "q-data.example", RESTPort: "8000", BasePath: "/k8s"}
	base := map[string]string{"protocol": "smb", "allowedNetworks": "10.0.0.0/8"}
	if _, err := parseVolumeOptions(cfg, "pvc-a", base); err == nil {
		t.Fatal("SMB share without a trustee was accepted")
	}
	base["smbTrusteeAuthID"] = "1234"
	opts, err := parseVolumeOptions(cfg, "pvc-a", base)
	if err != nil {
		t.Fatal(err)
	}
	if opts.SMBTrustee.AuthID != "1234" || !opts.SMBRequireEncryption {
		t.Fatalf("unexpected SMB options: %#v", opts)
	}
}

func TestParseVolumeOptionsRejectsUnsafeEndpoints(t *testing.T) {
	for _, cfg := range []Config{
		{Endpoint: "http://q.example", DataServer: "q-data.example", RESTPort: "8000", BasePath: "/k8s"},
		{Endpoint: "q.example", DataServer: "q-data.example:445", RESTPort: "8000", BasePath: "/k8s"},
		{Endpoint: "q.example/path", DataServer: "q-data.example", RESTPort: "8000", BasePath: "/k8s"},
	} {
		if _, err := parseVolumeOptions(cfg, "pvc-a", map[string]string{"protocol": "nfs", "allowedNetworks": "10.0.0.0/8"}); err == nil {
			t.Fatalf("unsafe config was accepted: %#v", cfg)
		}
	}
	cfg := Config{Endpoint: "https://q.example:8000", DataServer: "q-data.example", RESTPort: "8000", BasePath: "/k8s"}
	for _, endpoint := range []string{"http://q.example:8000", "https://q.example:8443", "https://other.example:8000"} {
		if _, err := parseVolumeOptions(cfg, "pvc-a", map[string]string{
			"protocol": "nfs", "allowedNetworks": "10.0.0.0/8", "endpoint": endpoint,
		}); err == nil {
			t.Fatalf("mismatched per-volume endpoint %q was accepted", endpoint)
		}
	}
}

func TestVolumeHandleRoundTripAndEndpointBinding(t *testing.T) {
	h := volumeHandle{
		Protocol: protocolSMB, Endpoint: "q.example", RESTPort: "8000",
		Server: "q-data.example", FSPath: "/k8s/smb/vol", DirectoryID: "dir-42", ResourceID: "42",
		ResourceName: "vol", SpecFingerprint: testSpecFingerprint, Capacity: 1024, DeleteData: true,
	}
	raw, err := h.encode(testHandleKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeVolumeHandle(raw, Config{Endpoint: "https://q.example:8000", DataServer: "q-data.example", HandleKey: testHandleKey})
	if err != nil {
		t.Fatal(err)
	}
	if got.ResourceID != "42" || got.Protocol != protocolSMB {
		t.Fatalf("unexpected handle: %#v", got)
	}
	if _, err := decodeVolumeHandle(raw, Config{Endpoint: "evil.example", DataServer: "q-data.example", HandleKey: testHandleKey}); err == nil {
		t.Fatal("expected endpoint binding failure")
	}
	replacement := "A"
	if strings.HasSuffix(raw, replacement) {
		replacement = "B"
	}
	tampered := raw[:len(raw)-1] + replacement
	if _, err := decodeVolumeHandle(tampered, Config{Endpoint: "q.example", DataServer: "q-data.example", HandleKey: testHandleKey}); err == nil {
		t.Fatal("expected signature validation failure")
	}
}

func TestVolumeHandleRejectsUnsafeEndpointAndServer(t *testing.T) {
	base := volumeHandle{
		Protocol: protocolNFS, Endpoint: "q.example", RESTPort: "8000",
		Server: "q-data.example", FSPath: "/k8s/nfs/vol", DirectoryID: "dir-42", ResourceID: "42",
		ResourceName: "/k8s/vol", SpecFingerprint: testSpecFingerprint, NFSVersion: "4.1",
	}
	unsafeEndpoint := base
	unsafeEndpoint.Endpoint = "http://q.example"
	if _, err := unsafeEndpoint.encode(testHandleKey); err == nil {
		t.Fatal("HTTP endpoint was accepted in a signed volume handle")
	}
	unsafeServer := base
	unsafeServer.Server = "q-data.example:2049"
	if _, err := unsafeServer.encode(testHandleKey); err == nil {
		t.Fatal("data server with a caller-controlled port was accepted")
	}
	for _, fingerprint := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("z", 64)} {
		invalidFingerprint := base
		invalidFingerprint.SpecFingerprint = fingerprint
		if _, err := invalidFingerprint.encode(testHandleKey); err == nil {
			t.Fatalf("invalid specification fingerprint %q was accepted", fingerprint)
		}
	}
}

func TestParseCSIAddressRequiresAbsoluteUnixSocket(t *testing.T) {
	for _, address := range []string{"unix:///csi/csi.sock", "/var/lib/csi/csi.sock"} {
		if network, endpoint, err := parseCSIAddress(address); err != nil || network != "unix" || endpoint == "" {
			t.Fatalf("valid address %q rejected: network=%q endpoint=%q err=%v", address, network, endpoint, err)
		}
	}
	for _, address := range []string{"unix://relative.sock", "tcp://127.0.0.1:10000", "/", ""} {
		if _, _, err := parseCSIAddress(address); err == nil {
			t.Fatalf("unsafe CSI address %q was accepted", address)
		}
	}
}

func TestVolumeResourceNameDeterministicAndBounded(t *testing.T) {
	name := "pvc-" + string(make([]byte, 200))
	a := volumeResourceName(name)
	b := volumeResourceName(name)
	if a != b || len(a) > 63 || len(a) < 10 {
		t.Fatalf("bad resource name %q", a)
	}
	parts := strings.Split(a, "-")
	if suffix := parts[len(parts)-1]; len(suffix) != 32 {
		t.Fatalf("resource name digest suffix=%q, want 128-bit hexadecimal", suffix)
	}

	// These two names collide when only the first 32 digest bits are used and
	// their identical long prefix is truncated. They must remain distinct.
	left := volumeResourceName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa51920")
	right := volumeResourceName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa106464")
	if left == right {
		t.Fatalf("distinct CSI names collapsed to backend resource %q", left)
	}
}
