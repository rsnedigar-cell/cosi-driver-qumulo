//go:build !linux

package csidriver

import (
	"context"
	"fmt"
)

type unsupportedMounter struct{}

func newPlatformMounter() mounter { return &unsupportedMounter{} }

func (*unsupportedMounter) Lookup(context.Context, string) (mountRecord, bool, error) {
	return mountRecord{}, false, fmt.Errorf("CSI node mounting is supported only on Linux")
}

func (*unsupportedMounter) Mount(context.Context, string, string, string, []string) error {
	return fmt.Errorf("CSI node mounting is supported only on Linux")
}

func (*unsupportedMounter) Unmount(context.Context, string) error {
	return fmt.Errorf("CSI node mounting is supported only on Linux")
}
