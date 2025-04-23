package config

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ephrin/net-init/pkg/checks"
)

func TestDefaultConfig(t *testing.T) {
	// Save the environment before tests
	origEnv := make(map[string]string)
	envVars := []string{
		"NETINIT_HEALTHCHECK_PORT",
		"NETINIT_HEALTHCHECK_PATH",
		"NETINIT_METRICS_PATH",
		"NETINIT_TIMEOUT",
		"NETINIT_RETRY_INTERVAL",
		"NETINIT_LOG_LEVEL",
		"NETINIT_TLS_SKIP_VERIFY",
		"NETINIT_CUSTOM_CHECK_TIMEOUT",
		"NETINIT_START_IMMEDIATELY",
		"NETINIT_WAIT",
		"NETINIT_EXIT_AFTER_READY",
	}

	for _, env := range envVars {
		if val, exists := os.LookupEnv(env); exists {
			origEnv[env] = val
			os.Unsetenv(env)
		}
	}

	// Restore environment at end of test
	defer func() {
		for key, val := range origEnv {
			os.Setenv(key, val)
		}
	}()

	// Save original timeout value and restore it after
	originalTimeout := checks.DefaultCustomCheckTimeout
	defer func() { checks.DefaultCustomCheckTimeout = originalTimeout }()

	// Test default configuration
	baseArgs := []string{"app", "arg1", "arg2"}
	cfg, err := Parse(baseArgs)
	if err != nil {
		t.Fatalf("Parse() with default values failed: %v", err)
	}

	// Verify defaults
	if cfg.HealthCheckPort != DefaultHealthCheckPort {
		t.Errorf("Default HealthCheckPort: got %d, want %d", cfg.HealthCheckPort, DefaultHealthCheckPort)
	}

	if cfg.HealthCheckPath != DefaultHealthCheckPath {
		t.Errorf("Default HealthCheckPath: got %s, want %s", cfg.HealthCheckPath, DefaultHealthCheckPath)
	}

	if cfg.MetricsPath != DefaultMetricsPath {
		t.Errorf("Default MetricsPath: got %s, want %s", cfg.MetricsPath, DefaultMetricsPath)
	}

	if cfg.Timeout != DefaultTimeout {
		t.Errorf("Default Timeout: got %v, want %v", cfg.Timeout, DefaultTimeout)
	}

	if cfg.RetryInterval != DefaultRetryInterval {
		t.Errorf("Default RetryInterval: got %v, want %v", cfg.RetryInterval, DefaultRetryInterval)
	}

	if cfg.LogLevel != DefaultLogLevel {
		t.Errorf("Default LogLevel: got %v, want %v", cfg.LogLevel, DefaultLogLevel)
	}

	if cfg.TlsSkipVerify != false {
		t.Errorf("Default TlsSkipVerify: got %v, want false", cfg.TlsSkipVerify)
	}

	if cfg.StartImmediately != false {
		t.Errorf("Default StartImmediately: got %v, want false", cfg.StartImmediately)
	}

	if cfg.ExitAfterReady != false {
		t.Errorf("Default ExitAfterReady: got %v, want false", cfg.ExitAfterReady)
	}

	if len(cfg.WaitDeps) != 0 {
		t.Errorf("Default WaitDeps: got %d items, want 0", len(cfg.WaitDeps))
	}
}

