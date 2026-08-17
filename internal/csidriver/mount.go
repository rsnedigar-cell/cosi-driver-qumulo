package csidriver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type mountRecord struct {
	MountID      string
	ParentID     string
	Root         string
	Source       string
	FSType       string
	MountOptions []string
	Options      []string
}

type mounter interface {
	Lookup(context.Context, string) (mountRecord, bool, error)
	Mount(context.Context, string, string, string, []string) error
	Unmount(context.Context, string) error
}

// reservedMountOptionKeys contains options that can redirect the mount source,
// inject credentials, weaken driver-selected transport security, or change the
// mount operation itself. mount.nfs and mount.cifs both have historical aliases
// for several of these options, so this boundary deliberately blocks every
// known spelling rather than relying on a short prefix list.
var reservedMountOptionKeys = map[string]struct{}{
	// Source, address, path, and service redirection.
	"addr": {}, "ip": {}, "mountaddr": {}, "mounthost": {}, "clientaddr": {},
	"unc": {}, "source": {}, "device": {}, "prefixpath": {}, "prepath": {},
	"port": {}, "mountport": {}, "nfsprog": {}, "mountprog": {},
	"servern": {}, "servernetbiosname": {},

	// CIFS credentials and authentication identity aliases.
	"cred": {}, "creds": {}, "credential": {}, "credentials": {},
	"credfile": {}, "credentialsfile": {},
	"user": {}, "username": {},
	"pass": {}, "password": {}, "pass2": {}, "password2": {},
	"domain": {}, "dom": {}, "workgroup": {}, "domainauto": {},
	"guest": {}, "nullauth": {}, "multiuser": {}, "cruid": {}, "upcall_target": {},
	"backupuid": {}, "backupgid": {}, "backup_uid": {}, "backup_gid": {},
	"snapshot": {},

	// Driver-controlled protocol, authentication, and transport security.
	"vers": {}, "version": {}, "nfsvers": {}, "mountvers": {}, "minorversion": {},
	"sec": {}, "security": {}, "seal": {}, "noseal": {}, "sign": {}, "nosign": {},
	"proto": {}, "protocol": {}, "mountproto": {},
	"tcp": {}, "tcp6": {}, "udp": {}, "udp6": {}, "rdma": {}, "rdma6": {},
	"xprtsec": {}, "soft": {}, "softerr": {}, "softreval": {}, "nosoftreval": {},
	"sharesock": {}, "nosharesock": {},

	// Options that change mount(8) control flow or rebind another filesystem.
	"bind": {}, "rbind": {}, "move": {}, "remount": {}, "defaults": {},
	"loop": {}, "offset": {}, "sizelimit": {}, "sloppy": {},
	"bg": {}, "background": {}, "nofail": {}, "auto": {}, "noauto": {},
	"dev": {}, "suid": {}, "users": {}, "owner": {}, "group": {},
	// The CSI readonly field is authoritative. Accepting ro/rw here and then
	// overriding it would silently ignore an incompatible caller request.
	"ro": {}, "rw": {},
}

