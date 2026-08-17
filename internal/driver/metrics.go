package driver

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/rsnedigar-cell/cosi-driver-qumulo/internal/qumulo"
)

type Metrics struct {
	rpcCount    *prometheus.CounterVec
	rpcLatency  *prometheus.HistogramVec
	qumuloClass *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	return newMetrics(prometheus.DefaultRegisterer)
}

// NewTestMetrics registers on an isolated registry so tests can construct
// many Driver instances without colliding on the default registerer.
func NewTestMetrics() *Metrics {
	return newMetrics(prometheus.NewRegistry())
}

func newMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		rpcCount: f.NewCounterVec(prometheus.CounterOpts{
			Name: "qumulo_cosi_rpc_total",
			Help: "COSI RPCs by method and gRPC code",
		}, []string{"method", "code"}),
		rpcLatency: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "qumulo_cosi_rpc_duration_seconds",
			Help:    "COSI RPC latency",
			Buckets: prometheus.DefBuckets,
		}, []string{"method"}),
		qumuloClass: f.NewCounterVec(prometheus.CounterOpts{
			Name: "qumulo_cosi_api_errors_total",
			Help: "Qumulo REST errors by error_class",
		}, []string{"error_class"}),
	}
}

func (m *Metrics) UnaryInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)
		m.rpcCount.WithLabelValues(info.FullMethod, code.String()).Inc()
		m.rpcLatency.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
		if api, ok := qumulo.AsAPIError(err); ok {
			m.qumuloClass.WithLabelValues(api.ErrorClass).Inc()
		}
		if err != nil {
			log.Warn("rpc failed", "method", info.FullMethod, "code", code.String(), "err", err)
		} else {
			log.Debug("rpc ok", "method", info.FullMethod, "dur", time.Since(start))
		}
		return resp, err
	}
}
