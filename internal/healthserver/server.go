package healthserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type CheckFunc func(ctx context.Context) error

type Server struct {
	addr       string
	readyCheck CheckFunc
	metricsReg *prometheus.Registry
	log        *slog.Logger
}

func New(addr string, readyCheck CheckFunc) *Server {
	return &Server{
		addr:       addr,
		readyCheck: readyCheck,
		log:        slog.Default(),
	}
}

// SetMetricsRegistry wires GET /metrics, serving the given registry.
// Unauthenticated, same as the global API's /metrics — the health
// server binds to the OOB/fabric side only, never internet-facing.
// Must be called before ListenAndServe.
func (s *Server) SetMetricsRegistry(reg *prometheus.Registry) {
	s.metricsReg = reg
}

func (s *Server) SetLogger(l *slog.Logger) {
	s.log = l
}

// buildMux assembles the route table. Extracted from ListenAndServe so
// tests can exercise routing (the /metrics conditional in particular)
// without binding a socket.
func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	if s.metricsReg != nil {
		mux.Handle("/metrics", promhttp.HandlerFor(s.metricsReg,
			promhttp.HandlerOpts{Registry: s.metricsReg}))
	}
	return mux
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.buildMux(),
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second,
	}

	s.log.Info("health server starting", "addr", s.addr)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.readyCheck == nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
		return
	}

	if err := s.readyCheck(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "not ready",
			"reason": err.Error(),
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}
