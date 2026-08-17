package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/config"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/driver"
	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/naming"
)

var version = "0.2.0"

func main() {
	cfg := config.FromEnv()
	fs := flag.NewFlagSet("qumulo-cosi-driver", flag.ExitOnError)
	fs.StringVar(&cfg.Address, "driver-address", cfg.Address, "gRPC listen address (unix:///path or host:port)")
	fs.StringVar(&cfg.Name, "driver-name", first(cfg.Name, naming.DriverName), "COSI driver name; must match BucketClass.driverName")
	fs.StringVar(&cfg.MetricsAddress, "metrics-address", cfg.MetricsAddress, "Prometheus /metrics listen address (empty to disable)")
	fs.StringVar(&cfg.DefaultEndpoint, "endpoint", cfg.DefaultEndpoint, "default Qumulo REST host")
	fs.StringVar(&cfg.DefaultRESTPort, "rest-port", cfg.DefaultRESTPort, "default Qumulo REST port")
	fs.StringVar(&cfg.Kubeconfig, "kubeconfig", cfg.Kubeconfig, "optional kubeconfig for out-of-cluster secret reads")
	fs.StringVar(&cfg.DriverNamespace, "secret-namespace", cfg.DriverNamespace, "default namespace for credentials secrets")
	extraNS := fs.String("secret-namespaces", strings.Join(cfg.SecretNamespaces, ","), "comma-separated extra namespaces the driver may read secrets from (* for all)")
	v := fs.Int("v", 0, "log verbosity (0=info, 1+=debug)")
	_ = fs.Parse(os.Args[1:])
	if *extraNS != "" {
		cfg.SecretNamespaces = splitCSV(*extraNS)
	}

	level := slog.LevelInfo
	if *v > 0 {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	log.Info("starting qumulo COSI driver", "version", version, "name", cfg.Name, "addr", cfg.Address)

	var secrets config.SecretReader
	const serviceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	hasKubeCredentials := cfg.Kubeconfig != ""
	if !hasKubeCredentials {
		if _, err := os.Stat(serviceAccountToken); err == nil {
			hasKubeCredentials = true
		} else if !os.IsNotExist(err) {
			log.Warn("cannot inspect Kubernetes service-account token; class-referenced secrets are disabled", "err", err)
		}
	}
	if hasKubeCredentials {
		ks, err := config.NewKubeSecrets(cfg.Kubeconfig)
		if err != nil {
			log.Warn("kubernetes client unavailable; class-referenced secrets will fail", "err", err)
		} else {
			secrets = ks
		}
	} else {
		log.Info("Kubernetes secret reader disabled; using mounted Qumulo credentials")
	}

	d := driver.New(cfg, secrets, log, driver.NewMetrics())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := driver.ListenAndServe(ctx, cfg.Address, cfg.MetricsAddress, d, log); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func first(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
