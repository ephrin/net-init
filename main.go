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
	Cmd              []string
	TlsSkipVerify    bool
	TlsCACertPath    string
	StartImmediately bool
}

type Dependency struct {
	Raw       string
	Type      string
	Target    string
	Args      []string
	isReady   atomic.Bool
	CheckFunc func(context.Context, Dependency, *http.Client) error
	Metric    prometheus.Gauge
}

// --- Default values ---
const (
	defaultHealthCheckPort = 8887
	defaultHealthCheckPath = "/health"
	defaultMetricsPath     = "/metrics"
	defaultTimeout         = 300 * time.Second
	defaultRetryInterval   = 5 * time.Second
	defaultLogLevel        = slog.LevelInfo
)

var defaultCustomCheckTimeout = 10 * time.Second // Keep as var for tests

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

// --- Main Orchestration ---

func main() {
	// 1. Setup Logging
	setupLogging()

	// 2. Parse Configuration
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		slog.Error("Configuration error", "error", err)
		os.Exit(1)
	}
	slog.Info("Net-Init starting", "config", fmt.Sprintf("Cmd=%v WaitDeps=%d StartImmediately=%t HealthPort=%d Timeout=%v Retry=%v LogLevel=%v", cfg.Cmd, len(cfg.WaitDeps), cfg.StartImmediately, cfg.HealthCheckPort, cfg.Timeout, cfg.RetryInterval, cfg.LogLevel))

	// 3. Setup Context and Signal Handling for cancellation
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 4. Prepare Shared State and HTTP Client
	var allReady atomic.Bool
	allReady.Store(false)
	readyChan := make(chan struct{}) // Used only if !cfg.StartImmediately
	var readyOnce sync.Once
	httpClient := createHTTPClient(cfg)

	// 5. Start Health/Metrics Server
	httpServer := startHTTPServer(cfg, &allReady)
	defer shutdownHTTPServer(httpServer) // Ensure server is shut down

	// 6. Start Dependency Checks
	var wg sync.WaitGroup
	startDependencyChecks(ctx, cfg, &wg, readyChan, &readyOnce, &allReady, httpClient)

	// 7. Execute Application (conditionally waiting for dependencies)
	exitCode := executeApplication(ctx, cfg, sigChan, readyChan, &wg)

	// 8. Final Cleanup and Exit
	slog.Info("Net-Init exiting", "exitCode", exitCode)
	cancel()  // Ensure context is cancelled if not already
	wg.Wait() // Wait for dependency checks to fully stop
	os.Exit(exitCode)
}

// --- Helper Functions ---

// setupLogging configures the global logger based on environment variables.
func setupLogging() {
	logLevel := defaultLogLevel
	if levelStr := os.Getenv("NETINIT_LOG_LEVEL"); levelStr != "" {
		var err error
		logLevel, err = parseLogLevel(levelStr)
		if err != nil {
			// Use default logger initially for this error message
			slog.Error("Invalid NETINIT_LOG_LEVEL, using default", "value", levelStr, "error", err)
			logLevel = defaultLogLevel
		}
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	logger := slog.New(jsonHandler)
	slog.SetDefault(logger)
}

// parseLogLevel parses the log level string.
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
	case "":
		return defaultLogLevel, nil // Default for empty
	default:
		return defaultLogLevel, fmt.Errorf("unknown log level: %s", levelStr)
	}
}

// parseIntEnv parses an integer environment variable with validation.
func parseIntEnv(key string, defaultVal int, allowZero bool, allowNegative bool) (int, error) {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultVal, nil
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return defaultVal, fmt.Errorf("invalid %s format: %w", key, err)
	}
	if !allowZero && val == 0 {
		return defaultVal, fmt.Errorf("invalid %s: must not be zero", key)
	}
	if !allowNegative && val < 0 {
		return defaultVal, fmt.Errorf("invalid %s: must be non-negative", key)
	}
	// Add upper bounds check if needed (e.g., for ports)
	if key == "NETINIT_HEALTHCHECK_PORT" && (val <= 0 || val > 65535) {
		return defaultVal, fmt.Errorf("invalid %s value: %d", key, val)
	}
	return val, nil
}