func sanitizedMountFlags(flags []string) ([]string, error) {
	seen := map[string]struct{}{}
	seenByKey := map[string]string{}
	contradictory := map[string]string{
		"exec": "noexec", "noexec": "exec",
		"atime": "noatime", "noatime": "atime",
		"sync": "async", "async": "sync",
		"ro": "rw", "rw": "ro",
	}
	for _, raw := range flags {
		for _, item := range strings.Split(raw, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if strings.ContainsAny(item, "\x00\r\n") {
				return nil, fmt.Errorf("mount option contains a control character")
			}
			key, err := mountOptionKey(item)
			if err != nil {
				return nil, err
			}
			// X-mount.* options are interpreted by util-linux itself rather
			// than by the filesystem helper. They can select a subtree, turn
			// off path canonicalization, change ownership/mode after mounting,
			// or create an idmapped mount, so none are caller-controlled here.
			if strings.HasPrefix(key, "x-mount.") || strings.HasPrefix(key, "x-mount-") {
				return nil, fmt.Errorf("mount option %q is controlled by the Qumulo CSI driver", key)
			}
			if _, blocked := reservedMountOptionKeys[key]; blocked {
				return nil, fmt.Errorf("mount option %q is controlled by the Qumulo CSI driver", key)
			}
			_, value, hasValue := strings.Cut(item, "=")
			normalized := key
			if hasValue {
				normalized += "=" + value
			}
			if previous, exists := seenByKey[key]; exists {
				if previous != normalized {
					return nil, fmt.Errorf("mount option %q is specified with conflicting values", key)
				}
				continue
			}
			if opposite := contradictory[key]; opposite != "" {
				if _, exists := seenByKey[opposite]; exists {
					return nil, fmt.Errorf("mount options %q and %q are contradictory", key, opposite)
				}
			}
			seenByKey[key] = normalized
			seen[normalized] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for option := range seen {
		out = append(out, option)
	}
	sort.Strings(out)
	return out, nil
}

func mountOptionKey(option string) (string, error) {
	key, _, _ := strings.Cut(option, "=")
	if key == "" || key != strings.TrimSpace(key) || strings.HasPrefix(key, "-") {
		return "", fmt.Errorf("mount option has an invalid key")
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return "", fmt.Errorf("mount option has an invalid key")
	}
	return strings.ToLower(key), nil
}

type recordedMountOptions struct {
	flags  map[string]bool
	values map[string]string
}

func newRecordedMountOptions(options []string) recordedMountOptions {
	state := recordedMountOptions{flags: map[string]bool{}, values: map[string]string{}}
	for _, raw := range options {
		for _, item := range strings.Split(raw, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			key, err := mountOptionKey(item)
			if err != nil {
				continue
			}
			if _, value, found := strings.Cut(item, "="); found {
				state.values[key] = value
				continue
			}
			state.flags[key] = true
		}
	}
	return state
}

func expectedMountFSType(h volumeHandle) string {
	if h.Protocol == protocolSMB {
		return "cifs"
	}
	if h.NFSVersion == "3" {
		return "nfs"
	}
	return "nfs4"
}

func validateMountedVolumeIdentity(h volumeHandle, record mountRecord) error {
	if !sameMountSource(h.Protocol, record.Source, mountSource(h)) {
		return fmt.Errorf("existing mount has an incompatible source")
	}
	if !strings.EqualFold(strings.TrimSpace(record.FSType), expectedMountFSType(h)) {
		return fmt.Errorf("existing mount has an incompatible filesystem type")
	}
	// This driver always mounts the root of the signed NFS export or SMB
	// share directly. A different mountinfo root identifies a bind mount or
	// X-mount.subdir view whose source and filesystem type may still match.
	if record.Root != "/" {
		return fmt.Errorf("existing mount exposes an incompatible filesystem subtree")
	}
	return nil
}

func validateExistingMount(h volumeHandle, record mountRecord, readOnly bool, requestedFlags []string) error {
	if err := validateMountedVolumeIdentity(h, record); err != nil {
		return err
	}

	state := newRecordedMountOptions(record.Options)
	modeOptions := record.MountOptions
	if len(modeOptions) == 0 {
		// Test and alternate mounters may provide only the combined option set.
		modeOptions = record.Options
	}
	modeState := newRecordedMountOptions(modeOptions)
	ro, rw := modeState.flags["ro"], modeState.flags["rw"]
	if ro == rw || ro != readOnly {
		return fmt.Errorf("existing mount has an incompatible read-only state")
	}
	if !state.flags["nodev"] || !state.flags["nosuid"] {
		return fmt.Errorf("existing mount is missing required security options")
	}

	requireValue := func(want string, keys ...string) bool {
		for _, key := range keys {
			if got, ok := state.values[key]; ok && strings.EqualFold(got, want) {
				return true
			}
		}
		return false
	}
	if h.Protocol == protocolSMB {
		if !requireValue("3.1.1", "vers", "version") {
			return fmt.Errorf("existing SMB mount has an incompatible protocol version")
		}
		if h.SMBEncrypted && !state.flags["seal"] {
			return fmt.Errorf("existing SMB mount is missing required encryption")
		}
		return validateRequestedMountOptions(record, requestedFlags, readOnly)
	}

	version := h.NFSVersion
	if version == "" {
		version = "4.1"
	}
	if !requireValue(version, "vers", "nfsvers") {
		return fmt.Errorf("existing NFS mount has an incompatible protocol version")
	}
	if !requireValue("sys", "sec", "security") {
		return fmt.Errorf("existing NFS mount has an incompatible security flavor")
	}
	if !state.flags["hard"] || state.flags["soft"] || state.flags["softerr"] {
		return fmt.Errorf("existing NFS mount has an incompatible retry policy")
	}
	if !requireValue("600", "timeo") || !requireValue("2", "retrans") {
		return fmt.Errorf("existing NFS mount has incompatible timeout settings")
	}
	return validateRequestedMountOptions(record, requestedFlags, readOnly)
}

func validateRequestedMountOptions(record mountRecord, requested []string, readOnly bool) error {
	effective, err := mergeMountOptions(nil, requested, readOnly)
	if err != nil {
		return err
	}
	state := newRecordedMountOptions(record.Options)
	for _, option := range effective {
		key, err := mountOptionKey(option)
		if err != nil {
			return err
		}
		// The per-mount read-only state is checked against MountOptions in
		// validateExistingMount. The superblock option set can legitimately
		// still contain rw for a read-only view.
		if key == "ro" || key == "rw" {
			continue
		}
		if _, value, found := strings.Cut(option, "="); found {
			if got, ok := state.values[key]; !ok || got != value {
				return fmt.Errorf("existing mount does not contain requested option %q", key)
			}
			continue
		}
		if !state.flags[key] {
			return fmt.Errorf("existing mount does not contain requested option %q", key)
		}
	}
	return nil
}

func mergeMountOptions(base, supplied []string, readOnly bool) ([]string, error) {
	clean, err := sanitizedMountFlags(supplied)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	flags := map[string]bool{}
	add := func(option string) {
		key, value, found := strings.Cut(option, "=")
		if found {
			values[strings.ToLower(key)] = key + "=" + value
			return
		}
		flags[strings.ToLower(option)] = true
	}
	for _, option := range base {
		add(option)
	}
	for _, option := range clean {
		add(option)
	}
	if readOnly {
		delete(flags, "rw")
		flags["ro"] = true
	} else {
		delete(flags, "ro")
		flags["rw"] = true
	}
	var out []string
	for _, value := range values {
		out = append(out, value)
	}
	for flag := range flags {
		out = append(out, flag)
	}
	sort.Strings(out)
	return out, nil
}

func validateTargetPath(root, target string) error {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(target) == "" {
		return fmt.Errorf("kubelet root and target path are required")
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) {
		return fmt.Errorf("kubelet root and target path must be absolute")
	}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("target path %q is outside kubelet root %q", target, root)
	}
	return rejectExistingSymlinkComponents(absRoot, rel)
}

func rejectExistingSymlinkComponents(root, rel string) error {
	current := root
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect target path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("target path contains symlink component %q", current)
		}
	}
	return nil
}

func writeSMBCredentials(stateDir string, secrets map[string]string) (string, func(), error) {
	username := strings.TrimSpace(secrets["username"])
	password := secrets["password"]
	domain := strings.TrimSpace(secrets["domain"])
	if username == "" || password == "" {
		return "", func() {}, fmt.Errorf("SMB node-publish secret must contain username and password")
	}
	for key, value := range map[string]string{"username": username, "password": password, "domain": domain} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return "", func() {}, fmt.Errorf("SMB %s contains a control character", key)
		}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create CSI state directory: %w", err)
	}
	info, err := os.Lstat(stateDir)
	if err != nil {
		return "", func() {}, fmt.Errorf("inspect CSI state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", func() {}, fmt.Errorf("CSI state path must be a real directory")
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("secure CSI state directory: %w", err)
	}
	file, err := os.CreateTemp(stateDir, "smb-credentials-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create SMB credentials file: %w", err)
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, err
	}
	content := "username=" + username + "\npassword=" + password + "\n"
	if domain != "" {
		content += "domain=" + domain + "\n"
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return file.Name(), cleanup, nil
}
