package main

import (
	"context"
	"crypto/tls" // <-- Ensure this import is present
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec" // Keep the import
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- Test Helper Functions ---

// createDummyScript creates a simple shell script for testing exec checks.
// Returns the path to the script and a cleanup function.
func createDummyScript(t *testing.T, name string, content string) (string, func()) {
	t.Helper()
	dir := t.TempDir() // Creates a temporary directory, cleaned up automatically
	scriptPath := filepath.Join(dir, name)
	// Ensure script content starts with shebang for non-Windows
	if runtime.GOOS != "windows" && !strings.HasPrefix(content, "#!") {
		content = "#!/bin/sh\n" + content
	}
	// On Windows, maybe use .bat or .cmd extension and different content?
	// For simplicity, we skip exec tests on Windows for now.

	err := os.WriteFile(scriptPath, []byte(content), 0755) // Make executable
	if err != nil {
		t.Fatalf("Failed to create dummy script %s: %v", name, err)
	}
	// No explicit cleanup needed for file due to t.TempDir()
	return scriptPath, func() {} // Return empty cleanup as TempDir handles it
}

// --- Tests ---

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name        string
		levelStr    string
		expectedLvl slog.Level
		expectError bool
	}{
		{"DebugLower", "debug", slog.LevelDebug, false},
		{"InfoUpper", "INFO", slog.LevelInfo, false},
		{"WarnMixed", "WaRn", slog.LevelWarn, false},
		{"ErrorExact", "error", slog.LevelError, false},
		{"DefaultEmpty", "", slog.LevelInfo, false},         // Test that empty string uses default without error
		{"DefaultInvalid", "invalid", slog.LevelInfo, true}, // Invalid should return default level but signal error
		{"Number", "1", slog.LevelInfo, true},
	}

	// No need to save/restore defaultLogLevel as it's a const

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lvl, err := parseLogLevel(tt.levelStr)
			if (err != nil) != tt.expectError {
				t.Errorf("parseLogLevel(%q) error = %v, expectError %v", tt.levelStr, err, tt.expectError)
				return
			}
			if !tt.expectError && lvl != tt.expectedLvl {
				t.Errorf("parseLogLevel(%q) level = %v, want %v", tt.levelStr, lvl, tt.expectedLvl)
			}
			if tt.levelStr == "" {
				if err != nil {
					t.Errorf("parseLogLevel(\"\") expected no error, got %v", err)
				}
				if lvl != defaultLogLevel {
					t.Errorf("parseLogLevel(\"\") expected default level %v, got %v", defaultLogLevel, lvl)
				}
			}
			if tt.name == "DefaultInvalid" && err != nil && lvl != defaultLogLevel {
				t.Errorf("parseLogLevel(\"invalid\") expected default level %v on error, got %v", defaultLogLevel, lvl)
			}
		})
	}
}

// testDefaultConfig verifies the default configuration values
func testDefaultConfig(t *testing.T, cfg *Config, baseArgs []string, originalTimeoutVar time.Duration) {
	t.Helper()

	// Basic config checks
	if cfg.HealthCheckPort != defaultHealthCheckPort {
		t.Errorf("Default HealthCheckPort: got %d, want %d", cfg.HealthCheckPort, defaultHealthCheckPort)
	}
	if cfg.HealthCheckPath != defaultHealthCheckPath {
		t.Errorf("Default HealthCheckPath: got %s, want %s", cfg.HealthCheckPath, defaultHealthCheckPath)
	}
	if cfg.MetricsPath != defaultMetricsPath {
		t.Errorf("Default MetricsPath: got %s, want %s", cfg.MetricsPath, defaultMetricsPath)
	}
	if cfg.Timeout != defaultTimeout {
		t.Errorf("Default Timeout: got %v, want %v", cfg.Timeout, defaultTimeout)
	}
	if cfg.RetryInterval != defaultRetryInterval {
		t.Errorf("Default RetryInterval: got %v, want %v", cfg.RetryInterval, defaultRetryInterval)
	}
	if cfg.LogLevel != defaultLogLevel {
		t.Errorf("Default LogLevel: got %v, want %v", cfg.LogLevel, defaultLogLevel)
	}
	if defaultCustomCheckTimeout != originalTimeoutVar {
		t.Errorf("Default CustomCheckTimeout (global var): got %v, want %v", defaultCustomCheckTimeout, originalTimeoutVar)
	}
	if cfg.StartImmediately != false {
		t.Errorf("Default StartImmediately: got %t, want %t", cfg.StartImmediately, false)
	}
	if !reflect.DeepEqual(cfg.Cmd, baseArgs) {
		t.Errorf("Default Cmd: got %v, want %v", cfg.Cmd, baseArgs)
	}
	if len(cfg.WaitDeps) != 0 {
		t.Errorf("Default WaitDeps: got %d items, want 0", len(cfg.WaitDeps))
	}
}