// parseDurationEnv parses a duration (in seconds) environment variable.
func parseDurationEnv(key string, defaultVal time.Duration, requirePositive bool) (time.Duration, error) {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultVal, nil
	}
	seconds, err := strconv.Atoi(valStr)
	if err != nil {
		return defaultVal, fmt.Errorf("invalid %s format: %w", key, err)
	}
	if requirePositive && seconds <= 0 {
		return defaultVal, fmt.Errorf("invalid %s: must be positive", key)
	}
	if !requirePositive && seconds < 0 {
		return defaultVal, fmt.Errorf("invalid %s: must be non-negative", key)
	}
	return time.Duration(seconds) * time.Second, nil
}

// parseBoolEnv parses a boolean environment variable (true if "true").
func parseBoolEnv(key string, defaultVal bool) bool {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultVal
	}
	return strings.ToLower(valStr) == "true"
}

// parseConfig parses all configuration from environment variables.
func parseConfig(args []string) (*Config, error) {
	var err error
	cfg := &Config{
		HealthCheckPort:  defaultHealthCheckPort,
		HealthCheckPath:  defaultHealthCheckPath,
		MetricsPath:      defaultMetricsPath,
		Timeout:          defaultTimeout,
		RetryInterval:    defaultRetryInterval,
		LogLevel:         defaultLogLevel, // Will be updated below
		TlsSkipVerify:    false,
		StartImmediately: false, // Default is to wait
	}

	// Parse basic types
	cfg.HealthCheckPort, err = parseIntEnv("NETINIT_HEALTHCHECK_PORT", defaultHealthCheckPort, false, false)
	if err != nil {
		return nil, err
	}
	cfg.Timeout, err = parseDurationEnv("NETINIT_TIMEOUT", defaultTimeout, false)
	if err != nil {
		return nil, err
	}
	cfg.RetryInterval, err = parseDurationEnv("NETINIT_RETRY_INTERVAL", defaultRetryInterval, true)
	if err != nil {
		return nil, err
	}
	customTimeoutSec, err := parseIntEnv("NETINIT_CUSTOM_CHECK_TIMEOUT", int(defaultCustomCheckTimeout.Seconds()), true, false)
	if err != nil {
		return nil, err
	}
	defaultCustomCheckTimeout = time.Duration(customTimeoutSec) * time.Second // Update global var

	// Parse paths with validation
	if path := os.Getenv("NETINIT_HEALTHCHECK_PATH"); path != "" {
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("invalid NETINIT_HEALTHCHECK_PATH: must start with /")
		}
		cfg.HealthCheckPath = path
	}
	if path := os.Getenv("NETINIT_METRICS_PATH"); path != "" {
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("invalid NETINIT_METRICS_PATH: must start with /")
		}
		cfg.MetricsPath = path
	}

	// Parse LogLevel (allow default on error)
	logLevelStr := os.Getenv("NETINIT_LOG_LEVEL")
	parsedLevel, errLog := parseLogLevel(logLevelStr)
	if errLog != nil {
		slog.Warn("Invalid NETINIT_LOG_LEVEL specified, using default", "value", logLevelStr, "default", defaultLogLevel.String())
	}
	cfg.LogLevel = parsedLevel // Use parsed level or default if error

	// Parse booleans
	cfg.TlsSkipVerify = parseBoolEnv("NETINIT_TLS_SKIP_VERIFY", false)
	cfg.StartImmediately = parseBoolEnv("NETINIT_START_IMMEDIATELY", false)

	// Parse other strings
	cfg.TlsCACertPath = os.Getenv("NETINIT_TLS_CA_CERT_PATH")

	// Parse Dependencies
	cfg.WaitDeps, err = parseDependencies(os.Getenv("NETINIT_WAIT"))
	if err != nil {
		return nil, err
	}

	cfg.Cmd = args // Assign remaining arguments as the command
	return cfg, nil
}

