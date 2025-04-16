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
	Timeout          time.Duration // Timeout for deps ready (default) or overall (immediate start)
	RetryInterval    time.Duration
	LogLevel         slog.Level
	Cmd              []string // Command to execute as child process
	TlsSkipVerify    bool
	TlsCACertPath    string
	StartImmediately bool // New flag: Start app immediately or wait for deps?
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
	// --- Initial Setup (Logging, Config Parsing) ---
	logLevel := defaultLogLevel
	if levelStr := os.Getenv("NETINIT_LOG_LEVEL"); levelStr != "" {
		var err error
		logLevel, err = parseLogLevel(levelStr)
		if err != nil {
			slog.Error("Invalid NETINIT_LOG_LEVEL, using default", "value", levelStr, "error", err)
			logLevel = defaultLogLevel
		}
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	logger := slog.New(jsonHandler)
	slog.SetDefault(logger)

	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		slog.Error("Configuration error", "error", err)
		os.Exit(1)
	}
	slog.Info("Net-Init starting", "config", fmt.Sprintf("Cmd=%v WaitDeps=%d StartImmediately=%t HealthPort=%d Timeout=%v Retry=%v LogLevel=%v", cfg.Cmd, len(cfg.WaitDeps), cfg.StartImmediately, cfg.HealthCheckPort, cfg.Timeout, cfg.RetryInterval, cfg.LogLevel))

	// --- Context and Signal Handling ---
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// --- Global Ready State & HTTP Client ---
	var allReady atomic.Bool
	allReady.Store(false)
	httpClient := &http.Client{
		Timeout: cfg.RetryInterval / 2,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ InsecureSkipVerify: cfg.TlsSkipVerify },
		},
	}


	// Channel to signal when dependencies are ready (only used in default mode)
	readyChan := make(chan struct{})
	var readyOnce sync.Once // Ensure readyChan is closed only once

	// --- Start Health Check Server ---
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
	httpServer := &http.Server{ Addr: fmt.Sprintf(":%d", cfg.HealthCheckPort), Handler: mux }
	go func() {
		slog.Info("Starting health check server", "address", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Health check server failed", "error", err)
		}
	}()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		slog.Info("Shutting down health check server...")
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("Health check server shutdown failed", "error", err)
		}
	}()


	// --- Start Dependency Checks Concurrently ---
	var wg sync.WaitGroup
	if len(cfg.WaitDeps) > 0 {
		for i := range cfg.WaitDeps {
			wg.Add(1)
			go func(dep *Dependency) {
				defer wg.Done()
				slog.Info("Starting check", "dependency", dep.Raw)
				ticker := time.NewTicker(cfg.RetryInterval)
				defer ticker.Stop()

				// Initial check needed before ticker loop starts
				isInitiallyReady := false
				func() {
					checkCtx, checkCancel := context.WithTimeout(ctx, cfg.RetryInterval)
					defer checkCancel()
					err := dep.CheckFunc(checkCtx, *dep, httpClient)
					if err == nil {
						if !dep.isReady.Load() { // Check if state changes
							slog.Info("Dependency ready", "dependency", dep.Raw)
							dep.isReady.Store(true)
							dep.Metric.Set(1)
						}
						isInitiallyReady = true // Mark ready for overall check below
					} else {
						slog.Debug("Dependency not ready (initial check)", "dependency", dep.Raw, "error", err)
						dep.Metric.Set(0) // Ensure metric is 0
						isInitiallyReady = false
					}
				}() // End initial check func


				checkOverallReady := func() {
					if checkAllDependenciesReady(cfg.WaitDeps) {
						if !allReady.Load() {
							slog.Info("All dependencies are now ready!")
							allReady.Store(true)
							overallStatus.Set(1)
							readyOnce.Do(func() { close(readyChan) })
						}
					} else {
						if allReady.Load() {
							slog.Warn("Overall status changed to NOT READY")
							allReady.Store(false)
							overallStatus.Set(0)
						}
					}
				}
                checkOverallReady() // Check after initial check


				// Subsequent checks only if not initially ready or if context not done
				if !isInitiallyReady {
					for {
						select {
						case <-ctx.Done():
							slog.Warn("Stopping check due to context cancellation", "dependency", dep.Raw, "reason", ctx.Err())
							if dep.isReady.Load() {
								dep.isReady.Store(false)
								dep.Metric.Set(0)
								checkOverallReady()
							}
							return
						case <-ticker.C:
							checkCtx, checkCancel := context.WithTimeout(ctx, cfg.RetryInterval)
							err := dep.CheckFunc(checkCtx, *dep, httpClient)
							checkCancel()

							stateChanged := false
							if err == nil {
								if !dep.isReady.Load() {
									slog.Info("Dependency ready", "dependency", dep.Raw)
									dep.isReady.Store(true)
									dep.Metric.Set(1)
									stateChanged = true
								}
							} else {
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
						}
					}
				} else {
                     <-ctx.Done()
                     slog.Warn("Stopping check (was initially ready) due to context cancellation", "dependency", dep.Raw, "reason", ctx.Err())
                     if dep.isReady.Load() {
                         dep.isReady.Store(false)
                         dep.Metric.Set(0)
                         checkOverallReady()
                     }
                }
			}(&cfg.WaitDeps[i])
		}
	} else {
		allReady.Store(true)
		overallStatus.Set(1)
		slog.Info("No dependencies specified, marking ready immediately.")
		readyOnce.Do(func() { close(readyChan) })
	}

	// --- Start Child Process (Conditional) ---
	var cmd *exec.Cmd
	var cmdPath string

	if len(cfg.Cmd) > 0 {
		var errLookPath error
		cmdPath, errLookPath = exec.LookPath(cfg.Cmd[0])
		if errLookPath != nil {
			slog.Error("Failed to find command executable", "command", cfg.Cmd[0], "error", errLookPath)
			os.Exit(127)
		}
		cmd = exec.Command(cmdPath, cfg.Cmd[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		slog.Warn("No command specified. Net-init will run checks and serve health endpoints.")
		if !cfg.StartImmediately && len(cfg.WaitDeps) > 0 {
			slog.Info("Waiting for dependencies to be ready before idling...")
			select {
			case <-readyChan:
				slog.Info("Dependencies ready. Idling until terminated.")
			case <-ctx.Done():
				slog.Error("Timeout reached while waiting for dependencies (no command specified).", "error", ctx.Err())
				exitCode := 1
				if errors.Is(ctx.Err(), context.DeadlineExceeded) { exitCode = 124 }
                cancel()
				wg.Wait()
				os.Exit(exitCode)
			case sig := <-sigChan:
				slog.Info("Received signal while waiting for dependencies (no command specified). Exiting.", "signal", sig)
				cancel()
				wg.Wait()
				os.Exit(0)
			}
		} else {
            slog.Info("Idling until terminated (no command specified, start immediately or no deps).")
        }
        <-sigChan
		slog.Info("Received termination signal, exiting.")
		cancel()
		wg.Wait()
		os.Exit(0)
	}


	// --- Main Execution Logic ---
	exitCode := 0
	exitChan := make(chan error, 1)

	if !cfg.StartImmediately {
		// --- Default: Wait for Dependencies First ---
		slog.Info("Waiting for dependencies before starting command...")
		select {
		case <-readyChan:
			slog.Info("Dependencies ready. Starting command.")
			err = cmd.Start()
			if err != nil {
				slog.Error("Failed to start command after dependencies ready", "command", cmdPath, "error", err)
				os.Exit(126)
			}
			slog.Info("Child process started", "pid", cmd.Process.Pid)
			go func() { exitChan <- cmd.Wait() }()

		case <-ctx.Done():
			slog.Error("Timeout reached while waiting for dependencies.", "error", ctx.Err())
			exitCode = 1
			if errors.Is(ctx.Err(), context.DeadlineExceeded) { exitCode = 124 }
            cancel()
            wg.Wait()
			os.Exit(exitCode)

		case sig := <-sigChan:
			slog.Info("Received signal while waiting for dependencies. Exiting.", "signal", sig)
            cancel()
			wg.Wait()
			os.Exit(0)
		}
	} else {
		// --- Optional: Start Immediately ---
		slog.Info("Starting command immediately.")
		err = cmd.Start()
		if err != nil {
			slog.Error("Failed to start command immediately", "command", cmdPath, "error", err)
			os.Exit(126)
		}
		slog.Info("Child process started", "pid", cmd.Process.Pid)
		go func() { exitChan <- cmd.Wait() }()
	}


	// --- Signal Handling / Wait Loop (runs only if child was started) ---
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

		case err = <-exitChan:
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


	slog.Info("Net-Init exiting", "exitCode", exitCode)
	cancel()
	wg.Wait()
	os.Exit(exitCode)
}


// --- Helper Functions ---

func parseLogLevel(levelStr string) (slog.Level, error) {
	switch strings.ToLower(levelStr) {
	case "debug": return slog.LevelDebug, nil
	case "info": return slog.LevelInfo, nil
	case "warn": return slog.LevelWarn, nil
	case "error": return slog.LevelError, nil
	case "": return defaultLogLevel, nil // Default for empty
	default: return defaultLogLevel, fmt.Errorf("unknown log level: %s", levelStr)
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
		StartImmediately: false, // Default is to wait
	}

	// --- Parse Env Vars ---
	if portStr := os.Getenv("NETINIT_HEALTHCHECK_PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			if port > 0 && port <= 65535 { cfg.HealthCheckPort = port
			} else { return nil, fmt.Errorf("invalid NETINIT_HEALTHCHECK_PORT value: %d", port) }
		} else { return nil, fmt.Errorf("invalid NETINIT_HEALTHCHECK_PORT format: %w", err) }
	}
	if path := os.Getenv("NETINIT_HEALTHCHECK_PATH"); path != "" {
		if !strings.HasPrefix(path, "/") { return nil, fmt.Errorf("invalid NETINIT_HEALTHCHECK_PATH: must start with /") }
		cfg.HealthCheckPath = path
	}
	if path := os.Getenv("NETINIT_METRICS_PATH"); path != "" {
		if !strings.HasPrefix(path, "/") { return nil, fmt.Errorf("invalid NETINIT_METRICS_PATH: must start with /") }
		cfg.MetricsPath = path
	}
	if timeoutStr := os.Getenv("NETINIT_TIMEOUT"); timeoutStr != "" {
		if timeout, err := strconv.Atoi(timeoutStr); err == nil {
			if timeout >= 0 { cfg.Timeout = time.Duration(timeout) * time.Second
			} else { return nil, fmt.Errorf("invalid NETINIT_TIMEOUT: must be non-negative") }
		} else { return nil, fmt.Errorf("invalid NETINIT_TIMEOUT format: %w", err) }
	}
	if intervalStr := os.Getenv("NETINIT_RETRY_INTERVAL"); intervalStr != "" {
		if interval, err := strconv.Atoi(intervalStr); err == nil {
			if interval > 0 { cfg.RetryInterval = time.Duration(interval) * time.Second
			} else { return nil, fmt.Errorf("invalid NETINIT_RETRY_INTERVAL: must be positive") }
		} else { return nil, fmt.Errorf("invalid NETINIT_RETRY_INTERVAL format: %w", err) }
	}
	if customTimeoutStr := os.Getenv("NETINIT_CUSTOM_CHECK_TIMEOUT"); customTimeoutStr != "" {
		if customTimeout, err := strconv.Atoi(customTimeoutStr); err == nil && customTimeout > 0 {
			defaultCustomCheckTimeout = time.Duration(customTimeout) * time.Second
		} else { return nil, fmt.Errorf("invalid NETINIT_CUSTOM_CHECK_TIMEOUT: %s", customTimeoutStr) }
	}
	logLevelStr := os.Getenv("NETINIT_LOG_LEVEL")
	parsedLevel, err := parseLogLevel(logLevelStr)
	if err != nil { slog.Warn("Invalid NETINIT_LOG_LEVEL", "value", logLevelStr, "error", err) }
	cfg.LogLevel = parsedLevel
	if skipVerifyStr := os.Getenv("NETINIT_TLS_SKIP_VERIFY"); skipVerifyStr == "true" {
		cfg.TlsSkipVerify = true
	}
	cfg.TlsCACertPath = os.Getenv("NETINIT_TLS_CA_CERT_PATH")
	if startImmediatelyStr := os.Getenv("NETINIT_START_IMMEDIATELY"); startImmediatelyStr == "true" {
		cfg.StartImmediately = true
	}

	// Parse Dependencies
	waitStr := os.Getenv("NETINIT_WAIT")
	if waitStr != "" {
		depStrings := strings.Split(waitStr, ",")
		cfg.WaitDeps = make([]Dependency, 0, len(depStrings))
		uniqueDeps := make(map[string]struct{})
		for _, depRaw := range depStrings {
			depRaw = strings.TrimSpace(depRaw)
			if depRaw == "" { continue }
			if _, exists := uniqueDeps[depRaw]; exists {
				slog.Warn("Duplicate dependency specified, ignoring.", "dependency", depRaw)
				continue
			}
			uniqueDeps[depRaw] = struct{}{}
			dep := Dependency{Raw: depRaw}
			parts := strings.SplitN(depRaw, "://", 2)
			if len(parts) == 1 {
				if !strings.Contains(parts[0], ":") { return nil, fmt.Errorf("invalid default TCP format: '%s'", depRaw)}
				dep.Type = "tcp"
				dep.Target = parts[0]
			} else {
				dep.Type = strings.ToLower(parts[0])
				dep.Target = parts[1]
				if dep.Target == "" { return nil, fmt.Errorf("missing target for type '%s' in '%s'", dep.Type, depRaw)}
			}
			switch dep.Type {
			case "tcp":
				if !strings.Contains(dep.Target, ":") { return nil, fmt.Errorf("invalid TCP format: '%s'", depRaw)}
				dep.CheckFunc = checkTCP
			case "udp":
				if !strings.Contains(dep.Target, ":") { return nil, fmt.Errorf("invalid UDP format: '%s'", depRaw)}
				dep.CheckFunc = checkUDP
			case "http", "https":
				dep.CheckFunc = checkHTTP
			case "exec":
				cmdParts := strings.Fields(dep.Target)
				if len(cmdParts) == 0 { return nil, fmt.Errorf("invalid exec format: '%s'", depRaw)}
				dep.Target = cmdParts[0]
				dep.Args = cmdParts[1:]
				dep.CheckFunc = checkExec
			default:
				return nil, fmt.Errorf("unsupported dependency type '%s' in '%s'", dep.Type, depRaw)
			}
			dep.Metric = depStatus.WithLabelValues(dep.Raw)
			cfg.WaitDeps = append(cfg.WaitDeps, dep)
		}
	}
	cfg.Cmd = args
	return cfg, nil
}