// testOverrideValues verifies that environment variables correctly override defaults
func testOverrideValues(t *testing.T, cfg *Config, expectedTimeout time.Duration) {
	t.Helper()

	if cfg.HealthCheckPort != 9090 {
		t.Errorf("Port override failed")
	}
	if cfg.HealthCheckPath != "/ready" {
		t.Errorf("HealthPath override failed")
	}
	if cfg.MetricsPath != "/prom" {
		t.Errorf("MetricsPath override failed")
	}
	if cfg.Timeout != 60*time.Second {
		t.Errorf("Timeout override failed")
	}
	if cfg.RetryInterval != 2*time.Second {
		t.Errorf("RetryInterval override failed")
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel override failed")
	}
	if !cfg.TlsSkipVerify {
		t.Errorf("TlsSkipVerify override failed")
	}
	if defaultCustomCheckTimeout != expectedTimeout {
		t.Errorf("Global defaultCustomCheckTimeout not updated correctly: got %v, want %v",
			defaultCustomCheckTimeout, expectedTimeout)
	}
	if cfg.StartImmediately != true {
		t.Errorf("StartImmediately override failed: got %t, want %t", cfg.StartImmediately, true)
	}
}

// testDependencyValues verifies the proper parsing of dependencies
func testDependencyValues(t *testing.T, deps []Dependency, expectedTypes []string, expectedTargets []string, expectedArgsLen []int) {
	t.Helper()

	if len(deps) != len(expectedTypes) {
		t.Fatalf("WaitDeps count wrong: got %d, want %d", len(deps), len(expectedTypes))
	}

	for i, dep := range deps {
		if dep.Type != expectedTypes[i] {
			t.Errorf("Dep %d Type: got %s, want %s", i, dep.Type, expectedTypes[i])
		}
		if dep.Target != expectedTargets[i] {
			t.Errorf("Dep %d Target: got %s, want %s", i, dep.Target, expectedTargets[i])
		}
		if len(dep.Args) != expectedArgsLen[i] {
			t.Errorf("Dep %d ArgsLen: got %d, want %d", i, len(dep.Args), expectedArgsLen[i])
		}
		if dep.CheckFunc == nil {
			t.Errorf("Dep %d CheckFunc is nil", i)
		}
	}
}

// testValidOverrideDependencies verifies the dependency parsing in valid overrides
func testValidOverrideDependencies(t *testing.T, cfg *Config) {
	t.Helper()

	if len(cfg.WaitDeps) != 2 {
		t.Fatalf("WaitDeps count wrong: got %d, want 2", len(cfg.WaitDeps))
	}
	if cfg.WaitDeps[0].Raw != "tcp://db:1234" {
		t.Errorf("WaitDep[0] wrong: %s", cfg.WaitDeps[0].Raw)
	}
	if cfg.WaitDeps[1].Raw != "https://api.com/status" {
		t.Errorf("WaitDep[1] wrong: %s", cfg.WaitDeps[1].Raw)
	}
}

