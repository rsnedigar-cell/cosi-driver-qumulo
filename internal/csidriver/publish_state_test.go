package csidriver

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPublishStateRoundTripAtomicReplaceAndPermissions(t *testing.T) {
	root := t.TempDir()
	cfg := Config{StateDir: root}
	target := filepath.Join(root, "pods", "pod-a", "target")
	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)
	if err := writePublishState(cfg, target, first); err != nil {
		t.Fatal(err)
	}
	if err := writePublishState(cfg, target, second); err != nil {
		t.Fatal(err)
	}
	got, err := readPublishState(cfg, target)
	if err != nil || got != second {
		t.Fatalf("readPublishState() = %q, %v; want replacement %q", got, err, second)
	}
	dir, err := publishStateDirectory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path, err := publishStatePath(cfg, target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("publish state directory mode=%v, want 0700", dirInfo.Mode().Perm())
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fileInfo.Mode().Perm() != 0o600 {
			t.Fatalf("publish state file mode=%v, want 0600", fileInfo.Mode().Perm())
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), target) {
		t.Fatal("durable publish marker stored the target path instead of only its digest")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readPublishState(cfg, target); err == nil {
			t.Fatal("overly permissive durable publish marker was accepted")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removePublishState(cfg, target); err != nil {
		t.Fatal(err)
	}
	if _, err := readPublishState(cfg, target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed publish state remained readable: %v", err)
	}
}

func TestPublishStateUsesPersistentCSISocketDirectory(t *testing.T) {
	cfg := Config{Address: "unix:///csi/csi.sock", StateDir: "/transient-state"}
	dir, err := publishStateDirectory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != publishStateDirectoryName || filepath.Clean(filepath.Dir(dir)) != filepath.Clean("/csi") {
		t.Fatalf("publish state directory=%q, want a directory beside /csi/csi.sock", dir)
	}
}

func TestPublishStateRejectsSymlinks(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		root := t.TempDir()
		external := t.TempDir()
		cfg := Config{StateDir: root}
		dir, err := publishStateDirectory(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, dir); err != nil {
			t.Skipf("symlinks are unavailable: %v", err)
		}
		if err := writePublishState(cfg, filepath.Join(root, "target"), strings.Repeat("a", 64)); err == nil {
			t.Fatal("symlinked durable publish state directory was accepted")
		}
	})
	t.Run("marker", func(t *testing.T) {
		root := t.TempDir()
		cfg := Config{StateDir: root}
		target := filepath.Join(root, "target")
		if _, err := ensurePublishStateDirectory(cfg); err != nil {
			t.Fatal(err)
		}
		path, err := publishStatePath(cfg, target)
		if err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(t.TempDir(), "external")
		if err := os.WriteFile(external, []byte("do not overwrite"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, path); err != nil {
			t.Skipf("symlinks are unavailable: %v", err)
		}
		if _, err := readPublishState(cfg, target); err == nil {
			t.Fatal("symlinked durable publish marker was read")
		}
		if err := writePublishState(cfg, target, strings.Repeat("a", 64)); err == nil {
			t.Fatal("symlinked durable publish marker was replaced")
		}
		raw, err := os.ReadFile(external)
		if err != nil || string(raw) != "do not overwrite" {
			t.Fatalf("external symlink target changed: %q err=%v", raw, err)
		}
	})
}
