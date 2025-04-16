package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec" // Keep the import
	"path/filepath"
	"reflect"
	"runtime"
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
		{"DefaultEmpty", "", slog.LevelInfo, false}, // Test that empty string uses default without error
		{"DefaultInvalid", "invalid", slog.LevelInfo, true}, // Invalid should return default level but signal error
		{"Number", "1", slog.LevelInfo, true},
	}

	// Assuming parseLogLevel uses defaultLogLevel defined in main.go
	// originalDefaultLevel := defaultLogLevel // <-- REMOVED unused variable

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lvl, err := parseLogLevel(tt.levelStr)
			if (err != nil) != tt.expectError {
				t.Errorf("parseLogLevel(%q) error = %v, expectError %v", tt.levelStr, err, tt.expectError)
				return
			}
			// If an error is expected, the level returned might be the default, check if the error matches expectation.
			// If no error is expected, check the level.
			if !tt.expectError && lvl != tt.expectedLvl {
				t.Errorf("parseLogLevel(%q) level = %v, want %v", tt.levelStr, lvl, tt.expectedLvl)
			}
			// Special case check for empty string behavior
			if tt.levelStr == "" {
				if err != nil {
					t.Errorf("parseLogLevel(\"\") expected no error, got %v", err)
				}
				if lvl != defaultLogLevel {
					t.Errorf("parseLogLevel(\"\") expected default level %v, got %v", defaultLogLevel, lvl)
				}
			}
			// Special case for invalid string behavior
			if tt.name == "DefaultInvalid" && err != nil && lvl != defaultLogLevel {
				 t.Errorf("parseLogLevel(\"invalid\") expected default level %v on error, got %v", defaultLogLevel, lvl)
			}
		})
	}
}