// testExplicitStartValues verifies different settings for START_IMMEDIATELY
func testExplicitStartValues(t *testing.T, setenv func(t *testing.T, key, value string), baseArgs []string) {
	t.Helper()

	// Test explicit false
	t.Run("ExplicitStartFalse", func(t *testing.T) {
		setenv(t, "NETINIT_START_IMMEDIATELY", "false")
		cfgFalse, errFalse := parseConfig(baseArgs)
		if errFalse != nil {
			t.Fatalf("parseConfig() with StartImmediately=false failed: %v", errFalse)
		}
		if cfgFalse.StartImmediately != false {
			t.Errorf("StartImmediately=false override failed: got %t, want %t", cfgFalse.StartImmediately, false)
		}
	})

	// Test invalid value (should default to false)
	t.Run("InvalidStartValue", func(t *testing.T) {
		setenv(t, "NETINIT_START_IMMEDIATELY", "yes")
		cfgInvalid, errInvalid := parseConfig(baseArgs)
		if errInvalid != nil {
			t.Fatalf("parseConfig() with StartImmediately=yes failed: %v", errInvalid)
		}
		if cfgInvalid.StartImmediately != false {
			t.Errorf("StartImmediately=yes override failed (should default to false): got %t, want %t",
				cfgInvalid.StartImmediately, false)
		}
	})
}

// runInvalidValueTest runs a test for an invalid config value
func runInvalidValueTest(t *testing.T, tc struct{ key, val string }, setenv func(t *testing.T, key, value string), baseArgs []string, originalTimeout time.Duration) {
	t.Helper()

	// Reset environment and set one invalid value
	setenv(t, tc.key, tc.val)

	// Reset global var before testing custom timeout
	if tc.key == "NETINIT_CUSTOM_CHECK_TIMEOUT" {
		defaultCustomCheckTimeout = originalTimeout
	}

	// Parse with the invalid value
	_, err := parseConfig(baseArgs)

	// Special case for log level which uses default on error
	if err == nil {
		if tc.key == "NETINIT_LOG_LEVEL" {
			t.Logf("Ignoring expected nil error for invalid NETINIT_LOG_LEVEL (uses default)")
		} else {
			t.Errorf("Expected error for %s=%s, but got nil", tc.key, tc.val)
		}
	} else {
		t.Logf("Got expected error for %s=%s: %v", tc.key, tc.val, err)
	}
}

