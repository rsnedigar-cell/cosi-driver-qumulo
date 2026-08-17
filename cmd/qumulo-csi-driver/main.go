package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/csidriver"
)

var version = "0.2.0"

func main() {
	cfg := csidriver.ConfigFromEnv(version)
	flags := flag.NewFlagSet("qumulo-csi-driver", flag.ExitOnError)
	flags.StringVar(&cfg.Name, "driver-name", cfg.Name, "CSI driver name")
	flags.StringVar(&cfg.Address, "driver-address", cfg.Address, "CSI gRPC endpoint (unix:///path)")
	flags.StringVar(&cfg.Mode, "mode", cfg.Mode, "service mode: controller, node, or all")
	flags.StringVar(&cfg.NodeID, "node-id", cfg.NodeID, "Kubernetes node identifier (required in node mode)")
	flags.StringVar(&cfg.Endpoint, "endpoint", cfg.Endpoint, "Qumulo REST endpoint")
	flags.StringVar(&cfg.DataServer, "data-server", cfg.DataServer, "Qumulo NFS/SMB data-plane hostname")
	flags.StringVar(&cfg.RESTPort, "rest-port", cfg.RESTPort, "Qumulo REST port")
	flags.StringVar(&cfg.BasePath, "base-path", cfg.BasePath, "Qumulo filesystem base path for CSI volumes")
	flags.StringVar(&cfg.VersionFloor, "version-floor", cfg.VersionFloor, "minimum supported Qumulo Core version")
	flags.StringVar(&cfg.CredentialsDir, "credentials-dir", cfg.CredentialsDir, "mounted Qumulo controller credential directory")
	flags.StringVar(&cfg.CAFile, "ca-file", cfg.CAFile, "Qumulo REST CA bundle")
	flags.BoolVar(&cfg.InsecureSkipTLSVerify, "insecure-skip-tls-verify", cfg.InsecureSkipTLSVerify, "disable Qumulo REST TLS verification")
	flags.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "private node state directory")
	flags.StringVar(&cfg.KubeletRoot, "kubelet-root", cfg.KubeletRoot, "host kubelet root directory")
	flags.StringVar(&cfg.HandleKeyFile, "handle-key-file", cfg.HandleKeyFile, "shared CSI volume-handle signing key file")
	allowed := flags.String("allowed-networks", strings.Join(cfg.DefaultAllowedNetworks, ","), "default comma-separated NFS/SMB client networks")
	verbosity := flags.Int("v", 0, "log verbosity (0=info, 1+=debug)")
	_ = flags.Parse(os.Args[1:])
	cfg.DefaultAllowedNetworks = splitList(*allowed)
	if err := cfg.LoadHandleKey(); err != nil {
		fatal(err.Error())
	}
	if err := cfg.Validate(); err != nil {
		fatal(err.Error())
	}
	level := slog.LevelInfo
	if *verbosity > 0 {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	log.Info("starting Qumulo CSI driver", "version", version, "mode", cfg.Mode, "name", cfg.Name)

	driver := csidriver.New(cfg, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := csidriver.ListenAndServe(ctx, driver); err != nil {
		log.Error("CSI server exited", "err", err)
		os.Exit(1)
	}
}

func splitList(raw string) []string {
	var out []string
	for _, value := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func fatal(message string) {
	_, _ = os.Stderr.WriteString(message + "\n")
	os.Exit(2)
}