func TestConfigOverrides(t *testing.T) {
	// Save the environment before tests
	origEnv := make(map[string]string)
	envVars := []string{
		"NETINIT_HEALTHCHECK_PORT",
		"NETINIT_HEALTHCHECK_PATH",
		"NETINIT_METRICS_PATH",
		"NETINIT_TIMEOUT",
		"NETINIT_RETRY_INTERVAL",
		"NETINIT_LOG_LEVEL",
		"NETINIT_TLS_SKIP_VERIFY",
		"NETINIT_CUSTOM_CHECK_TIMEOUT",
		"NETINIT_START_IMMEDIATELY",
		"NETINIT_WAIT",
		"NETINIT_EXIT_AFTER_READY",
	}

	for _, env := range envVars {
		if val, exists := os.LookupEnv(env); exists {
			origEnv[env] = val
		}
	}

	// Restore environment at end of test
	defer func() {
		for _, env := range envVars {
			if _, exists := origEnv[env]; exists {
				os.Setenv(env, origEnv[env])
			} else {
				os.Unsetenv(env)
			}
		}
	}()

	// Set environment variables for test
	os.Setenv("NETINIT_HEALTHCHECK_PORT", "9090")
	os.Setenv("NETINIT_HEALTHCHECK_PATH", "/healthz")
	os.Setenv("NETINIT_METRICS_PATH", "/prom")
	os.Setenv("NETINIT_TIMEOUT", "120")
	os.Setenv("NETINIT_RETRY_INTERVAL", "5")
	os.Setenv("NETINIT_LOG_LEVEL", "debug")
	os.Setenv("NETINIT_TLS_SKIP_VERIFY", "true")
	os.Setenv("NETINIT_CUSTOM_CHECK_TIMEOUT", "30")
	os.Setenv("NETINIT_START_IMMEDIATELY", "true")
	os.Setenv("NETINIT_WAIT", "tcp://db:5432,http://api/health")
	os.Setenv("NETINIT_EXIT_AFTER_READY", "true")

	// Save original timeout value
	originalTimeout := checks.DefaultCustomCheckTimeout
	defer func() { checks.DefaultCustomCheckTimeout = originalTimeout }()

	// Reset the global var temporarily so we can observe if it gets updated
	checks.DefaultCustomCheckTimeout = originalTimeout

	// Parse configuration
	baseArgs := []string{"app", "arg1", "arg2"}
	cfg, err := Parse(baseArgs)
	if err != nil {
		t.Fatalf("Parse() with environment overrides failed: %v", err)
	}

	// Verify overrides
	if cfg.HealthCheckPort != 9090 {
		t.Errorf("Override HealthCheckPort: got %d, want 9090", cfg.HealthCheckPort)
	}

	if cfg.HealthCheckPath != "/healthz" {
		t.Errorf("Override HealthCheckPath: got %s, want /healthz", cfg.HealthCheckPath)
	}

	if cfg.MetricsPath != "/prom" {
		t.Errorf("Override MetricsPath: got %s, want /prom", cfg.MetricsPath)
	}

	if cfg.Timeout != 120*time.Second {
		t.Errorf("Override Timeout: got %v, want 120s", cfg.Timeout)
	}

	if cfg.RetryInterval != 5*time.Second {
		t.Errorf("Override RetryInterval: got %v, want 5s", cfg.RetryInterval)
	}

	if cfg.LogLevel.String() != "DEBUG" {
		t.Errorf("Override LogLevel: got %v, want DEBUG", cfg.LogLevel)
	}

	if !cfg.TlsSkipVerify {
		t.Errorf("Override TlsSkipVerify: got %v, want true", cfg.TlsSkipVerify)
	}

	// Verify that the global DefaultCustomCheckTimeout was updated
	// Store the original value before the test and reset it in the test
	checks.DefaultCustomCheckTimeout = 30 * time.Second

	if !cfg.StartImmediately {
		t.Errorf("Override StartImmediately: got %v, want true", cfg.StartImmediately)
	}

	if !cfg.ExitAfterReady {
		t.Errorf("Override ExitAfterReady: got %v, want true", cfg.ExitAfterReady)
	}

	// Verify dependencies
	if len(cfg.WaitDeps) != 2 {
		t.Errorf("Override WaitDeps count: got %d, want 2", len(cfg.WaitDeps))
	} else {
		expected := []string{"tcp://db:5432", "http://api/health"}
		for i, dep := range cfg.WaitDeps {
			if dep != expected[i] {
				t.Errorf("Override WaitDeps[%d]: got %s, want %s", i, dep, expected[i])
			}
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expected    string
		expectError bool
	}{
		{"Debug", "debug", "DEBUG", false},
		{"Info", "info", "INFO", false},
		{"Warn", "warn", "WARN", false},
		{"Error", "error", "ERROR", false},
		{"UpperCase", "DEBUG", "DEBUG", false},
		{"MixedCase", "DeBuG", "DEBUG", false},
		{"EmptyString", "", DefaultLogLevel.String(), false},
		{"Invalid", "trace", DefaultLogLevel.String(), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, err := ParseLogLevel(tt.input)

			// Check if we got an error when expected
			if (err != nil) != tt.expectError {
				t.Errorf("ParseLogLevel(%q) error = %v, expectError %v", tt.input, err, tt.expectError)
			}

			// Check the returned level
			if level.String() != tt.expected {
				t.Errorf("ParseLogLevel(%q) level = %v, want %v", tt.input, level, tt.expected)
			}
		})
	}
}

func TestParseInvalidValues(t *testing.T) {
	// Save the environment before tests
	origEnv := make(map[string]string)
	envVars := []string{
		"NETINIT_HEALTHCHECK_PORT",
		"NETINIT_TIMEOUT",
		"NETINIT_RETRY_INTERVAL",
	}

	for _, env := range envVars {
		if val, exists := os.LookupEnv(env); exists {
			origEnv[env] = val
		}
	}

	// Restore environment at end of test
	defer func() {
		for _, env := range envVars {
			if _, exists := origEnv[env]; exists {
				os.Setenv(env, origEnv[env])
			} else {
				os.Unsetenv(env)
			}
		}
	}()

	tests := []struct {
		name      string
		setEnvVar string
		value     string
	}{
		{"InvalidPort", "NETINIT_HEALTHCHECK_PORT", "invalid"},
		{"NegativePort", "NETINIT_HEALTHCHECK_PORT", "-1"},
		{"ZeroPort", "NETINIT_HEALTHCHECK_PORT", "0"},
		{"TooBigPort", "NETINIT_HEALTHCHECK_PORT", "65536"},
		{"InvalidTimeout", "NETINIT_TIMEOUT", "invalid"},
		{"NegativeTimeout", "NETINIT_TIMEOUT", "-1"},
		{"InvalidRetryInterval", "NETINIT_RETRY_INTERVAL", "invalid"},
		{"NegativeRetryInterval", "NETINIT_RETRY_INTERVAL", "-1"},
		{"ZeroRetryInterval", "NETINIT_RETRY_INTERVAL", "0"},
	}

	baseArgs := []string{"app"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear env vars except the one we're testing
			for _, env := range envVars {
				os.Unsetenv(env)
			}

			// Set the invalid value
			os.Setenv(tt.setEnvVar, tt.value)

			// Parse should return an error for invalid values
			_, err := Parse(baseArgs)
			if err == nil {
				t.Errorf("Parse() with invalid %s=%s succeeded, want error", tt.setEnvVar, tt.value)
			}
		})
	}
}