// parseDependencies parses the NETINIT_WAIT string.
func parseDependencies(waitStr string) ([]Dependency, error) {
	if waitStr == "" {
		return nil, nil
	}

	depStrings := strings.Split(waitStr, ",")
	deps := make([]Dependency, 0, len(depStrings))
	uniqueDeps := make(map[string]struct{})

	for _, depRaw := range depStrings {
		depRaw = strings.TrimSpace(depRaw)
		if depRaw == "" {
			continue
		}
		if _, exists := uniqueDeps[depRaw]; exists {
			slog.Warn("Duplicate dependency specified, ignoring.", "dependency", depRaw)
			continue
		}
		uniqueDeps[depRaw] = struct{}{}

		dep, err := parseSingleDependency(depRaw)
		if err != nil {
			return nil, err // Propagate error
		}
		deps = append(deps, dep)
	}
	return deps, nil
}

// parseSingleDependency parses one dependency string entry.
func parseSingleDependency(depRaw string) (Dependency, error) {
	dep := Dependency{Raw: depRaw}
	parts := strings.SplitN(depRaw, "://", 2)

	if len(parts) == 1 { // Default to TCP
		target := parts[0]
		if !strings.Contains(target, ":") {
			return dep, fmt.Errorf("invalid default TCP dependency format (missing port?): '%s'", depRaw)
		}
		dep.Type = "tcp"
		dep.Target = target
	} else {
		depType := strings.ToLower(parts[0])
		target := parts[1]
		if target == "" {
			return dep, fmt.Errorf("missing target for dependency type '%s' in '%s'", depType, depRaw)
		}
		dep.Type = depType
		dep.Target = target
	}

	// Assign check function based on type
	switch dep.Type {
	case "tcp":
		if !strings.Contains(dep.Target, ":") {
			return dep, fmt.Errorf("invalid TCP dependency format (missing port?): '%s'", depRaw)
		}
		dep.CheckFunc = checkTCP
	case "udp":
		if !strings.Contains(dep.Target, ":") {
			return dep, fmt.Errorf("invalid UDP dependency format (missing port?): '%s'", depRaw)
		}
		dep.CheckFunc = checkUDP
	case "http", "https":
		dep.CheckFunc = checkHTTP
	case "exec":
		cmdParts := strings.Fields(dep.Target)
		if len(cmdParts) == 0 {
			return dep, fmt.Errorf("invalid exec dependency format (empty command): '%s'", depRaw)
		}
		dep.Target = cmdParts[0] // The command/script path
		dep.Args = cmdParts[1:]  // The arguments
		dep.CheckFunc = checkExec
	default:
		return dep, fmt.Errorf("unsupported dependency type '%s' in '%s'", dep.Type, depRaw)
	}

	// Initialize metric
	dep.Metric = depStatus.WithLabelValues(dep.Raw)
	return dep, nil
}

// createHTTPClient creates the shared HTTP client.
func createHTTPClient(cfg *Config) *http.Client {
	return &http.Client{
		Timeout: cfg.RetryInterval / 2, // Sensible default timeout per attempt
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: cfg.TlsSkipVerify,
				// TODO: Load CA cert from cfg.TlsCACertPath if provided
			},
			// Consider adding other transport settings like MaxIdleConnsPerHost
		},
	}
}

// startHTTPServer starts the health and metrics server in a goroutine.
func startHTTPServer(cfg *Config, allReady *atomic.Bool) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(cfg.HealthCheckPath, func(w http.ResponseWriter, r *http.Request) {
		if allReady.Load() {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "OK")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "Waiting for dependencies")
		}
	})
	mux.Handle(cfg.MetricsPath, promhttp.Handler())

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HealthCheckPort),
		Handler: mux,
	}

	go func() {
		slog.Info("Starting health check server", "address", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Health check server failed", "error", err)
			// Consider signaling main goroutine about this fatal error
		}
	}()
	return httpServer
}

