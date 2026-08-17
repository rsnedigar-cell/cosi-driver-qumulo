package csidriver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const publishStateDirectoryName = ".qumulo-csi-publish-state"

type durablePublishState struct {
	Fingerprint string `json:"fingerprint"`
}

// publishStateDirectory deliberately lives beside the CSI socket. The node
// deployment mounts that directory from the kubelet plugin hostPath, while
// StateDir is a memory-backed directory used only for transient SMB secrets.
func publishStateDirectory(cfg Config) (string, error) {
	var base string
	if strings.TrimSpace(cfg.Address) != "" {
		network, endpoint, err := parseCSIAddress(cfg.Address)
		if err != nil {
			return "", err
		}
		if network != "unix" {
			return "", fmt.Errorf("durable publish state requires a Unix CSI socket")
		}
		base = filepath.Dir(filepath.Clean(endpoint))
	} else {
		// Config validation requires Address in a running driver. This fallback
		// keeps directly constructed unit-test drivers self-contained.
		base = strings.TrimSpace(cfg.StateDir)
	}
	if base == "" || base == "." {
		return "", fmt.Errorf("durable publish state directory is unavailable")
	}
	return filepath.Join(base, publishStateDirectoryName), nil
}

func publishStatePath(cfg Config, target string) (string, error) {
	dir, err := publishStateDirectory(cfg)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(target)))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}

func ensurePublishStateDirectory(cfg Config) (string, error) {
	dir, err := publishStateDirectory(cfg)
	if err != nil {
		return "", err
	}
	if err := validatePublishStateBase(dir); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create durable publish state directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect durable publish state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("durable publish state path must be a real directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("secure durable publish state directory: %w", err)
	}
	if runtime.GOOS != "windows" {
		secured, err := os.Lstat(dir)
		if err != nil || secured.Mode().Perm() != 0o700 {
			return "", fmt.Errorf("durable publish state directory permissions are not 0700")
		}
	}
	return dir, nil
}

func validatePublishStateBase(dir string) error {
	base := filepath.Dir(dir)
	baseInfo, err := os.Lstat(base)
	if err != nil {
		return fmt.Errorf("inspect CSI socket directory: %w", err)
	}
	if !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("CSI socket path must be a real directory")
	}
	if runtime.GOOS != "windows" && baseInfo.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("CSI socket directory must not be group- or world-writable")
	}
	return nil
}

func inspectPublishStateFile(path string, allowMissing bool) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("durable publish state file must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("durable publish state file permissions are not 0600")
	}
	if info.Size() <= 0 || info.Size() > 1024 {
		return nil, fmt.Errorf("durable publish state file has an invalid size")
	}
	return info, nil
}

func writePublishState(cfg Config, target, fingerprint string) error {
	if err := validatePublishFingerprint(fingerprint); err != nil {
		return err
	}
	dir, err := ensurePublishStateDirectory(cfg)
	if err != nil {
		return err
	}
	path, err := publishStatePath(cfg, target)
	if err != nil {
		return err
	}
	if _, err := inspectPublishStateFile(path, true); err != nil {
		return fmt.Errorf("inspect existing durable publish state: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".publish-*.tmp")
	if err != nil {
		return fmt.Errorf("create durable publish state: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	defer cleanup()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure durable publish state: %w", err)
	}
	if err := json.NewEncoder(temp).Encode(durablePublishState{Fingerprint: fingerprint}); err != nil {
		return fmt.Errorf("encode durable publish state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync durable publish state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close durable publish state: %w", err)
	}
	if runtime.GOOS == "windows" {
		// Windows cannot atomically replace an existing file with os.Rename.
		// The CSI node service is Linux-only; this branch supports unit tests.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replace durable publish state: %w", err)
		}
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("atomically replace durable publish state: %w", err)
	}
	if err := syncPublishStateDirectory(dir); err != nil {
		return err
	}
	return nil
}

func readPublishState(cfg Config, target string) (string, error) {
	dir, err := publishStateDirectory(cfg)
	if err != nil {
		return "", err
	}
	if err := validatePublishStateBase(dir); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("durable publish state path must be a real directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("durable publish state directory permissions are not 0700")
	}
	path, err := publishStatePath(cfg, target)
	if err != nil {
		return "", err
	}
	if _, err := inspectPublishStateFile(path, false); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	dec := json.NewDecoder(io.LimitReader(file, 1025))
	dec.DisallowUnknownFields()
	var state durablePublishState
	if err := dec.Decode(&state); err != nil {
		return "", fmt.Errorf("decode durable publish state: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("decode durable publish state: trailing data")
	}
	if err := validatePublishFingerprint(state.Fingerprint); err != nil {
		return "", err
	}
	return state.Fingerprint, nil
}

func removePublishState(cfg Config, target string) error {
	dir, err := publishStateDirectory(cfg)
	if err != nil {
		return err
	}
	if err := validatePublishStateBase(dir); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect durable publish state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("durable publish state path must be a real directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		return fmt.Errorf("durable publish state directory permissions are not 0700")
	}
	path, err := publishStatePath(cfg, target)
	if err != nil {
		return err
	}
	fileInfo, err := inspectPublishStateFile(path, true)
	if err != nil {
		return fmt.Errorf("inspect durable publish state before removal: %w", err)
	}
	if fileInfo == nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove durable publish state: %w", err)
	}
	return syncPublishStateDirectory(dir)
}

func syncPublishStateDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open durable publish state directory: %w", err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync durable publish state directory: %w", err)
	}
	return nil
}

func validatePublishFingerprint(fingerprint string) error {
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(fingerprint) != fingerprint {
		return fmt.Errorf("durable publish fingerprint is invalid")
	}
	return nil
}
