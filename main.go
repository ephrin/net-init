package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// --- Configuration ---

type Config struct {
	WaitDeps         []Dependency
	HealthCheckPort  int
	HealthCheckPath  string
	MetricsPath      string
	Timeout          time.Duration
	RetryInterval    time.Duration
	LogLevel         slog.Level
	Cmd              []string // Command to execute after setup
	TlsSkipVerify    bool     // Global option for simplicity, could be per-dep
	TlsCACertPath    string   // Global option
	CustomCheckTimeout time.Duration // Timeout for custom script execution
}

type Dependency struct {
	Raw      string // Original string (e.g., "tcp://redis:6379")
	Type     string // "tcp", "udp", "http", "https", "exec"
	Target   string // Host:Port, URL, or command path
	Args     []string // Arguments for exec type
	isReady  atomic.Bool
	CheckFunc func(context.Context, Dependency, *http.Client) error
	Metric    prometheus.Gauge
}

// Default values
const (
	defaultHealthCheckPort = 8887
	defaultHealthCheckPath = "/health"
	defaultMetricsPath     = "/metrics"
	defaultTimeout         = 300 * time.Second
	defaultRetryInterval   = 5 * time.Second
	defaultLogLevel        = slog.LevelInfo
	defaultCustomCheckTimeout = 10 * time.Second
)

// --- Prometheus Metrics ---
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

// --- Main Logic ---

func main() {
	// Configure Logging early
	logLevel := defaultLogLevel
	if levelStr := os.Getenv("NETINIT_LOG_LEVEL"); levelStr != "" {
		var err error
		logLevel, err = parseLogLevel(levelStr)
		if err != nil {
			slog.Error("Invalid NETINIT_LOG_LEVEL", "value", levelStr, "error", err)
			os.Exit(1)
		}
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	logger := slog.New(jsonHandler)
	slog.SetDefault(logger)

	// --- Configuration Parsing ---
	cfg, err := parseConfig(os.Args[1:]) // Pass CMD args
	if err != nil {
		slog.Error("Configuration error", "error", err)
		os.Exit(1)
	}
	slog.Info("Net-Init starting", "config", fmt.Sprintf("%+v", cfg)) // Avoid logging raw command

	// --- Setup Context & Signal Handling ---
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel() // Ensure resources are released

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// --- Global Ready State ---
	var allReady atomic.Bool
	allReady.Store(false) // Start as not ready

	// --- HTTP Client (for http/https checks) ---
	httpClient := &http.Client{
		Timeout: cfg.RetryInterval / 2, // Timeout per check attempt < retry interval
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: cfg.TlsSkipVerify,
				// TODO: Load CA cert from cfg.TlsCACertPath if provided
			},
		},
	}

	// --- Start Dependency Checks Concurrently ---
	var wg sync.WaitGroup
	for i := range cfg.WaitDeps {
		wg.Add(1)
		go func(dep *Dependency) { // Pass pointer to modify isReady
			defer wg.Done()
			slog.Info("Starting check", "dependency", dep.Raw)
			ticker := time.NewTicker(cfg.RetryInterval)
			defer ticker.Stop()

			initialCheckDone := false
			for {
				select {
				case <-ctx.Done(): // Overall timeout or signal received
					slog.Warn("Stopping check due to context cancellation", "dependency", dep.Raw, "reason", ctx.Err())
					dep.Metric.Set(0) // Mark as down on exit
					return
				case <-ticker.C:
					if !initialCheckDone { // Check immediately on start
						initialCheckDone = true
					} else {
						// Ticker logic will handle subsequent checks
					}
				}

				// Create a context for this specific check attempt
				checkCtx, checkCancel := context.WithTimeout(ctx, cfg.RetryInterval)

				err := dep.CheckFunc(checkCtx, *dep, httpClient) // Pass value copy to check func
				checkCancel() // Release check context

				if err == nil {
					if !dep.isReady.Load() { // Status changed to Ready
						slog.Info("Dependency ready", "dependency", dep.Raw)
						dep.isReady.Store(true)
						dep.Metric.Set(1)
						// No need to keep checking aggressively once ready? Could add option later.
						// For now, keep checking to detect if it goes DOWN again.
					}
				} else {
					if dep.isReady.Load() { // Status changed to Not Ready
						slog.Warn("Dependency NOT ready (was ready before)", "dependency", dep.Raw, "error", err)
						dep.isReady.Store(false)
						dep.Metric.Set(0)
					} else {
						slog.Debug("Dependency not ready", "dependency", dep.Raw, "error", err)
						// Metric already 0 or set initially
					}
				}

				// Check if all dependencies are now ready
				if checkAllDependenciesReady(cfg.WaitDeps) {
					if !allReady.Load() {
						slog.Info("All dependencies are now ready!")
						allReady.Store(true)
						overallStatus.Set(1)
					}
				} else {
					if allReady.Load() {
						slog.Warn("Overall status changed to NOT READY (one or more dependencies failed)")
						allReady.Store(false)
						overallStatus.Set(0)
					}
				}
			}
		}(&cfg.WaitDeps[i]) // Pass pointer to goroutine
	}

	// --- Start Health Check Server ---
	http.HandleFunc(cfg.HealthCheckPath, func(w http.ResponseWriter, r *http.Request) {
		if allReady.Load() {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "OK")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "Waiting for dependencies")
		}
	})
	http.Handle(cfg.MetricsPath, promhttp.Handler())

	go func() {
		addr := fmt.Sprintf(":%d", cfg.HealthCheckPort)
		slog.Info("Starting health check server", "address", addr, "health_path", cfg.HealthCheckPath, "metrics_path", cfg.MetricsPath)
		if err := http.ListenAndServe(addr, nil); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Health check server failed", "error", err)
			// We probably can't recover from this, signal failure?
			// Sending SIGTERM to self might be an option if running as PID 1
			// syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
			// For now, log and continue; exec will likely happen anyway.
		}
	}()

	// --- Wait for Signal or Initial Checks (Optional Wait) ---
	// The design says "Runs the main application regardless of dependency state"
	// So we exec immediately after starting checks and the health server.
	// If we wanted to wait for *initial* check results or a signal, logic would go here.

	// --- Execute Main Application Command ---
	slog.Info("Executing main command", "command", strings.Join(cfg.Cmd, " "))
	if len(cfg.Cmd) == 0 {
		slog.Error("No command provided to execute")
		os.Exit(1)
	}

	cmdPath, err := exec.LookPath(cfg.Cmd[0])
	if err != nil {
		slog.Error("Failed to find command executable", "command", cfg.Cmd[0], "error", err)
		os.Exit(1)
	}

	// Before exec, stop listening for signals intended for net-init
	signal.Stop(sigChan)
	cancel() // Cancel the main context before exec

	// Replace the net-init process with the target command
	// Signals sent to the container will now go directly to the application
	err = syscall.Exec(cmdPath, cfg.Cmd, os.Environ())
	if err != nil {
		// This error only happens if Exec itself fails (e.g., permissions)
		slog.Error("Failed to execute command", "command", cmdPath, "error", err)
		os.Exit(1) // Exit net-init with an error
	}

	// --- Goroutine Management (Post-Exec - this won't be reached) ---
	// Normally, you might wait for goroutines, but Exec replaces the process.
	// wg.Wait() // This line will never be reached if Exec succeeds.
}