// shutdownHTTPServer gracefully shuts down the HTTP server.
func shutdownHTTPServer(server *http.Server) {
	if server == nil {
		return
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	slog.Info("Shutting down health check server...")
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("Health check server shutdown failed", "error", err)
	}
}

// handleDependencyStateChange updates the dependency state and metrics
func handleDependencyStateChange(dep *Dependency, isReady bool, err error, checkOverallReady func()) bool {
	stateChanged := false

	if isReady { // Dependency is ready
		if !dep.isReady.Load() {
			slog.Info("Dependency ready", "dependency", dep.Raw)
			dep.isReady.Store(true)
			dep.Metric.Set(1)
			stateChanged = true
		}
	} else { // Dependency is not ready
		if dep.isReady.Load() {
			slog.Warn("Dependency NOT ready (was ready before)", "dependency", dep.Raw, "error", err)
			dep.isReady.Store(false)
			dep.Metric.Set(0)
			stateChanged = true
		} else {
			slog.Debug("Dependency not ready", "dependency", dep.Raw, "error", err)
		}
	}

	if stateChanged {
		checkOverallReady()
	}
	return stateChanged
}

// performDependencyCheck performs a single check of a dependency
func performDependencyCheck(ctx context.Context, dep *Dependency, httpClient *http.Client, retryInterval time.Duration) error {
	checkCtx, checkCancel := context.WithTimeout(ctx, retryInterval)
	defer checkCancel()
	return dep.CheckFunc(checkCtx, *dep, httpClient)
}

// handleContextCancellation handles cleanup when context is cancelled
func handleContextCancellation(dep *Dependency, checkOverallReady func(), reason error) {
	slog.Warn("Stopping check due to context cancellation", "dependency", dep.Raw, "reason", reason)
	if dep.isReady.Load() {
		dep.isReady.Store(false)
		dep.Metric.Set(0)
		checkOverallReady()
	}
}

// createOverallReadyChecker creates a function to check overall readiness
func createOverallReadyChecker(deps []Dependency, allReady *atomic.Bool, readyOnce *sync.Once, readyChan chan struct{}) func() {
	return func() {
		if checkAllDependenciesReady(deps) {
			if !allReady.Load() {
				slog.Info("All dependencies are now ready!")
				allReady.Store(true)
				overallStatus.Set(1)
				readyOnce.Do(func() { close(readyChan) }) // Signal readiness once
			}
		} else {
			if allReady.Load() {
				slog.Warn("Overall status changed to NOT READY")
				allReady.Store(false)
				overallStatus.Set(0)
			}
		}
	}
}

// handleNoDependencies marks the system as ready when no dependencies are specified
func handleNoDependencies(allReady *atomic.Bool, readyOnce *sync.Once, readyChan chan struct{}) {
	allReady.Store(true)
	overallStatus.Set(1)
	slog.Info("No dependencies specified, marking ready immediately.")
	readyOnce.Do(func() { close(readyChan) })
}

// startDependencyCheck launches a single dependency check goroutine
func startDependencyCheck(
	ctx context.Context,
	dep *Dependency,
	wg *sync.WaitGroup,
	retryInterval time.Duration,
	httpClient *http.Client,
	checkOverallReady func(),
) {
	defer wg.Done()
	slog.Info("Starting check", "dependency", dep.Raw)
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	// Perform initial check
	err := performDependencyCheck(ctx, dep, httpClient, retryInterval)
	isInitiallyReady := err == nil

	// Update status based on initial check
	if !isInitiallyReady {
		dep.Metric.Set(0)
	}
	handleDependencyStateChange(dep, isInitiallyReady, err, checkOverallReady)

	// Subsequent checks
	if !isInitiallyReady {
		monitorDependency(ctx, dep, ticker, httpClient, retryInterval, checkOverallReady)
	} else {
		waitForCancellation(ctx, dep, checkOverallReady)
	}
}