func checkAllDependenciesReady(deps []Dependency) bool {
	if len(deps) == 0 { return true }
	for i := range deps {
		if !deps[i].isReady.Load() { return false }
	}
	return true
}

// --- Specific Check Functions ---

func checkTCP(ctx context.Context, dep Dependency, _ *http.Client) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", dep.Target)
	if err != nil { return err }
	return conn.Close()
}

func checkUDP(ctx context.Context, dep Dependency, _ *http.Client) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", dep.Target)
	if err != nil { return err }
	return conn.Close()
}

func checkHTTP(ctx context.Context, dep Dependency, client *http.Client) error {
	if client == nil { client = http.DefaultClient }
	reqUrl := dep.Target
	if !strings.HasPrefix(reqUrl, "http://") && !strings.HasPrefix(reqUrl, "https://") {
		if dep.Type == "http" || dep.Type == "https" {
			reqUrl = fmt.Sprintf("%s://%s", dep.Type, dep.Target)
		} else {
			return fmt.Errorf("invalid dependency type '%s' for http check on target '%s'", dep.Type, dep.Target)
		}
	}
	if _, err := url.ParseRequestURI(reqUrl); err != nil {
		return fmt.Errorf("invalid URL format: %s, error: %w", reqUrl, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqUrl, nil)
	if err != nil { return fmt.Errorf("failed to create request: %w", err) }
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) { return fmt.Errorf("request timed out: %w", err) }
		if urlErr, ok := err.(*url.Error); ok { return fmt.Errorf("request url error: %w", urlErr) }
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if _, err = io.Copy(io.Discard, resp.Body); err != nil {
		slog.Warn("Error reading response body", "url", reqUrl, "error", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
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