// --- Helper Functions ---

func parseLogLevel(levelStr string) (slog.Level, error) {
	switch strings.ToLower(levelStr) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return defaultLogLevel, fmt.Errorf("unknown log level: %s", levelStr)
	}
}

func parseConfig(args []string) (*Config, error) {
	cfg := &Config{
		HealthCheckPort:  defaultHealthCheckPort,
		HealthCheckPath:  defaultHealthCheckPath,
		MetricsPath:      defaultMetricsPath,
		Timeout:          defaultTimeout,
		RetryInterval:    defaultRetryInterval,
		LogLevel:         defaultLogLevel,
		TlsSkipVerify:    false,
		CustomCheckTimeout: defaultCustomCheckTimeout,
	}

	// Env Vars
	if portStr := os.Getenv("NETINIT_HEALTHCHECK_PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			cfg.HealthCheckPort = port
		} else {
			return nil, fmt.Errorf("invalid NETINIT_HEALTHCHECK_PORT: %w", err)
		}
	}
	if path := os.Getenv("NETINIT_HEALTHCHECK_PATH"); path != "" {
		cfg.HealthCheckPath = path
	}
	if path := os.Getenv("NETINIT_METRICS_PATH"); path != "" {
		cfg.MetricsPath = path
	}
	if timeoutStr := os.Getenv("NETINIT_TIMEOUT"); timeoutStr != "" {
		if timeout, err := strconv.Atoi(timeoutStr); err == nil {
			cfg.Timeout = time.Duration(timeout) * time.Second
		} else {
			return nil, fmt.Errorf("invalid NETINIT_TIMEOUT: %w", err)
		}
	}
	if intervalStr := os.Getenv("NETINIT_RETRY_INTERVAL"); intervalStr != "" {
		if interval, err := strconv.Atoi(intervalStr); err == nil {
			cfg.RetryInterval = time.Duration(interval) * time.Second
		} else {
			return nil, fmt.Errorf("invalid NETINIT_RETRY_INTERVAL: %w", err)
		}
	}
	if levelStr := os.Getenv("NETINIT_LOG_LEVEL"); levelStr != "" {
		level, err := parseLogLevel(levelStr)
		if err != nil {
			return nil, fmt.Errorf("invalid NETINIT_LOG_LEVEL: %w", err)
		}
		cfg.LogLevel = level
	}
	if skipVerifyStr := os.Getenv("NETINIT_TLS_SKIP_VERIFY"); skipVerifyStr == "true" {
		cfg.TlsSkipVerify = true
	}
	cfg.TlsCACertPath = os.Getenv("NETINIT_TLS_CA_CERT_PATH") // Store path, load later if needed

	// Parse Dependencies
	waitStr := os.Getenv("NETINIT_WAIT")
	if waitStr != "" {
		depStrings := strings.Split(waitStr, ",")
		cfg.WaitDeps = make([]Dependency, 0, len(depStrings))
		for _, depRaw := range depStrings {
			depRaw = strings.TrimSpace(depRaw)
			if depRaw == "" {
				continue
			}

			dep := Dependency{Raw: depRaw}
			parts := strings.SplitN(depRaw, "://", 2)

			if len(parts) == 1 { // Default to TCP
				dep.Type = "tcp"
				dep.Target = parts[0]
			} else {
				dep.Type = strings.ToLower(parts[0])
				dep.Target = parts[1]
			}

			switch dep.Type {
			case "tcp":
				dep.CheckFunc = checkTCP
			case "udp":
				dep.CheckFunc = checkUDP
			case "http", "https":
				dep.CheckFunc = checkHTTP
			case "exec":
				cmdParts := strings.Fields(dep.Target) // Basic splitting
				if len(cmdParts) == 0 {
					return nil, fmt.Errorf("invalid exec dependency format: '%s'", depRaw)
				}
				dep.Target = cmdParts[0] // The command/script path
				dep.Args = cmdParts[1:]   // The arguments
				dep.CheckFunc = checkExec
			default:
				return nil, fmt.Errorf("unsupported dependency type '%s' in '%s'", dep.Type, depRaw)
			}
			// Initialize metric for this dependency
			dep.Metric = depStatus.WithLabelValues(dep.Raw)
			dep.Metric.Set(0) // Start as down

			cfg.WaitDeps = append(cfg.WaitDeps, dep)
		}
	}

	// Remaining args are the command
	if len(args) == 0 && len(cfg.WaitDeps) > 0 { // Allow running net-init just for checks if no CMD
        slog.Warn("No command specified after ENTRYPOINT arguments. Net-init will run checks but not execute an application.")
        // In this mode, we might want to wait differently, e.g., until context cancelation
        // For now, it will setup checks and the server, then potentially exit if main has nothing else to do
        // A `select{}` loop could keep it alive listening for signals if no command is given.
	} else {
		cfg.Cmd = args
	}


	return cfg, nil
}