// startDependencyChecks launches goroutines for each dependency check.
func startDependencyChecks(ctx context.Context, cfg *Config, wg *sync.WaitGroup, readyChan chan struct{}, readyOnce *sync.Once, allReady *atomic.Bool, httpClient *http.Client) {
	if len(cfg.WaitDeps) == 0 {
		// No dependencies, mark as ready immediately and signal
		handleNoDependencies(allReady, readyOnce, readyChan)
		return
	}

	checkOverallReady := createOverallReadyChecker(cfg.WaitDeps, allReady, readyOnce, readyChan)

	// Launch a goroutine for each dependency
	for i := range cfg.WaitDeps {
		wg.Add(1)
		go startDependencyCheck(
			ctx,
			&cfg.WaitDeps[i],
			wg,
			cfg.RetryInterval,
			httpClient,
			checkOverallReady,
		)
	}
}

// monitorDependency handles periodic dependency checks
func monitorDependency(ctx context.Context, dep *Dependency, ticker *time.Ticker, httpClient *http.Client, retryInterval time.Duration, checkOverallReady func()) {
	for {
		select {
		case <-ctx.Done():
			handleContextCancellation(dep, checkOverallReady, ctx.Err())
			return
		case <-ticker.C:
			err := performDependencyCheck(ctx, dep, httpClient, retryInterval)
			handleDependencyStateChange(dep, err == nil, err, checkOverallReady)
		}
	}
}

// waitForCancellation waits for context cancellation for initially ready dependencies
func waitForCancellation(ctx context.Context, dep *Dependency, checkOverallReady func()) {
	<-ctx.Done()
	slog.Warn("Stopping check (was initially ready) due to context cancellation",
		"dependency", dep.Raw, "reason", ctx.Err())
	if dep.isReady.Load() {
		dep.isReady.Store(false)
		dep.Metric.Set(0)
		checkOverallReady()
	}
}

// checkAllDependenciesReady checks the atomic flags for all dependencies.
func checkAllDependenciesReady(deps []Dependency) bool {
	if len(deps) == 0 {
		return true
	}
	for i := range deps {
		if !deps[i].isReady.Load() {
			return false
		}
	}
	return true
}

