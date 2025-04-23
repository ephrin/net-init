package checkers

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// MockDependencyInfo is a test implementation of DependencyInfo
type MockDependencyInfo struct {
	target string
	typ    string
	args   []string
	raw    string
}

func (m MockDependencyInfo) GetTarget() string {
	return m.target
}

func (m MockDependencyInfo) GetType() string {
	return m.typ
}

func (m MockDependencyInfo) GetArgs() []string {
	return m.args
}

func (m MockDependencyInfo) GetRaw() string {
	return m.raw
}

func TestCheckTCP(t *testing.T) {
	// Start a TCP listener for testing
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start TCP listener: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()

	// Accept connections in a goroutine
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	tests := []struct {
		name        string
		target      string
		expectError bool
	}{
		{"ValidConnection", addr, false},
		{"InvalidHost", "nonexistent.local:12345", true},
		{"ClosedPort", "127.0.0.1:1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := MockDependencyInfo{
				target: tt.target,
				typ:    "tcp",
				raw:    "tcp://" + tt.target,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()

			err := CheckTCP(ctx, dep, nil)
			if (err != nil) != tt.expectError {
				t.Errorf("CheckTCP() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}

func TestCheckUDP(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		expectError bool
	}{
		// UDP checks don't usually provide reliable success feedback
		// so we're mostly testing that the function runs without panic
		{"LocalhostUDP", "127.0.0.1:12345", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := MockDependencyInfo{
				target: tt.target,
				typ:    "udp",
				raw:    "udp://" + tt.target,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()

			err := CheckUDP(ctx, dep, nil)
			if (err != nil) != tt.expectError {
				t.Errorf("CheckUDP() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}

func TestCheckHTTP(t *testing.T) {
	// Create test servers
	serverOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	}))
	defer serverOK.Close()

	serverBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "Error")
	}))
	defer serverBad.Close()

	serverTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	}))
	defer serverTLS.Close()

	// Create a client that accepts self-signed certs
	insecureClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	tests := []struct {
		name        string
		target      string
		typ         string
		client      *http.Client
		expectError bool
	}{
		{"ValidHTTP", strings.TrimPrefix(serverOK.URL, "http://"), "http", nil, false},
		{"ErrorStatusHTTP", strings.TrimPrefix(serverBad.URL, "http://"), "http", nil, true},
		{"InvalidURL", "invalid-url", "http", nil, true},
		{"ValidHTTPS", strings.TrimPrefix(serverTLS.URL, "https://"), "https", insecureClient, false},
		{"ValidExplicitScheme", serverOK.URL, "http", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := MockDependencyInfo{
				target: tt.target,
				typ:    tt.typ,
				raw:    tt.typ + "://" + tt.target,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			err := CheckHTTP(ctx, dep, tt.client)
			if (err != nil) != tt.expectError {
				t.Errorf("CheckHTTP() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}

func TestCheckExec(t *testing.T) {
	// Create temporary test scripts
	tempDir := t.TempDir()

	// Success script
	successScript := filepath.Join(tempDir, "success.sh")
	if err := os.WriteFile(successScript, []byte("#!/bin/sh\necho 'Success'\nexit 0\n"), 0755); err != nil {
		t.Fatalf("Failed to create success script: %v", err)
	}

	// Failure script
	failureScript := filepath.Join(tempDir, "failure.sh")
	if err := os.WriteFile(failureScript, []byte("#!/bin/sh\necho 'Failure' >&2\nexit 1\n"), 0755); err != nil {
		t.Fatalf("Failed to create failure script: %v", err)
	}

	// Timeout script
	timeoutScript := filepath.Join(tempDir, "timeout.sh")
	if err := os.WriteFile(timeoutScript, []byte("#!/bin/sh\nsleep 2\nexit 0\n"), 0755); err != nil {
		t.Fatalf("Failed to create timeout script: %v", err)
	}

	tests := []struct {
		name        string
		target      string
		args        []string
		timeout     time.Duration
		expectError bool
	}{
		{"SuccessScript", successScript, nil, 1 * time.Second, false},
		{"FailureScript", failureScript, nil, 1 * time.Second, true},
		{"TimeoutScript", timeoutScript, nil, 100 * time.Millisecond, true},
		{"NonexistentScript", "/nonexistent/script.sh", nil, 1 * time.Second, true},
		{"WithArgs", successScript, []string{"arg1", "arg2"}, 1 * time.Second, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := MockDependencyInfo{
				target: tt.target,
				typ:    "exec",
				args:   tt.args,
				raw:    fmt.Sprintf("exec://%s %s", tt.target, strings.Join(tt.args, " ")),
			}

			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()

			err := CheckExec(ctx, dep, nil)
			if (err != nil) != tt.expectError {
				t.Errorf("CheckExec() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}