// TestParseConfig verifies config parsing from environment variables
func TestParseConfig(t *testing.T) {
	// Helper to set env vars and cleanup
	setenv := func(t *testing.T, key, value string) {
		t.Helper()
		t.Setenv(key, value)
	}

	baseArgs := []string{"my", "app", "command"} // Mock command args

	// Save original default value and restore it after test suite
	originalCustomCheckTimeoutVar := defaultCustomCheckTimeout
	t.Cleanup(func() { defaultCustomCheckTimeout = originalCustomCheckTimeoutVar })

	// Test default configuration
	t.Run("Defaults", func(t *testing.T) {
		// Reset global var to its original default for this test run
		defaultCustomCheckTimeout = originalCustomCheckTimeoutVar

		cfg, err := parseConfig(baseArgs)
		if err != nil {
			t.Fatalf("parseConfig() with defaults failed: %v", err)
		}

		testDefaultConfig(t, cfg, baseArgs, originalCustomCheckTimeoutVar)
	})

	// Test valid configuration overrides
	t.Run("ValidOverrides", func(t *testing.T) {
		// Setup all environment variables
		setenv(t, "NETINIT_HEALTHCHECK_PORT", "9090")
		setenv(t, "NETINIT_HEALTHCHECK_PATH", "/ready")
		setenv(t, "NETINIT_METRICS_PATH", "/prom")
		setenv(t, "NETINIT_TIMEOUT", "60")
		setenv(t, "NETINIT_RETRY_INTERVAL", "2")
		setenv(t, "NETINIT_LOG_LEVEL", "debug")
		setenv(t, "NETINIT_TLS_SKIP_VERIFY", "true")
		setenv(t, "NETINIT_CUSTOM_CHECK_TIMEOUT", "15")
		setenv(t, "NETINIT_START_IMMEDIATELY", "true")
		setenv(t, "NETINIT_WAIT", "tcp://db:1234,https://api.com/status")

		// Reset global var before parsing
		defaultCustomCheckTimeout = originalCustomCheckTimeoutVar

		cfg, err := parseConfig(baseArgs)
		if err != nil {
			t.Fatalf("parseConfig() with valid overrides failed: %v", err)
		}

		// Test general overrides
		expectedCustomTimeout := 15 * time.Second
		testOverrideValues(t, cfg, expectedCustomTimeout)

		// Test dependency parsing
		testValidOverrideDependencies(t, cfg)

		// Test StartImmediately variations
		testExplicitStartValues(t, setenv, baseArgs)
	})

	// Test invalid configuration values
	t.Run("InvalidValues", func(t *testing.T) {
		testCases := []struct{ key, val string }{
			{"NETINIT_HEALTHCHECK_PORT", "abc"},
			{"NETINIT_HEALTHCHECK_PORT", "0"},
			{"NETINIT_HEALTHCHECK_PORT", "65536"},
			{"NETINIT_HEALTHCHECK_PATH", "no-slash"},
			{"NETINIT_METRICS_PATH", "no-slash-either"},
			{"NETINIT_TIMEOUT", "-10x"},
			{"NETINIT_TIMEOUT", "-1"},
			{"NETINIT_RETRY_INTERVAL", "foo"},
			{"NETINIT_RETRY_INTERVAL", "0"},
			{"NETINIT_RETRY_INTERVAL", "-1"},
			{"NETINIT_CUSTOM_CHECK_TIMEOUT", "-5"},
			{"NETINIT_LOG_LEVEL", "trace"},
			{"NETINIT_WAIT", "invalid://foo:bar"},
			{"NETINIT_WAIT", "exec://"},
			{"NETINIT_WAIT", "tcp://db"},
			{"NETINIT_WAIT", "db"},
			{"NETINIT_WAIT", "http://"},
		}

		for _, tc := range testCases {
			t.Run(fmt.Sprintf("Invalid_%s=%s", tc.key, tc.val), func(t *testing.T) {
				runInvalidValueTest(t, tc, setenv, baseArgs, originalCustomCheckTimeoutVar)
			})
		}
	})

	// Test dependency parsing
	t.Run("DependencyParsing", func(t *testing.T) {
		scriptPath, _ := createDummyScript(t, "test.sh", "exit 0")

		waitStr := fmt.Sprintf("db:5432,tcp://redis:6379,udp://stats:8125,http://api/h,https://sapi/s,exec://%s arg1, db:5432 ", scriptPath)
		setenv(t, "NETINIT_WAIT", waitStr)

		cfg, err := parseConfig(baseArgs)
		if err != nil {
			t.Fatalf("parseConfig() with dep string failed: %v", err)
		}

		expectedTypes := []string{"tcp", "tcp", "udp", "http", "https", "exec"}
		expectedTargets := []string{"db:5432", "redis:6379", "stats:8125", "api/h", "sapi/s", scriptPath}
		expectedArgsLen := []int{0, 0, 0, 0, 0, 1}

		testDependencyValues(t, cfg.WaitDeps, expectedTypes, expectedTargets, expectedArgsLen)

		// Check specific exec argument value
		if len(cfg.WaitDeps[5].Args) > 0 && cfg.WaitDeps[5].Args[0] != "arg1" {
			t.Errorf("Dep 5 Arg[0]: got %s, want arg1", cfg.WaitDeps[5].Args[0])
		}
	})

	// Test no command arguments
	t.Run("NoCommandArgs", func(t *testing.T) {
		setenv(t, "NETINIT_WAIT", "tcp://db:5432")
		cfg, err := parseConfig(nil) // No command args
		if err != nil {
			t.Fatalf("parseConfig() with no command args failed: %v", err)
		}
		if len(cfg.Cmd) != 0 {
			t.Errorf("Expected empty cfg.Cmd, got %v", cfg.Cmd)
		}
	})
}

