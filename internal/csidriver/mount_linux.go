//go:build linux

package csidriver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type platformMounter struct{}

func newPlatformMounter() mounter { return &platformMounter{} }

func (m *platformMounter) Lookup(_ context.Context, target string) (mountRecord, bool, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return mountRecord{}, false, fmt.Errorf("read mount table: %w", err)
	}
	return parseMountInfo(raw, target)
}

func parseMountInfo(raw []byte, target string) (mountRecord, bool, error) {
	want, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return mountRecord{}, false, err
	}
	var matches []mountRecord
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+2 >= len(fields) {
			continue
		}
		mountPoint := unescapeMountInfo(fields[4])
		absMount, err := filepath.Abs(filepath.Clean(mountPoint))
		if err != nil {
			continue
		}
		if absMount == want {
			mountOptions := splitMountInfoOptions(fields[5])
			options := append([]string(nil), mountOptions...)
			if separator+3 < len(fields) {
				options = append(options, splitMountInfoOptions(fields[separator+3])...)
			}
			matches = append(matches, mountRecord{
				MountID:      fields[0],
				ParentID:     fields[1],
				Root:         unescapeMountInfo(fields[3]),
				FSType:       fields[separator+1],
				Source:       unescapeMountInfo(fields[separator+2]),
				MountOptions: mountOptions,
				Options:      options,
			})
		}
	}
	if len(matches) == 0 {
		return mountRecord{}, false, nil
	}

	// Stacked mounts share a mountpoint. Linux links each newer visible
	// mount to the hidden mount below it through parent_id, so the topmost
	// record is the one that is not the parent of another same-target mount.
	hidden := make(map[string]bool, len(matches))
	for _, record := range matches {
		hidden[record.ParentID] = true
	}
	var topmost *mountRecord
	for i := range matches {
		if hidden[matches[i].MountID] {
			continue
		}
		if topmost != nil {
			return mountRecord{}, false, fmt.Errorf("mount table contains ambiguous topmost mounts at %q", target)
		}
		topmost = &matches[i]
	}
	if topmost == nil {
		return mountRecord{}, false, fmt.Errorf("mount table contains a cyclic mount stack at %q", target)
	}
	return *topmost, true, nil
}

func (m *platformMounter) Mount(ctx context.Context, source, target, fsType string, options []string) error {
	if err := os.MkdirAll(target, 0o750); err != nil {
		return fmt.Errorf("create mount target: %w", err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("inspect mount target: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("mount target must be a real directory")
	}
	args := []string{"-t", fsType}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	args = append(args, source, target)
	out, err := exec.CommandContext(ctx, "mount", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %s volume: %w: %s", fsType, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *platformMounter) Unmount(ctx context.Context, target string) error {
	out, err := exec.CommandContext(ctx, "umount", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("unmount volume: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func unescapeMountInfo(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

func splitMountInfoOptions(value string) []string {
	parts := strings.Split(value, ",")
	options := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			options = append(options, unescapeMountInfo(part))
		}
	}
	return options
}