// executeApplication handles the logic for maybe waiting, starting, and managing the child process.
func executeApplication(ctx context.Context, cfg *Config, sigChan chan os.Signal, readyChan chan struct{}, wg *sync.WaitGroup) (exitCode int) {
	exitCode = 0 // Default success
	exitChan := make(chan error, 1)
	var cmd *exec.Cmd

	// --- Prepare Command ---
	if len(cfg.Cmd) == 0 {
		slog.Warn("No command specified.")
		handleNoCommand(ctx, cfg, sigChan, readyChan, wg) // Handle idling
		return 0                                          // Exit cleanly if signaled while idling
	}
	cmdPath, err := exec.LookPath(cfg.Cmd[0])
	if err != nil {
		slog.Error("Failed to find command executable", "command", cfg.Cmd[0], "error", err)
		return 127 // Standard exit code for command not found
	}
	cmd = exec.Command(cmdPath, cfg.Cmd[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// --- Wait or Start Immediately ---
	// childStarted := false // REMOVED: declared and not used
	if !cfg.StartImmediately {
		slog.Info("Waiting for dependencies before starting command...")
		select {
		case <-readyChan:
			slog.Info("Dependencies ready. Starting command.")
			// Proceed to start below
		case <-ctx.Done():
			slog.Error("Timeout reached while waiting for dependencies.", "error", ctx.Err())
			exitCode = 1
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				exitCode = 124
			}
			return exitCode // Exit before starting child
		case sig := <-sigChan:
			slog.Info("Received signal while waiting for dependencies. Exiting.", "signal", sig)
			return 0 // Exit cleanly on signal before starting child
		}
	}

	// --- Start Child Process ---
	slog.Info("Starting child command", "command", strings.Join(cfg.Cmd, " "))
	err = cmd.Start()
	if err != nil {
		slog.Error("Failed to start command", "command", cmdPath, "error", err)
		return 126 // Standard exit code for command invoked cannot execute
	}
	// childStarted = true // REMOVED: declared and not used
	slog.Info("Child process started", "pid", cmd.Process.Pid)
	go func() { exitChan <- cmd.Wait() }() // Wait for exit in background

	// --- Handle Signals and Wait for Child Exit ---
	exitCode = handleSignalsAndWait(sigChan, exitChan, cmd)
	return exitCode
}

// handleNoCommand handles the case where no CMD is provided.
func handleNoCommand(ctx context.Context, cfg *Config, sigChan chan os.Signal, readyChan chan struct{}, wg *sync.WaitGroup) {
	if !cfg.StartImmediately && len(cfg.WaitDeps) > 0 {
		slog.Info("Waiting for dependencies to be ready before idling...")
		select {
		case <-readyChan:
			slog.Info("Dependencies ready. Idling until terminated.")
		case <-ctx.Done():
			slog.Error("Timeout reached while waiting for dependencies (no command specified). Exiting.", "error", ctx.Err())
			// exitCode := 1 // REMOVED: declared and not used
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				// exitCode = 124 // REMOVED: declared and not used
			}
			return // Let main handle final exit
		case sig := <-sigChan:
			slog.Info("Received signal while waiting for dependencies (no command specified). Exiting.", "signal", sig)
			return // Let main handle final exit
		}
	} else {
		slog.Info("Idling until terminated (no command specified, start immediately or no deps).")
	}
	// Wait indefinitely for signal if no command
	sig := <-sigChan // Wait for termination signal
	slog.Info("Received termination signal while idling. Exiting.", "signal", sig)
}

// handleSignalsAndWait manages signal forwarding and waits for the child process to exit.
func handleSignalsAndWait(sigChan chan os.Signal, exitChan chan error, cmd *exec.Cmd) (exitCode int) {
	exitCode = 0
	keepRunning := true
	for keepRunning {
		select {
		case sig := <-sigChan:
			slog.Info("Received signal", "signal", sig)
			if cmd.Process != nil {
				slog.Info("Forwarding signal to child process group", "signal", sig, "pgid", cmd.Process.Pid)
				errSig := syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
				if errSig != nil {
					slog.Error("Failed to forward signal to child process group", "error", errSig)
					if errFallback := cmd.Process.Signal(sig); errFallback != nil {
						slog.Error("Failed to forward signal to child process", "error", errFallback)
					}
				}
			} else {
				slog.Warn("Received signal but child process is not running.")
				keepRunning = false
			}

		case err := <-exitChan:
			slog.Info("Child process exited.")
			if err != nil {
				if exitError, ok := err.(*exec.ExitError); ok {
					exitCode = exitError.ExitCode()
				} else {
					slog.Error("Error waiting for child process", "error", err)
					exitCode = 1
				}
			} else {
				exitCode = 0
			}
			keepRunning = false
		}
	}
	return exitCode
}

// --- Specific Check Functions ---

