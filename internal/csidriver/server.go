package csidriver

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func ListenAndServe(ctx context.Context, d *Driver) error {
	if d == nil {
		return fmt.Errorf("CSI driver is required")
	}
	if err := d.cfg.Validate(); err != nil {
		return err
	}
	network, endpoint, err := parseCSIAddress(d.cfg.Address)
	if err != nil {
		return err
	}
	if network == "unix" {
		if err := os.MkdirAll(filepath.Dir(endpoint), 0o750); err != nil {
			return fmt.Errorf("create CSI socket directory: %w", err)
		}
		if info, statErr := os.Lstat(endpoint); statErr == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return fmt.Errorf("refusing to replace non-socket CSI endpoint %q", endpoint)
			}
			if err := os.Remove(endpoint); err != nil {
				return fmt.Errorf("remove stale CSI socket: %w", err)
			}
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect CSI socket: %w", statErr)
		}
	}
	listener, err := net.Listen(network, endpoint)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", d.cfg.Address, err)
	}
	defer listener.Close()
	if network == "unix" {
		if err := os.Chmod(endpoint, 0o660); err != nil {
			return fmt.Errorf("set CSI socket permissions: %w", err)
		}
	}

	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, d)
	if d.controllerEnabled() {
		csi.RegisterControllerServer(server, d)
	}
	if d.nodeEnabled() {
		csi.RegisterNodeServer(server, d)
	}
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, healthServer)

	serveErr := make(chan error, 1)
	go func() {
		d.log.Info("CSI gRPC listening", "address", d.cfg.Address, "mode", d.cfg.Mode, "driver", d.cfg.Name)
		serveErr <- server.Serve(listener)
	}()
	select {
	case <-ctx.Done():
		server.GracefulStop()
		return nil
	case err := <-serveErr:
		server.Stop()
		return err
	}
}

func parseCSIAddress(address string) (network, endpoint string, err error) {
	switch {
	case strings.HasPrefix(address, "unix://"):
		endpoint = strings.TrimPrefix(address, "unix://")
		if !strings.HasPrefix(endpoint, "/") || endpoint == "/" || strings.ContainsAny(endpoint, "\x00\r\n") {
			return "", "", fmt.Errorf("CSI Unix socket endpoint must be an absolute file path")
		}
		return "unix", endpoint, nil
	case strings.HasPrefix(address, "/") && address != "/" && !strings.ContainsAny(address, "\x00\r\n"):
		return "unix", address, nil
	default:
		return "", "", fmt.Errorf("CSI endpoint must be an absolute Unix socket path or use unix://")
	}
}
