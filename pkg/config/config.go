package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the full application configuration
type Config struct {
	WaitDeps         []string      // Raw dependency strings
	HealthCheckPort  int           // Port for health check HTTP server
	HealthCheckPath  string        // Path for health endpoint
	MetricsPath      string        // Path for Prometheus metrics endpoint
	Timeout          time.Duration // Overall timeout for the application
	RetryInterval    time.Duration // Interval between dependency checks
	LogLevel         slog.Level    // Logging level
	Cmd              []string      // Command to execute once dependencies are ready
	TlsSkipVerify    bool          // Skip TLS verification
	TlsCACertPath    string        // Path to CA certificate for TLS
	StartImmediately bool          // Start command without waiting for dependencies
	ExitAfterReady   bool          // Exit with success code after dependencies are ready (when no command provided)
}

// Default values
const (
	DefaultHealthCheckPort = 8887
	DefaultHealthCheckPath = "/health"
	DefaultMetricsPath     = "/metrics"
	DefaultTimeout         = 300 * time.Second
	DefaultRetryInterval   = 5 * time.Second
	DefaultLogLevel        = slog.LevelInfo
)

// Can be modified for tests
var DefaultCustomCheckTimeout = 10 * time.Second

// Parse creates a new Config from environment variables and command line arguments
func Parse(args []string) (*Config, error) {
	var err error
	cfg := &Config{
		HealthCheckPort:  DefaultHealthCheckPort,
		HealthCheckPath:  DefaultHealthCheckPath,
		MetricsPath:      DefaultMetricsPath,
		Timeout:          DefaultTimeout,
		RetryInterval:    DefaultRetryInterval,
		LogLevel:         DefaultLogLevel,
		TlsSkipVerify:    false,
		StartImmediately: false,
	}

	// Parse basic types
	cfg.HealthCheckPort, err = parseIntEnv("NETINIT_HEALTHCHECK_PORT", DefaultHealthCheckPort, false, false)
	if err != nil {
		return nil, err
	}
	cfg.Timeout, err = parseDurationEnv("NETINIT_TIMEOUT", DefaultTimeout, false)
	if err != nil {
		return nil, err
	}
	cfg.RetryInterval, err = parseDurationEnv("NETINIT_RETRY_INTERVAL", DefaultRetryInterval, true)
	if err != nil {
		return nil, err
	}
	customTimeoutSec, err := parseIntEnv("NETINIT_CUSTOM_CHECK_TIMEOUT", int(DefaultCustomCheckTimeout.Seconds()), true, false)
	if err != nil {
		return nil, err
	}
	DefaultCustomCheckTimeout = time.Duration(customTimeoutSec) * time.Second

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
	parsedLevel, errLog := ParseLogLevel(logLevelStr)
	if errLog != nil {
		slog.Warn("Invalid NETINIT_LOG_LEVEL specified, using default", "value", logLevelStr, "default", DefaultLogLevel.String())
	}
	cfg.LogLevel = parsedLevel

	// Parse booleans
	cfg.TlsSkipVerify = parseBoolEnv("NETINIT_TLS_SKIP_VERIFY", false)
	cfg.StartImmediately = parseBoolEnv("NETINIT_START_IMMEDIATELY", false)
	cfg.ExitAfterReady = parseBoolEnv("NETINIT_EXIT_AFTER_READY", false)

	// Parse other strings
	cfg.TlsCACertPath = os.Getenv("NETINIT_TLS_CA_CERT_PATH")

	// Parse Dependencies
	waitStr := os.Getenv("NETINIT_WAIT")
	if waitStr != "" {
		cfg.WaitDeps = ParseWaitDependencies(waitStr)
	}

	cfg.Cmd = args
	return cfg, nil
}

// ParseWaitDependencies parses the NETINIT_WAIT string into a string slice
func ParseWaitDependencies(waitStr string) []string {
	depStrings := strings.Split(waitStr, ",")
	result := make([]string, 0, len(depStrings))
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
		result = append(result, depRaw)
	}
	return result
}

// ParseLogLevel parses the log level string.
func ParseLogLevel(levelStr string) (slog.Level, error) {
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
		return DefaultLogLevel, nil // Default for empty
	default:
		return DefaultLogLevel, fmt.Errorf("unknown log level: %s", levelStr)
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

// SetupLogging configures the global logger based on environment variables.
func SetupLogging(level slog.Level) {
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	logger := slog.New(jsonHandler)
	slog.SetDefault(logger)
}