func checkTCP(ctx context.Context, dep Dependency, _ *http.Client) error {
	slog.Debug("Starting TCP dependency check", "target", dep.Target)

	// First check if we can resolve the hostname
	host, port, err := net.SplitHostPort(dep.Target)
	if err != nil {
		slog.Debug("Failed to split host:port", "target", dep.Target, "error", err)
		return fmt.Errorf("invalid host:port format - %w", err)
	}
	slog.Debug("Parsed target", "host", host, "port", port)

	// Manual check for Docker DNS - must be performed explicitly
	// Create a custom context with a short timeout just for DNS
	dnsCtx, dnsCancel := context.WithTimeout(ctx, 2*time.Second)
	defer dnsCancel()

	// Use a DNS resolver that explicitly uses the context
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, network, address)
		},
	}

	// Check if the host is an IP address
	if net.ParseIP(host) == nil {
		// Not an IP address, we MUST resolve it successfully
		slog.Debug("Host is not an IP, attempting DNS resolution", "host", host)
		addrs, err := resolver.LookupHost(dnsCtx, host)
		if err != nil {
			slog.Debug("DNS resolution failed", "host", host, "error", err)
			return fmt.Errorf("DNS resolution failed: %w", err)
		}

		if len(addrs) == 0 {
			slog.Debug("DNS resolution returned no addresses", "host", host)
			return fmt.Errorf("DNS resolution returned no addresses for host: %s", host)
		}

		slog.Debug("DNS resolution succeeded", "host", host, "addresses", addrs)
	} else {
		slog.Debug("Host is an IP address, skipping DNS resolution", "host", host)
	}

	// Now we'll make a TCP connection with a specific protocol-based check
	// according to the port/service
	slog.Debug("Attempting TCP connection", "target", dep.Target)

	// Use an explicit short timeout for the connection attempt
	connCtx, connCancel := context.WithTimeout(ctx, 3*time.Second)
	defer connCancel()

	dialer := net.Dialer{
		KeepAlive: -1, // Disable keep-alive to avoid false positives
	}

	conn, err := dialer.DialContext(connCtx, "tcp", dep.Target)
	if err != nil {
		slog.Debug("TCP connection failed", "target", dep.Target, "error", err)
		return fmt.Errorf("connection failed: %w", err)
	}
	defer conn.Close()

	// Verify the connection is actually to the address we expect
	localAddr := conn.LocalAddr().String()
	remoteAddr := conn.RemoteAddr().String()
	slog.Debug("TCP connection established", "target", dep.Target,
		"local", localAddr, "remote", remoteAddr)

	// Actually try sending a protocol-appropriate probe
	// For port 80/443, send an HTTP request
	portNum, _ := strconv.Atoi(port)

	if portNum == 80 || portNum == 443 {
		// HTTP probe for web servers
		slog.Debug("Port 80/443 detected, sending HTTP probe", "port", port)
		_, err = conn.Write([]byte("HEAD / HTTP/1.0\r\n\r\n"))
	} else {
		// Generic probe
		slog.Debug("Sending generic probe", "port", port)
		_, err = conn.Write([]byte("PING\r\n"))
	}

	if err != nil {
		slog.Debug("Failed to write probe to connection", "target", dep.Target, "error", err)
		return fmt.Errorf("connection write failed: %w", err)
	}

	// Now try to read a response - we don't require a valid response,
	// but at least some data or a graceful close
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	// Try to read a response
	buffer := make([]byte, 64) // Bigger buffer to catch protocol responses
	n, err := conn.Read(buffer)

	if err != nil {
		if errors.Is(err, io.EOF) {
			// Server closed connection after receiving our probe
			// This is acceptable behavior
			slog.Debug("Connection closed by server after probe (EOF)", "target", dep.Target)
		} else if os.IsTimeout(err) {
			// In Docker environment, we need to be super strict about timeouts
			// if we already confirmed DNS resolution
			slog.Debug("Connection read timed out - marking as failure", "target", dep.Target)
			return fmt.Errorf("connection read timed out: %w", err)
		} else {
			// Other network errors are considered failures
			slog.Debug("Connection read failed", "target", dep.Target, "error", err)
			return fmt.Errorf("connection read failed: %w", err)
		}
	} else {
		responseData := string(buffer[:n])
		slog.Debug("Received response from server", "target", dep.Target,
			"bytes", n, "data", responseData)

		// If we see a 302 Found redirect response with empty location,
		// this is likely Docker network handling a non-existent container
		if strings.Contains(responseData, "302 Found") &&
			strings.Contains(responseData, "location: https:///") {
			slog.Debug("Detected Docker network placeholder response - rejecting", "target", dep.Target)
			return fmt.Errorf("connection responded with Docker network placeholder")
		}
	}

	slog.Debug("TCP dependency check completed successfully", "target", dep.Target)
	return nil
}