func TestCheckTCP(t *testing.T) {
	// Create a TCP listener that simulates a server responding to protocol probes
	ln, err := net.Listen("tcp", "127.0.0.1:0") // Use port 0 for automatic assignment
	if err != nil {
		t.Fatalf("Failed to create TCP listener: %v", err)
	}
	serverAddr := ln.Addr().String()

	// Extract the port to determine appropriate protocol handling
	_, portStr, _ := net.SplitHostPort(serverAddr)
	port, _ := strconv.Atoi(portStr)
	isHTTPPort := port == 80 || port == 443

	// Accept connections and respond appropriately
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				// Listener was closed, exit goroutine
				return
			}

			// Read the probe
			buf := make([]byte, 64)
			n, _ := conn.Read(buf)

			// Send an appropriate response based on the detected protocol
			if n > 0 {
				if isHTTPPort || strings.HasPrefix(string(buf[:n]), "HEAD") {
					// HTTP-like response
					conn.Write([]byte("HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nOK"))
				} else {
					// Generic response for other protocols
					conn.Write([]byte("PONG\r\n"))
				}
			}
			conn.Close()
		}
	}()
	defer ln.Close()

	// Test with a working TCP server
	depOk := Dependency{Target: serverAddr, Type: "tcp"}
	ctxOk, cancelOk := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelOk()
	if err := checkTCP(ctxOk, depOk, nil); err != nil {
		t.Errorf("checkTCP failed for valid server (%s): %v", serverAddr, err)
	}

	// Test with an invalid port
	depFail := Dependency{Target: "localhost:1", Type: "tcp"}
	ctxFail, cancelFail := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelFail()
	if err := checkTCP(ctxFail, depFail, nil); err == nil {
		t.Errorf("checkTCP succeeded for invalid server (localhost:1), expected error")
	} else {
		t.Logf("Got expected error for checkTCP(localhost:1): %v", err)
	}

	// Test with non-existent hostname to verify DNS resolution check
	depDNSFail := Dependency{Target: "nonexistent-host-that-should-not-resolve.local:80", Type: "tcp"}
	ctxDNSFail, cancelDNSFail := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDNSFail()
	if err := checkTCP(ctxDNSFail, depDNSFail, nil); err == nil {
		t.Errorf("checkTCP succeeded for non-existent hostname, expected error")
	} else {
		t.Logf("Got expected error for DNS resolution failure: %v", err)
	}
}

func TestCheckUDP(t *testing.T) {
	depFail := Dependency{Target: "localhost:1", Type: "udp"}
	ctxFail, cancelFail := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelFail()
	err := checkUDP(ctxFail, depFail, nil)
	if err != nil {
		t.Logf("checkUDP for likely closed port returned (potentially expected) error: %v", err)
	} else {
		t.Logf("checkUDP for likely closed port returned no error (also potentially expected)")
	}
	depLoopback := Dependency{Target: "127.0.0.1:12345", Type: "udp"}
	ctxLoopback, cancelLoopback := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelLoopback()
	errLoopback := checkUDP(ctxLoopback, depLoopback, nil)
	if errLoopback != nil {
		t.Logf("checkUDP for loopback returned (potentially expected) error: %v", errLoopback)
	} else {
		t.Logf("checkUDP for loopback returned no error (also potentially expected)")
	}
}

