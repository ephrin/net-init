package main

import (
	"context"
	"crypto/tls" // Ensure TLS is imported if used, seems it was missing here too based on test error
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
	// defaultCustomCheckTimeout = 10 * time.Second // Changed below
)

// Changed from const to var to allow modification during testing
var defaultCustomCheckTimeout = 10 * time.Second

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
	// Apply the parsed custom timeout if it was set via env var (or keep the default var value)
	// Note: parseConfig gets the env var, but checkExec uses the global var.
	// This logic assumes the global var IS the source of truth unless overridden by env var.
	// Let's ensure parseConfig updates the global var if the env var is set.
	// We'll add this logic inside parseConfig.

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
	// Ensure crypto/tls is imported at the top
	httpClient := &http.Client{
		Timeout: cfg.RetryInterval / 2, // Timeout per check attempt < retry interval
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: cfg.TlsSkipVerify,
				// TODO: Load CA cert from cfg.TlsCACertPath if provided
			},
			// Add other transport settings if needed, like MaxIdleConnsPerHost
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

			// Perform initial check immediately
			func() {
				checkCtx, checkCancel := context.WithTimeout(ctx, cfg.RetryInterval)
				defer checkCancel()
				err := dep.CheckFunc(checkCtx, *dep, httpClient)
				if err == nil {
					if !dep.isReady.Load() {
						slog.Info("Dependency ready", "dependency", dep.Raw)
						dep.isReady.Store(true)
						dep.Metric.Set(1)
					}
				} else {
					slog.Debug("Dependency not ready (initial check)", "dependency", dep.Raw, "error", err)
					dep.Metric.Set(0) // Ensure metric is 0 initially if check fails
				}
				// Update overall status after initial check
				if checkAllDependenciesReady(cfg.WaitDeps) {
					if !allReady.Load() {
						slog.Info("All dependencies are now ready!")
						allReady.Store(true)
						overallStatus.Set(1)
					}
				} else {
					allReady.Store(false) // Ensure it's false if not all ready
					overallStatus.Set(0)
				}
			}()


			// Subsequent checks via ticker
			for {
				select {
				case <-ctx.Done(): // Overall timeout or signal received
					slog.Warn("Stopping check due to context cancellation", "dependency", dep.Raw, "reason", ctx.Err())
					// Ensure metric reflects final state (likely down if context cancelled)
					if dep.isReady.Load() {
						// If it was ready, but context cancelled, maybe mark as down? Or leave as last known state?
						// Let's mark as down for safety.
						dep.isReady.Store(false)
						dep.Metric.Set(0)
					}
					return
				case <-ticker.C:
					// Ticker logic for subsequent checks
					checkCtx, checkCancel := context.WithTimeout(ctx, cfg.RetryInterval)

					err := dep.CheckFunc(checkCtx, *dep, httpClient) // Pass value copy to check func
					checkCancel() // Release check context

					if err == nil {
						if !dep.isReady.Load() { // Status changed to Ready
							slog.Info("Dependency ready", "dependency", dep.Raw)
							dep.isReady.Store(true)
							dep.Metric.Set(1)
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
			}
		}(&cfg.WaitDeps[i]) // Pass pointer to goroutine
	}

	// --- Start Health Check Server ---
	mux := http.NewServeMux() // Use a mux for clarity
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

	go func() {
		addr := fmt.Sprintf(":%d", cfg.HealthCheckPort)
		slog.Info("Starting health check server", "address", addr, "health_path", cfg.HealthCheckPath, "metrics_path", cfg.MetricsPath)
		server := &http.Server{Addr: addr, Handler: mux}
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Health check server failed", "error", err)
		}
		// Consider graceful shutdown if needed later
	}()


	// --- Execute Main Application Command ---
	// Give checks a brief moment to run at least once before exec? Optional.
	// time.Sleep(50 * time.Millisecond)

	slog.Info("Executing main command", "command", strings.Join(cfg.Cmd, " "))
	if len(cfg.Cmd) == 0 {
		// If no command and deps exist, wait indefinitely for signal?
		if len(cfg.WaitDeps) > 0 {
			slog.Warn("No command specified. Net-init will run checks and serve health endpoints until terminated.")
			<-ctx.Done() // Wait for timeout or signal
			slog.Info("Net-init exiting.")
			os.Exit(0)
		} else {
			slog.Error("No command provided and no dependencies to check.")
			os.Exit(1)
		}
	}


	cmdPath, err := exec.LookPath(cfg.Cmd[0])
	if err != nil {
		slog.Error("Failed to find command executable", "command", cfg.Cmd[0], "error", err)
		os.Exit(1) // Use a distinct exit code? e.g., 127
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
		os.Exit(126) // Standard exit code for command invoked cannot execute
	}

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
	// Allow empty string to mean default without error
	case "":
		return defaultLogLevel, nil
	default:
		// Return default but also signal an error for invalid non-empty strings
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
		CustomCheckTimeout: defaultCustomCheckTimeout, // Initialize from the (now var) default
	}

	// Env Vars
	if portStr := os.Getenv("NETINIT_HEALTHCHECK_PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			if port > 0 && port <= 65535 {
				cfg.HealthCheckPort = port
			} else {
				return nil, fmt.Errorf("invalid NETINIT_HEALTHCHECK_PORT value: %d", port)
			}
		} else {
			return nil, fmt.Errorf("invalid NETINIT_HEALTHCHECK_PORT format: %w", err)
		}
	}
	if path := os.Getenv("NETINIT_HEALTHCHECK_PATH"); path != "" {
		// Basic validation: should start with /
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
	if timeoutStr := os.Getenv("NETINIT_TIMEOUT"); timeoutStr != "" {
		if timeout, err := strconv.Atoi(timeoutStr); err == nil {
			if timeout >= 0 { // Allow 0 timeout? Maybe minimum 1? Let's allow 0 for now.
				cfg.Timeout = time.Duration(timeout) * time.Second
			} else {
				return nil, fmt.Errorf("invalid NETINIT_TIMEOUT: must be non-negative")
			}
		} else {
			return nil, fmt.Errorf("invalid NETINIT_TIMEOUT format: %w", err)
		}
	}
	if intervalStr := os.Getenv("NETINIT_RETRY_INTERVAL"); intervalStr != "" {
		if interval, err := strconv.Atoi(intervalStr); err == nil {
			if interval > 0 {
				cfg.RetryInterval = time.Duration(interval) * time.Second
			} else {
				return nil, fmt.Errorf("invalid NETINIT_RETRY_INTERVAL: must be positive")
			}
		} else {
			return nil, fmt.Errorf("invalid NETINIT_RETRY_INTERVAL format: %w", err)
		}
	}
	// Update CustomCheckTimeout from Env Var *and* update the global variable
	// so checkExec uses the correct value without needing it passed explicitly.
	if customTimeoutStr := os.Getenv("NETINIT_CUSTOM_CHECK_TIMEOUT"); customTimeoutStr != "" {
		if customTimeout, err := strconv.Atoi(customTimeoutStr); err == nil {
			if customTimeout > 0 {
				cfg.CustomCheckTimeout = time.Duration(customTimeout) * time.Second
				defaultCustomCheckTimeout = cfg.CustomCheckTimeout // Update global var
			} else {
				return nil, fmt.Errorf("invalid NETINIT_CUSTOM_CHECK_TIMEOUT: must be positive")
			}
		} else {
			return nil, fmt.Errorf("invalid NETINIT_CUSTOM_CHECK_TIMEOUT format: %w", err)
		}
	} else {
        // Ensure cfg reflects the default if env var isn't set
        cfg.CustomCheckTimeout = defaultCustomCheckTimeout
    }


	// Parse LogLevel after other vars in case of error messages needing logging
	logLevelStr := os.Getenv("NETINIT_LOG_LEVEL")
	parsedLevel, err := parseLogLevel(logLevelStr)
	if err != nil {
		// Log the error using the initial default logger before updating level
		slog.Warn("Invalid NETINIT_LOG_LEVEL specified, using default", "value", logLevelStr, "default", defaultLogLevel.String())
		// We don't return error here, just use default level
		cfg.LogLevel = defaultLogLevel
	} else {
		cfg.LogLevel = parsedLevel
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
		uniqueDeps := make(map[string]struct{}) // Track unique dependencies

		for _, depRaw := range depStrings {
			depRaw = strings.TrimSpace(depRaw)
			if depRaw == "" {
				continue
			}

			// Prevent duplicate dependency definitions
			if _, exists := uniqueDeps[depRaw]; exists {
				slog.Warn("Duplicate dependency specified, ignoring.", "dependency", depRaw)
				continue
			}
			uniqueDeps[depRaw] = struct{}{}


			dep := Dependency{Raw: depRaw}
			parts := strings.SplitN(depRaw, "://", 2)

			if len(parts) == 1 { // Default to TCP
				// Basic validation for host:port format
				if !strings.Contains(parts[0], ":") {
					return nil, fmt.Errorf("invalid default TCP dependency format (missing port?): '%s'", depRaw)
				}
				dep.Type = "tcp"
				dep.Target = parts[0]
			} else {
				dep.Type = strings.ToLower(parts[0])
				dep.Target = parts[1]
				if dep.Target == "" {
					return nil, fmt.Errorf("missing target for dependency type '%s' in '%s'", dep.Type, depRaw)
				}
			}


			switch dep.Type {
			case "tcp":
				// Add validation for tcp target format if needed
				if !strings.Contains(dep.Target, ":") {
					return nil, fmt.Errorf("invalid TCP dependency format (missing port?): '%s'", depRaw)
				}
				dep.CheckFunc = checkTCP
			case "udp":
				// Add validation for udp target format if needed
				if !strings.Contains(dep.Target, ":") {
					return nil, fmt.Errorf("invalid UDP dependency format (missing port?): '%s'", depRaw)
				}
				dep.CheckFunc = checkUDP
			case "http", "https":
				// Basic validation: target shouldn't be empty
				dep.CheckFunc = checkHTTP
			case "exec":
				cmdParts := strings.Fields(dep.Target) // Basic splitting
				if len(cmdParts) == 0 {
					return nil, fmt.Errorf("invalid exec dependency format (empty command): '%s'", depRaw)
				}
				dep.Target = cmdParts[0] // The command/script path
				dep.Args = cmdParts[1:]   // The arguments
				dep.CheckFunc = checkExec
			default:
				return nil, fmt.Errorf("unsupported dependency type '%s' in '%s'", dep.Type, depRaw)
			}
			// Initialize metric for this dependency
			dep.Metric = depStatus.WithLabelValues(dep.Raw)
			// Don't set initial metric value here, let the first check do it.

			cfg.WaitDeps = append(cfg.WaitDeps, dep)
		}
	}

	// Remaining args are the command
	cfg.Cmd = args


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
	// Use context for dial timeout
	conn, err := dialer.DialContext(ctx, "tcp", dep.Target)
	if err != nil {
		return err
	}
	// Setting deadline might be useful if we were doing read/write checks
	// conn.SetDeadline(time.Now().Add(1 * time.Second)) // Example
	return conn.Close()
}