func checkUDP(ctx context.Context, dep Dependency, _ *http.Client) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", dep.Target)
	if err != nil {
		return err
	}
	return conn.Close()
}

func checkHTTP(ctx context.Context, dep Dependency, client *http.Client) error {
	slog.Debug("Starting HTTP dependency check", "target", dep.Target, "type", dep.Type)

	if client == nil {
		// Create a custom client that doesn't follow redirects
		client = &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // Don't follow redirects
			},
		}
	}

	// Ensure we have a properly formatted URL
	reqUrl := dep.Target
	if !strings.HasPrefix(reqUrl, "http://") && !strings.HasPrefix(reqUrl, "https://") {
		if dep.Type == "http" || dep.Type == "https" {
			reqUrl = fmt.Sprintf("%s://%s", dep.Type, dep.Target)
			slog.Debug("Prefixed URL with scheme", "original", dep.Target, "result", reqUrl)
		} else {
			return fmt.Errorf("invalid dependency type '%s' for http check on target '%s'", dep.Type, dep.Target)
		}
	}

	// Explicitly enforce HTTP if specified as the dependency type
	if dep.Type == "http" && strings.HasPrefix(reqUrl, "https://") {
		reqUrl = "http://" + strings.TrimPrefix(reqUrl, "https://")
		slog.Debug("Forced HTTP scheme", "url", reqUrl)
	}

	// Parse and validate the URL
	parsedURL, err := url.ParseRequestURI(reqUrl)
	if err != nil {
		slog.Debug("Invalid URL format", "url", reqUrl, "error", err)
		return fmt.Errorf("invalid URL format: %s, error: %w", reqUrl, err)
	}

	slog.Debug("Making HTTP request", "url", parsedURL.String(), "method", http.MethodGet)

	// Create a request with specific headers to prevent HTTPS upgrades
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Important: Set explicit connection timeout in case DNS resolve succeeds but connect hangs
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			slog.Debug("HTTP request timed out", "url", parsedURL.String())
			return fmt.Errorf("request timed out: %w", err)
		}
		if urlErr, ok := err.(*url.Error); ok {
			slog.Debug("HTTP URL error", "url", parsedURL.String(), "error", urlErr)
			return fmt.Errorf("request url error: %w", urlErr)
		}
		slog.Debug("HTTP request failed", "url", parsedURL.String(), "error", err)
		return fmt.Errorf("request failed: %w", err)
	}

	defer resp.Body.Close()

	// Read and discard response body
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024)) // Read max 1K for debugging
	if err != nil {
		slog.Warn("Error reading response body", "url", parsedURL.String(), "error", err)
	}

	slog.Debug("Received HTTP response",
		"url", parsedURL.String(),
		"status", resp.Status,
		"code", resp.StatusCode,
		"bodyPreview", string(body))

	// Check for successful status code (2xx)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	slog.Debug("HTTP dependency check successful", "url", parsedURL.String())
	return nil
}

func checkExec(ctx context.Context, dep Dependency, _ *http.Client) error {
	cmdCtx, cancel := context.WithTimeout(ctx, defaultCustomCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, dep.Target, dep.Args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("custom check script timed out after %v", defaultCustomCheckTimeout)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("script exited with non-zero status (%d)", exitErr.ExitCode())
		}
		return fmt.Errorf("script execution failed: %w", err)
	}
	return nil
}
