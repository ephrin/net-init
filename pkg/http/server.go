package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server represents a health check and metrics HTTP server
type Server struct {
	server  *http.Server
	port    int
	paths   Paths
	isReady *atomic.Bool
}

// Paths defines the URL paths for health and metrics endpoints
type Paths struct {
	Health  string
	Metrics string
}

// NewServer creates a new health check server
func NewServer(port int, paths Paths, isReady *atomic.Bool) *Server {
	return &Server{
		port:    port,
		paths:   paths,
		isReady: isReady,
	}
}

// Start launches the health check server
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc(s.paths.Health, func(w http.ResponseWriter, r *http.Request) {
		if s.isReady.Load() {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "OK")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "Waiting for dependencies")
		}
	})

	// Prometheus metrics endpoint
	mux.Handle(s.paths.Metrics, promhttp.Handler())

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	go func() {
		slog.Info("Starting health check server", "address", s.server.Addr)
		if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Health check server failed", "error", err)
		}
	}()

	return nil
}

// Shutdown stops the health check server gracefully
func (s *Server) Shutdown() error {
	if s.server == nil {
		return nil
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	slog.Info("Shutting down health check server...")
	if err := s.server.Shutdown(shutdownCtx); err != nil {
		slog.Error("Health check server shutdown failed", "error", err)
		return err
	}

	return nil
}
