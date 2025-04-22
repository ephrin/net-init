package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/ephrin/net-init/pkg/application"
	"github.com/ephrin/net-init/pkg/config"
	"github.com/ephrin/net-init/pkg/dependencies"
	httpserver "github.com/ephrin/net-init/pkg/http"
	"github.com/prometheus/client_golang/prometheus"
)

// Prometheus Metrics
var (
	depStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "netinit_dependency_up",
			Help: "Status of network dependencies (1=up, 0=down).",
		},
		[]string{"dependency"},
	)
	overallStatus = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "netinit_overall_status",
			Help: "Overall status reflecting if all dependencies are ready (1=ready, 0=waiting).",
		},
	)
)

func init() {
	prometheus.MustRegister(depStatus)
	prometheus.MustRegister(overallStatus)
	overallStatus.Set(0) // Start as not ready
}

func main() {
	// 1. Setup Logging
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		slog.Error("Configuration error", "error", err)
		os.Exit(1)
	}
	config.SetupLogging(cfg.LogLevel)

	slog.Info("Net-Init starting", "config", fmt.Sprintf("Cmd=%v WaitDeps=%d StartImmediately=%t HealthPort=%d Timeout=%v Retry=%v LogLevel=%v",
		cfg.Cmd, len(cfg.WaitDeps), cfg.StartImmediately, cfg.HealthCheckPort, cfg.Timeout, cfg.RetryInterval, cfg.LogLevel))

	// 2. Setup Context and Shared Resources
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	// 3. Create HTTP Client for dependency checks
	httpClient := createHTTPClient(cfg)

	// 4. Set up wait synchronization and overall status
	var wg sync.WaitGroup
	var allReady atomic.Bool
	allReady.Store(false)
	readyChan := make(chan struct{})
	var readyOnce sync.Once

	// 5. Parse Dependencies
	deps, err := dependencies.NewDependencies(cfg.WaitDeps, depStatus)
	if err != nil {
		slog.Error("Failed to parse dependencies", "error", err)
		os.Exit(1)
	}

	// 6. Start HTTP Health Server
	var server *httpserver.Server
	if len(cfg.Cmd) > 0 {
		server = httpserver.NewServer(
			cfg.HealthCheckPort,
			httpserver.Paths{
				Health:  cfg.HealthCheckPath,
				Metrics: cfg.MetricsPath,
			},
			&allReady,
		)
		if err := server.Start(); err != nil {
			slog.Error("Failed to start HTTP server", "error", err)
		}
		defer server.Shutdown()
	}

	// 7. Start Dependency Checks
	checker := dependencies.NewChecker(
		deps,
		httpClient,
		readyChan,
		&readyOnce,
		&allReady,
		overallStatus,
		cfg.RetryInterval,
		&wg,
	)
	checker.Start(ctx)

	// 8. Execute Application
	runner := application.NewRunner(
		cfg.Cmd,
		cfg.StartImmediately,
		readyChan,
		&wg,
	)

	exitCode, err := runner.Execute(ctx)
	if err != nil {
		slog.Error("Application execution error", "error", err)
	}

	// 9. Final Cleanup and Exit
	slog.Info("Net-Init exiting", "exitCode", exitCode)
	cancel()  // Ensure context is cancelled
	wg.Wait() // Wait for dependency checks to fully stop
	os.Exit(exitCode)
}

// createHTTPClient creates the shared HTTP client.
func createHTTPClient(cfg *config.Config) *http.Client {
	return &http.Client{
		Timeout: cfg.RetryInterval / 2,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: cfg.TlsSkipVerify,
				// TODO: Load CA cert from cfg.TlsCACertPath if provided
			},
		},
	}
}
