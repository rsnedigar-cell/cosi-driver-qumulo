package driver

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	cosi "sigs.k8s.io/container-object-storage-interface/proto"
)

// ListenAndServe starts the COSI gRPC server and optional metrics HTTP.
func ListenAndServe(ctx context.Context, addr, metricsAddr string, d *Driver, log *slog.Logger) error {
	network, endpoint, err := parseAddress(addr)
	if err != nil {
		return err
	}
	if network == "unix" {
		if err := os.Remove(endpoint); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
		if dir := parentDir(endpoint); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
	}
	lis, err := net.Listen(network, endpoint)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	s := grpc.NewServer(
		grpc.UnaryInterceptor(d.metrics.UnaryInterceptor(log)),
	)
	cosi.RegisterIdentityServer(s, d)
	cosi.RegisterProvisionerServer(s, d)
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(s, hs)
	reflection.Register(s)

	errCh := make(chan error, 2)
	go func() {
		log.Info("cosi gRPC listening", "addr", addr)
		errCh <- s.Serve(lis)
	}()

	var httpSrv *http.Server
	if metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		httpSrv = &http.Server{Addr: metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			log.Info("metrics listening", "addr", metricsAddr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		s.GracefulStop()
		if httpSrv != nil {
			_ = httpSrv.Shutdown(context.Background())
		}
		return nil
	case err := <-errCh:
		s.GracefulStop()
		return err
	}
}

func parseAddress(addr string) (network, endpoint string, err error) {
	switch {
	case strings.HasPrefix(addr, "unix://"):
		return "unix", strings.TrimPrefix(addr, "unix://"), nil
	case strings.HasPrefix(addr, "tcp://"):
		return "tcp", strings.TrimPrefix(addr, "tcp://"), nil
	case strings.HasPrefix(addr, "/"):
		return "unix", addr, nil
	default:
		return "tcp", addr, nil
	}
}

func parentDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}