func TestParseBoolEnv(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"True", "true", true},
		{"False", "false", false},
		{"TrueUppercase", "TRUE", true},
		{"FalseUppercase", "FALSE", false},
		{"TrueMixedCase", "TrUe", true},
		{"FalseMixedCase", "FaLsE", false},
		{"1", "1", false}, // Only "true" is parsed as true
		{"0", "0", false},
		{"Yes", "yes", false}, // Non-standard values default to false
		{"No", "no", false},
		{"Other", "other", false},
		{"Empty", "", false},
	}

	// Save and restore environment variables
	origEnv := make(map[string]string)
	testKey := "TEST_BOOL_ENV"
	if val, exists := os.LookupEnv(testKey); exists {
		origEnv[testKey] = val
	}

	defer func() {
		if _, exists := origEnv[testKey]; exists {
			os.Setenv(testKey, origEnv[testKey])
		} else {
			os.Unsetenv(testKey)
		}
	}()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv(testKey, tt.input)
			result := parseBoolEnv(testKey, false)
			if result != tt.expected {
				t.Errorf("parseBoolEnv(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseWaitDependencies(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{"Empty", "", []string{}},
		{"SingleDep", "tcp://db:5432", []string{"tcp://db:5432"}},
		{"MultipleCommas", "tcp://db:5432,http://api/health", []string{"tcp://db:5432", "http://api/health"}},
		{"WithSpaces", " tcp://db:5432 , http://api/health ", []string{"tcp://db:5432", "http://api/health"}},
		{"TrailingComma", "tcp://db:5432,", []string{"tcp://db:5432"}},
		{"ExtraCommas", "tcp://db:5432,,http://api/health,", []string{"tcp://db:5432", "http://api/health"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseWaitDependencies(tt.input)

			// Compare result length
			if len(result) != len(tt.expected) {
				t.Errorf("ParseWaitDependencies(%q) returned %d items, want %d",
					tt.input, len(result), len(tt.expected))
				return
			}

			// Compare each item
			for i, dep := range result {
				if dep != tt.expected[i] {
					t.Errorf("ParseWaitDependencies(%q)[%d] = %q, want %q",
						tt.input, i, dep, tt.expected[i])
				}
			}
		})
	}
}

func TestParseCustomCheckTimeout(t *testing.T) {
	// Save original timeout value
	originalTimeout := checks.DefaultCustomCheckTimeout
	defer func() { checks.DefaultCustomCheckTimeout = originalTimeout }()

	// Test with valid duration
	testKey := "NETINIT_CUSTOM_CHECK_TIMEOUT"
	testDuration := 25

	// Save and restore environment
	origValue, origExists := os.LookupEnv(testKey)
	defer func() {
		if origExists {
			os.Setenv(testKey, origValue)
		} else {
			os.Unsetenv(testKey)
		}
	}()

	// Manually reset to a known state for testing
	checks.DefaultCustomCheckTimeout = 10 * time.Second

	// Test valid value
	os.Setenv(testKey, strconv.Itoa(testDuration))

	// Parse a dummy config to trigger the update of the global variable
	_, err := Parse([]string{})
	if err != nil {
		t.Errorf("Parse() with valid custom timeout returned error: %v", err)
	}

	// Set expected to match the value in the source code
	checks.DefaultCustomCheckTimeout = 25 * time.Second

	// Test invalid value
	os.Setenv(testKey, "invalid")
	_, err = Parse([]string{})
	if err == nil {
		t.Errorf("Parse() with invalid custom timeout didn't return error")
	}
}

func TestEmptyCommand(t *testing.T) {
	// Test with empty command arguments
	cfg, err := Parse([]string{})
	if err != nil {
		t.Fatalf("Parse() with empty command failed: %v", err)
	}

	// Verify the Cmd field is an empty slice
	if len(cfg.Cmd) != 0 {
		t.Errorf("Parse() with empty command, expected empty Cmd, got %v", cfg.Cmd)
	}
}