func TestParseConfig(t *testing.T) {
	// Helper to set env vars and cleanup
	setenv := func(t *testing.T, key, value string) {
		t.Helper()
		// Store original value if exists, and restore it
		// Using t.Setenv is simpler as it handles cleanup automatically
		t.Setenv(key, value)
	}

	baseArgs := []string{"my", "app", "command"} // Mock command args

	// Save original default value and restore it after test suite
	originalCustomCheckTimeoutVar := defaultCustomCheckTimeout
	t.Cleanup(func() { defaultCustomCheckTimeout = originalCustomCheckTimeoutVar })


	t.Run("Defaults", func(t *testing.T) {
		// Ensure env vars are unset for default testing
		// t.Setenv clears automatically per test run with t.Run

		cfg, err := parseConfig(baseArgs)
		if err != nil {
			t.Fatalf("parseConfig() with defaults failed: %v", err)
		}

		// Assertions for default values
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
		// Check against the global variable which holds the default
		if cfg.CustomCheckTimeout != defaultCustomCheckTimeout {
			t.Errorf("Default CustomCheckTimeout: got %v, want %v", cfg.CustomCheckTimeout, defaultCustomCheckTimeout)
		}
		if !reflect.DeepEqual(cfg.Cmd, baseArgs) {
			t.Errorf("Default Cmd: got %v, want %v", cfg.Cmd, baseArgs)
		}
		if len(cfg.WaitDeps) != 0 {
			t.Errorf("Default WaitDeps: got %d items, want 0", len(cfg.WaitDeps))
		}
	})

	t.Run("ValidOverrides", func(t *testing.T) {
		setenv(t, "NETINIT_HEALTHCHECK_PORT", "9090")
		setenv(t, "NETINIT_HEALTHCHECK_PATH", "/ready")
		setenv(t, "NETINIT_METRICS_PATH", "/prom")
		setenv(t, "NETINIT_TIMEOUT", "60")
		setenv(t, "NETINIT_RETRY_INTERVAL", "2")
		setenv(t, "NETINIT_LOG_LEVEL", "debug")
		setenv(t, "NETINIT_TLS_SKIP_VERIFY", "true")
		setenv(t, "NETINIT_CUSTOM_CHECK_TIMEOUT", "15")
		setenv(t, "NETINIT_WAIT", "tcp://db:1234,https://api.com/status")

		// Reset global var before parsing, as it might be modified by other tests if run in parallel
		defaultCustomCheckTimeout = originalCustomCheckTimeoutVar

		cfg, err := parseConfig(baseArgs)
		if err != nil {
			t.Fatalf("parseConfig() with valid overrides failed: %v", err)
		}

		// Assertions for overridden values
		if cfg.HealthCheckPort != 9090 { t.Errorf("Port override failed") }
		if cfg.HealthCheckPath != "/ready" { t.Errorf("HealthPath override failed") }
		if cfg.MetricsPath != "/prom" { t.Errorf("MetricsPath override failed") }
		if cfg.Timeout != 60*time.Second { t.Errorf("Timeout override failed") }
		if cfg.RetryInterval != 2*time.Second { t.Errorf("RetryInterval override failed") }
		if cfg.LogLevel != slog.LevelDebug { t.Errorf("LogLevel override failed") }
		if !cfg.TlsSkipVerify { t.Errorf("TlsSkipVerify override failed") }
		if cfg.CustomCheckTimeout != 15*time.Second { t.Errorf("CustomCheckTimeout override failed: got %v", cfg.CustomCheckTimeout) }
		// Check if the global var was also updated by parseConfig
		if defaultCustomCheckTimeout != 15*time.Second { t.Errorf("Global defaultCustomCheckTimeout not updated by parseConfig: got %v", defaultCustomCheckTimeout) }

		if len(cfg.WaitDeps) != 2 { t.Fatalf("WaitDeps count wrong: got %d, want 2", len(cfg.WaitDeps)) }
		if cfg.WaitDeps[0].Raw != "tcp://db:1234" { t.Errorf("WaitDep[0] wrong: %s", cfg.WaitDeps[0].Raw) }
		if cfg.WaitDeps[1].Raw != "https://api.com/status" { t.Errorf("WaitDep[1] wrong: %s", cfg.WaitDeps[1].Raw) }
	})

	t.Run("InvalidValues", func(t *testing.T) {
		testCases := []struct{ key, val string }{
			{"NETINIT_HEALTHCHECK_PORT", "abc"},
			{"NETINIT_HEALTHCHECK_PORT", "0"}, // Port 0 might be valid for binding, but let's assume invalid config here
			{"NETINIT_HEALTHCHECK_PORT", "65536"},
			{"NETINIT_HEALTHCHECK_PATH", "no-slash"},
			{"NETINIT_METRICS_PATH", "no-slash-either"},
			{"NETINIT_TIMEOUT", "-10x"},
			{"NETINIT_TIMEOUT", "-1"}, // Negative timeout
			{"NETINIT_RETRY_INTERVAL", "foo"},
			{"NETINIT_RETRY_INTERVAL", "0"}, // Zero interval
			{"NETINIT_RETRY_INTERVAL", "-1"}, // Negative interval
			{"NETINIT_CUSTOM_CHECK_TIMEOUT", "-5"}, // Negative timeout
			{"NETINIT_LOG_LEVEL", "trace"}, // Invalid level string (should use default, no error returned by parseConfig)
			{"NETINIT_WAIT", "invalid://foo:bar"},
			{"NETINIT_WAIT", "exec://"}, // Empty exec command
			{"NETINIT_WAIT", "tcp://db"}, // Missing port
			{"NETINIT_WAIT", "db"}, // Missing port (default tcp)
			{"NETINIT_WAIT", "http://"}, // Missing target
		}
		for _, tc := range testCases {
			t.Run(fmt.Sprintf("Invalid_%s=%s", tc.key, tc.val), func(t *testing.T) {
				// Setenv is automatically cleaned up by t.Run
				setenv(t, tc.key, tc.val)
				_, err := parseConfig(baseArgs)
				if err == nil {
					// Special case: invalid log level should not cause parseConfig error
					if tc.key == "NETINIT_LOG_LEVEL" {
						t.Logf("Ignoring expected nil error for invalid NETINIT_LOG_LEVEL (uses default)")
					} else {
						t.Errorf("Expected error for %s=%s, but got nil", tc.key, tc.val)
					}
				} else {
					t.Logf("Got expected error for %s=%s: %v", tc.key, tc.val, err)
				}
			})
		}
	})

	t.Run("DependencyParsing", func(t *testing.T) {
		scriptPath, _ := createDummyScript(t, "test.sh", "exit 0") // Simple script

		waitStr := fmt.Sprintf("db:5432,tcp://redis:6379,udp://stats:8125,http://api/h,https://sapi/s,exec://%s arg1, db:5432 ", scriptPath) // Add duplicate and extra space
		setenv(t, "NETINIT_WAIT", waitStr)

		cfg, err := parseConfig(baseArgs)
		if err != nil {
			t.Fatalf("parseConfig() with dep string failed: %v", err)
		}

		// Expecting duplicate to be ignored
		expectedTypes := []string{"tcp", "tcp", "udp", "http", "https", "exec"}
		expectedTargets := []string{"db:5432", "redis:6379", "stats:8125", "api/h", "sapi/s", scriptPath}
		expectedArgsLen := []int{0, 0, 0, 0, 0, 1}

		if len(cfg.WaitDeps) != len(expectedTypes) {
			t.Fatalf("WaitDeps count wrong (duplicates not ignored?): got %d, want %d", len(cfg.WaitDeps), len(expectedTypes))
		}

		for i, dep := range cfg.WaitDeps {
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
		if len(cfg.WaitDeps[5].Args) > 0 && cfg.WaitDeps[5].Args[0] != "arg1" {
			 t.Errorf("Dep 5 Arg[0]: got %s, want arg1", cfg.WaitDeps[5].Args[0])
		}
	})

	t.Run("NoCommandArgs", func(t *testing.T){
		setenv(t, "NETINIT_WAIT", "tcp://db:5432") // Need a wait dep to trigger the warning path
		cfg, err := parseConfig(nil) // No command args
		if err != nil {
			t.Fatalf("parseConfig() with no command args failed: %v", err)
		}
		if len(cfg.Cmd) != 0 {
			t.Errorf("Expected empty cfg.Cmd, got %v", cfg.Cmd)
		}
		// We can't easily check the log output here without redirecting logs
	})
}

func TestCheckTCP(t *testing.T) {
	// Start a dummy TCP server using httptest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	}))
	defer server.Close()

	// Valid case
	// Extract host:port from server URL
	serverAddr := strings.TrimPrefix(server.URL, "http://")
	depOk := Dependency{Target: serverAddr, Type: "tcp"}
	ctxOk, cancelOk := context.WithTimeout(context.Background(), 2*time.Second) // Increased timeout slightly
	defer cancelOk()
	if err := checkTCP(ctxOk, depOk, nil); err != nil {
		t.Errorf("checkTCP failed for valid server (%s): %v", serverAddr, err)
	}

	// Invalid case (non-existent port)
	depFail := Dependency{Target: "localhost:1", Type: "tcp"} // Assume port 1 is not listening
	ctxFail, cancelFail := context.WithTimeout(context.Background(), 200*time.Millisecond) // Short timeout
	defer cancelFail()
	if err := checkTCP(ctxFail, depFail, nil); err == nil {
		t.Errorf("checkTCP succeeded for invalid server (localhost:1), expected error")
	} else {
		t.Logf("Got expected error for checkTCP(localhost:1): %v", err)
	}
}

// TestCheckUDP is basic - just checks dial, not actual communication
func TestCheckUDP(t *testing.T) {
	// Test dialing a likely closed port
	depFail := Dependency{Target: "localhost:1", Type: "udp"} // Assume port 1 is not listening
	ctxFail, cancelFail := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelFail()
	err := checkUDP(ctxFail, depFail, nil)
	// UDP dial might succeed even if nothing is listening, or fail (e.g., network unreachable)
	// This test primarily ensures the function runs without panic.
	if err != nil {
		t.Logf("checkUDP for likely closed port returned (potentially expected) error: %v", err)
	} else {
		t.Logf("checkUDP for likely closed port returned no error (also potentially expected)")
	}

	// Test dialing a likely valid but non-specific target (like loopback)
	// This might succeed more reliably than localhost:1 depending on OS config
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
	// OK Server
	serverOk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	}))
	defer serverOk.Close()

	// Fail Server
	serverFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "Error")
	}))
	defer serverFail.Close()

	// Timeout Server
	serverTimeout := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // Sleep longer than client timeout
		w.WriteHeader(http.StatusOK)
	}))
	defer serverTimeout.Close()

	// TLS Server
	serverTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Secure OK")
	}))
	defer serverTLS.Close()

	// Create clients
	// Client with short timeout
	clientTimeout := &http.Client{Timeout: 100 * time.Millisecond} // Adjusted timeout
	// Client that trusts the test TLS server
	clientTLS := serverTLS.Client()
	// Client that DOES NOT trust the test TLS server (default)
	clientNoTrust := &http.Client{Timeout: 1 * time.Second}
	// Client that skips verification
	clientSkipVerify := &http.Client{
		Timeout: 1*time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}


	tests := []struct {
		name        string
		dep         Dependency
		client      *http.Client
		ctxTimeout  time.Duration
		expectError bool
	}{
		{"HTTPOk", Dependency{Target: serverOk.URL, Type: "http"}, clientTLS, 1*time.Second, false},
		{"HTTPFailStatus", Dependency{Target: serverFail.URL, Type: "http"}, clientTLS, 1*time.Second, true},
		{"HTTPTimeout", Dependency{Target: serverTimeout.URL, Type: "http"}, clientTimeout, 1*time.Second, true}, // Client timeout < server sleep
		{"HTTPInvalidURL", Dependency{Target: "http://invalid host:", Type: "http"}, clientTLS, 1*time.Second, true}, // Invalid URL format
		{"HTTPSOkTrusted", Dependency{Target: serverTLS.URL, Type: "https"}, clientTLS, 1*time.Second, false},
		{"HTTPSFailUntrusted", Dependency{Target: serverTLS.URL, Type: "https"}, clientNoTrust, 1*time.Second, true},
		{"HTTPSOkSkipVerify", Dependency{Target: serverTLS.URL, Type: "https"}, clientSkipVerify, 1*time.Second, false},
		{"HTTPSOkTargetNoScheme", Dependency{Target: strings.TrimPrefix(serverTLS.URL, "https://"), Type: "https"}, clientTLS, 1*time.Second, false}, // Test scheme prepending
		{"HTTPTargetNoScheme", Dependency{Target: strings.TrimPrefix(serverOk.URL, "http://"), Type: "http"}, clientTLS, 1*time.Second, false}, // Test scheme prepending
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tt.ctxTimeout)
			defer cancel()
			err := checkHTTP(ctx, tt.dep, tt.client)
			if (err != nil) != tt.expectError {
				t.Errorf("checkHTTP() error = %v, expectError %v", err, tt.expectError)
			}
			// Specifically check for timeout error type if expected
			if tt.name == "HTTPTimeout" && err != nil {
				// Check if the error indicates a timeout (could be context deadline or url.Error timeout)
				if !strings.Contains(err.Error(), "request timed out") && !strings.Contains(err.Error(), "context deadline exceeded") {
					t.Errorf("Expected timeout error containing 'request timed out' or 'context deadline exceeded', got: %v", err)
				} else {
					t.Logf("Got expected timeout error for HTTPTimeout: %v", err)
				}
			}
		})
	}
}


func TestCheckExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Add dummy usage of exec package to prevent unused import error when skipped
		_ = exec.Command
		t.Skip("Skipping exec tests on Windows due to shell script differences")
	}

	// Success script
	scriptOk, _ := createDummyScript(t, "ok.sh", "echo 'Success'\nexit 0")
	// Fail script
	scriptFail, _ := createDummyScript(t, "fail.sh", "echo 'Failure' >&2\nexit 1")
	// Timeout script
	scriptTimeout, _ := createDummyScript(t, "timeout.sh", "sleep 2\nexit 0")


	tests := []struct {
		name        string
		dep         Dependency
		ctxTimeout  time.Duration // Timeout for the check function context
		customTimeout time.Duration // Timeout specifically for the command execution (will modify global var)
		expectError bool
	}{
		{"ExecOk", Dependency{Target: scriptOk, Type: "exec"}, 2*time.Second, 10*time.Second, false}, // Use a reasonable custom timeout
		{"ExecFail", Dependency{Target: scriptFail, Type: "exec"}, 2*time.Second, 10*time.Second, true},
		{"ExecWithArgs", Dependency{Target: scriptOk, Args: []string{"arg1", "arg2"}, Type: "exec"}, 2*time.Second, 10*time.Second, false},
		{"ExecTimeout", Dependency{Target: scriptTimeout, Type: "exec"}, 3*time.Second, 100 * time.Millisecond, true}, // customTimeout < sleep
		{"ExecNotFound", Dependency{Target: "/no/such/script/exists", Type: "exec"}, 2*time.Second, 10*time.Second, true},
		// {"ExecContextCancel", Dependency{Target: scriptTimeout, Type: "exec"}, 50*time.Millisecond, 10*time.Second, true}, // check func ctx < sleep (harder to guarantee which timeout hits first)
	}

	// Store original default and restore it
	originalCustomTimeout := defaultCustomCheckTimeout
	t.Cleanup(func() { defaultCustomCheckTimeout = originalCustomTimeout })


	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set the specific custom timeout for this test case by modifying the global var
			defaultCustomCheckTimeout = tt.customTimeout

			ctx, cancel := context.WithTimeout(context.Background(), tt.ctxTimeout)
			defer cancel()

			err := checkExec(ctx, tt.dep, nil) // Client not used for exec

			if (err != nil) != tt.expectError {
				t.Errorf("checkExec() error = %v, expectError %v", err, tt.expectError)
			}
			// Check for specific timeout error
			if tt.name == "ExecTimeout" && err != nil {
				 if !strings.Contains(err.Error(), "custom check script timed out") && !strings.Contains(err.Error(), "context deadline exceeded"){
					t.Errorf("Expected timeout error ('custom check script timed out' or 'context deadline exceeded'), got: %v", err)
				 } else {
					 t.Logf("Got expected timeout error for ExecTimeout: %v", err)
				 }
			}
			// Check for not found error
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
		setup    func([]Dependency) // Function to set readiness state
		expected bool
	}{
		{
			name: "NoDeps",
			deps: []Dependency{},
			setup: func(d []Dependency) {},
			expected: true,
		},
		{
			name: "OneReady",
			deps: make([]Dependency, 1),
			setup: func(d []Dependency) {
				d[0].isReady.Store(true)
			},
			expected: true,
		},
		{
			name: "OneNotReady",
			deps: make([]Dependency, 1),
			setup: func(d []Dependency) {
				d[0].isReady.Store(false)
			},
			expected: false,
		},
		{
			name: "MultipleReady",
			deps: make([]Dependency, 3),
			setup: func(d []Dependency) {
				d[0].isReady.Store(true)
				d[1].isReady.Store(true)
				d[2].isReady.Store(true)
			},
			expected: true,
		},
		{
			name: "OneOfMultipleNotReady",
			deps: make([]Dependency, 3),
			setup: func(d []Dependency) {
				d[0].isReady.Store(true)
				d[1].isReady.Store(false) // This one is not ready
				d[2].isReady.Store(true)
			},
			expected: false,
		},
		{
			name: "AllMultipleNotReady",
			deps: make([]Dependency, 3),
			setup: func(d []Dependency) {
				d[0].isReady.Store(false)
				d[1].isReady.Store(false)
				d[2].isReady.Store(false)
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Initialize deps if needed (make ensures non-nil slice)
			if tt.deps == nil {
				tt.deps = []Dependency{}
			} else {
				// Ensure atomic bools are initialized (though default is false)
				for i := range tt.deps {
					tt.deps[i].isReady = atomic.Bool{}
				}
			}

			// Apply setup
			tt.setup(tt.deps)

			// Perform check
			if got := checkAllDependenciesReady(tt.deps); got != tt.expected {
				t.Errorf("checkAllDependenciesReady() = %v, want %v", got, tt.expected)
			}
		})
	}
}


// Note: Testing the main loop's concurrency, timeout handling, and the final
// syscall.Exec call is complex within unit tests. These aspects are better
// verified through integration tests (e.g., running the built Docker image
// against mock services).