func checkUDP(ctx context.Context, dep Dependency, _ *http.Client) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", dep.Target)
	if err != nil {
		return err
	}
	// As noted before, UDP dial success is not a strong indicator.
	// A write *could* be attempted, but might block or succeed falsely.
	// _, err = conn.Write([]byte("ping")) // Example write attempt
	// if err != nil {
	// 	 conn.Close()
	// 	 return fmt.Errorf("udp write failed: %w", err)
	// }
	return conn.Close()
}

func checkHTTP(ctx context.Context, dep Dependency, client *http.Client) error {
	// Ensure client is not nil
	if client == nil {
		// This shouldn't happen based on current main() logic, but defensive check
		client = http.DefaultClient
	}

	reqUrl := dep.Target
	// Prepend scheme if missing, based on dep.Type
	if !strings.HasPrefix(reqUrl, "http://") && !strings.HasPrefix(reqUrl, "https://") {
		// Validate dep.Type is http or https before prepending
		if dep.Type == "http" || dep.Type == "https" {
			reqUrl = fmt.Sprintf("%s://%s", dep.Type, dep.Target)
		} else {
			// Should not happen if parseConfig is correct, but handle defensively
			return fmt.Errorf("invalid dependency type '%s' for http check on target '%s'", dep.Type, dep.Target)
		}
	}

	// Validate URL before creating request
	_, err := url.ParseRequestURI(reqUrl)
	if err != nil {
		return fmt.Errorf("invalid URL format: %s, error: %w", reqUrl, err)
	}


	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqUrl, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	// Add a user-agent?
	// req.Header.Set("User-Agent", "net-init/1.0")

	resp, err := client.Do(req)
	if err != nil {
		// Check if the error is a context deadline exceeded (timeout)
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("request timed out: %w", err)
		}
		// Check for other URL errors
		if urlErr, ok := err.(*url.Error); ok {
			return fmt.Errorf("request url error: %w", urlErr)
		}
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Drain body to allow connection reuse and handle potential errors during read
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		// Log this error, but the status code check is primary for readiness
		slog.Warn("Error reading response body", "url", reqUrl, "error", err)
	}


	if resp.StatusCode < 200 || resp.StatusCode >= 300 { // Check for 2xx success
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

func checkExec(ctx context.Context, dep Dependency, _ *http.Client) error {
	// Use the globally set (potentially overridden by env var) custom timeout
	cmdCtx, cancel := context.WithTimeout(ctx, defaultCustomCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, dep.Target, dep.Args...)
	// Capture output for better error messages? Could be large.
	// var stderr bytes.Buffer
	// cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard // Keep stderr discarded unless debugging needed


	err := cmd.Run() // Runs command and waits for completion or context cancelation

	// Check context error first - differentiates timeout from script failure
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("custom check script timed out after %v", defaultCustomCheckTimeout)
	}

	// If context didn't expire, check command execution error
	if err != nil {
		// Check if it's an ExitError to provide more context
		if exitErr, ok := err.(*exec.ExitError); ok {
			// stderrString := stderr.String() // Get captured stderr
			// if stderrString != "" {
			// 	 return fmt.Errorf("script exited with non-zero status (%d): %s", exitErr.ExitCode(), stderrString)
			// }
			return fmt.Errorf("script exited with non-zero status (%d)", exitErr.ExitCode())
		}
		// Other errors (e.g., command not found, permission denied)
		return fmt.Errorf("script execution failed: %w", err)
	}

	// Exit code 0 means success
	return nil
}