func checkAllDependenciesReady(deps []Dependency) bool {
	if len(deps) == 0 {
		return true // No dependencies to wait for
	}
	for i := range deps {
		if !deps[i].isReady.Load() {
			return false
		}
	}
	return true
}

// --- Specific Check Functions ---

func checkTCP(ctx context.Context, dep Dependency, _ *http.Client) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", dep.Target)
	if err != nil {
		return err
	}
	return conn.Close()
}

func checkUDP(ctx context.Context, dep Dependency, _ *http.Client) error {
	// UDP is connectionless, "dialing" doesn't guarantee reachability like TCP.
	// A simple dial might succeed even if the host is down but network path exists.
	// A better check might involve sending data and expecting a response,
	// but that's application-specific. This is a basic "can resolve/route" check.
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", dep.Target)
	if err != nil {
		return err
	}
	// Could attempt a write here, but it might block or succeed falsely.
	// For simplicity, just closing the "connection".
	return conn.Close()
}

func checkHTTP(ctx context.Context, dep Dependency, client *http.Client) error {
	reqUrl := dep.Target
	if !strings.HasPrefix(reqUrl, "http://") && !strings.HasPrefix(reqUrl, "https://") {
		reqUrl = fmt.Sprintf("%s://%s", dep.Type, dep.Target) // Add scheme if missing
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqUrl, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		// Check if the error is a timeout
		if urlErr, ok := err.(*url.Error); ok && urlErr.Timeout() {
		    return fmt.Errorf("request timed out: %w", urlErr)
		}
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) // Consume body to allow connection reuse

	if resp.StatusCode < 200 || resp.StatusCode >= 300 { // Check for 2xx success
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

func checkExec(ctx context.Context, dep Dependency, _ *http.Client) error {
	// Use the overall retry interval context, but potentially add a shorter specific timeout for the exec itself
	cmdCtx, cancel := context.WithTimeout(ctx, defaultCustomCheckTimeout) // Use a specific timeout for the script
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, dep.Target, dep.Args...)
	// Prevent script output from mixing with net-init logs unless debugging
	// cmd.Stdout = os.Stdout
	// cmd.Stderr = os.Stderr
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard


	err := cmd.Run() // Runs command and waits for completion or context cancelation

	if cmdCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("custom check script timed out after %v", defaultCustomCheckTimeout)
	}

	if err != nil {
		// Check if it's an ExitError to provide more context
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("script exited with non-zero status (%d): %w", exitErr.ExitCode(), err)
		}
		// Other errors (e.g., command not found)
		return fmt.Errorf("script execution failed: %w", err)
	}

	// Exit code 0 means success
	return nil
}