func TestCheckHTTP(t *testing.T) {
	// Setup test servers
	serverOk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	}))
	defer serverOk.Close()

	serverRedirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com", http.StatusFound)
	}))
	defer serverRedirect.Close()

	serverFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer serverFail.Close()

	serverTimeout := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
	}))
	defer serverTimeout.Close()

	serverTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Secure OK")
	}))
	defer serverTLS.Close()

	// Setup clients
	clientTimeout := &http.Client{Timeout: 100 * time.Millisecond}
	clientTLS := serverTLS.Client()
	clientNoTrust := &http.Client{Timeout: 1 * time.Second}
	clientSkipVerify := &http.Client{
		Timeout:   1 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	// Test cases
	tests := []struct {
		name        string
		dep         Dependency
		client      *http.Client
		ctxTimeout  time.Duration
		expectError bool
		errorMsg    string
	}{
		{"HTTPOk", Dependency{Target: serverOk.URL, Type: "http"}, clientTLS, 1 * time.Second, false, ""},
		{"HTTPRedirect", Dependency{Target: serverRedirect.URL, Type: "http"}, nil, 1 * time.Second, true, "unexpected status code: 302"},
		{"HTTPFailStatus", Dependency{Target: serverFail.URL, Type: "http"}, clientTLS, 1 * time.Second, true, "unexpected status code: 500"},
		{"HTTPTimeout", Dependency{Target: serverTimeout.URL, Type: "http"}, clientTimeout, 1 * time.Second, true, "context deadline exceeded"},
		{"HTTPInvalidURL", Dependency{Target: "http://invalid host:", Type: "http"}, clientTLS, 1 * time.Second, true, "invalid URL format"},
		{"HTTPSOkTrusted", Dependency{Target: serverTLS.URL, Type: "https"}, clientTLS, 1 * time.Second, false, ""},
		{"HTTPSFailUntrusted", Dependency{Target: serverTLS.URL, Type: "https"}, clientNoTrust, 1 * time.Second, true, "certificate signed by unknown authority"},
		{"HTTPSOkSkipVerify", Dependency{Target: serverTLS.URL, Type: "https"}, clientSkipVerify, 1 * time.Second, false, ""},
		{"HTTPSOkTargetNoScheme", Dependency{Target: strings.TrimPrefix(serverTLS.URL, "https://"), Type: "https"}, clientTLS, 1 * time.Second, false, ""},
		{"HTTPTargetNoScheme", Dependency{Target: strings.TrimPrefix(serverOk.URL, "http://"), Type: "http"}, clientTLS, 1 * time.Second, false, ""},
		{"HTTPExplicitSchemeForce", Dependency{Target: "https://" + strings.TrimPrefix(serverOk.URL, "http://"), Type: "http"}, nil, 1 * time.Second, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tt.ctxTimeout)
			defer cancel()
			err := checkHTTP(ctx, tt.dep, tt.client)

			if tt.expectError && err == nil {
				t.Errorf("checkHTTP() expected error but got nil")
				return
			}
			if !tt.expectError && err != nil {
				t.Errorf("checkHTTP() unexpected error: %v", err)
				return
			}

			// Verify error message contains expected text if applicable
			if tt.expectError && err != nil && tt.errorMsg != "" {
				if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("Error '%v' does not contain expected text '%s'", err, tt.errorMsg)
				} else {
					t.Logf("Got expected error for %s: %v", tt.name, err)
				}
			}
		})
	}
}

func TestCheckExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		_ = exec.Command // Dummy usage to prevent unused import error when skipped
		t.Skip("Skipping exec tests on Windows due to shell script differences")
	}

	scriptOk, _ := createDummyScript(t, "ok.sh", "echo 'Success'\nexit 0")
	scriptFail, _ := createDummyScript(t, "fail.sh", "echo 'Failure' >&2\nexit 1")
	scriptTimeout, _ := createDummyScript(t, "timeout.sh", "sleep 2\nexit 0")

	tests := []struct {
		name          string
		dep           Dependency
		ctxTimeout    time.Duration
		customTimeout time.Duration
		expectError   bool
	}{
		{"ExecOk", Dependency{Target: scriptOk, Type: "exec"}, 2 * time.Second, 10 * time.Second, false},
		{"ExecFail", Dependency{Target: scriptFail, Type: "exec"}, 2 * time.Second, 10 * time.Second, true},
		{"ExecWithArgs", Dependency{Target: scriptOk, Args: []string{"arg1", "arg2"}, Type: "exec"}, 2 * time.Second, 10 * time.Second, false},
		{"ExecTimeout", Dependency{Target: scriptTimeout, Type: "exec"}, 3 * time.Second, 100 * time.Millisecond, true},
		{"ExecNotFound", Dependency{Target: "/no/such/script/exists", Type: "exec"}, 2 * time.Second, 10 * time.Second, true},
	}

	originalCustomTimeout := defaultCustomCheckTimeout
	t.Cleanup(func() { defaultCustomCheckTimeout = originalCustomTimeout })

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defaultCustomCheckTimeout = tt.customTimeout // Modify global var for test
			ctx, cancel := context.WithTimeout(context.Background(), tt.ctxTimeout)
			defer cancel()
			err := checkExec(ctx, tt.dep, nil)

			if (err != nil) != tt.expectError {
				t.Errorf("checkExec() error = %v, expectError %v", err, tt.expectError)
			}
			if tt.name == "ExecTimeout" && err != nil {
				if !strings.Contains(err.Error(), "custom check script timed out") && !strings.Contains(err.Error(), "context deadline exceeded") {
					t.Errorf("Expected timeout error ('custom check script timed out' or 'context deadline exceeded'), got: %v", err)
				} else {
					t.Logf("Got expected timeout error for ExecTimeout: %v", err)
				}
			}
			if tt.name == "ExecNotFound" && err != nil {
				if !strings.Contains(err.Error(), "no such file or directory") && !strings.Contains(err.Error(), "executable file not found") {
					t.Errorf("Expected 'not found' error, got: %v", err)
				} else {
					t.Logf("Got expected not found error for ExecNotFound: %v", err)
				}
			}
		})
	}
}

func TestCheckAllDependenciesReady(t *testing.T) {
	tests := []struct {
		name     string
		deps     []Dependency
		setup    func([]Dependency)
		expected bool
	}{
		{name: "NoDeps", deps: []Dependency{}, setup: func(d []Dependency) {}, expected: true},
		{name: "OneReady", deps: make([]Dependency, 1), setup: func(d []Dependency) { d[0].isReady.Store(true) }, expected: true},
		{name: "OneNotReady", deps: make([]Dependency, 1), setup: func(d []Dependency) { d[0].isReady.Store(false) }, expected: false},
		{name: "MultipleReady", deps: make([]Dependency, 3), setup: func(d []Dependency) { d[0].isReady.Store(true); d[1].isReady.Store(true); d[2].isReady.Store(true) }, expected: true},
		{name: "OneOfMultipleNotReady", deps: make([]Dependency, 3), setup: func(d []Dependency) { d[0].isReady.Store(true); d[1].isReady.Store(false); d[2].isReady.Store(true) }, expected: false},
		{name: "AllMultipleNotReady", deps: make([]Dependency, 3), setup: func(d []Dependency) { d[0].isReady.Store(false); d[1].isReady.Store(false); d[2].isReady.Store(false) }, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.deps == nil {
				tt.deps = []Dependency{}
			} else {
				for i := range tt.deps {
					tt.deps[i].isReady = atomic.Bool{}
				}
			}
			tt.setup(tt.deps)
			if got := checkAllDependenciesReady(tt.deps); got != tt.expected {
				t.Errorf("checkAllDependenciesReady() = %v, want %v", got, tt.expected)
			}
		})
	}
}
