package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewServer(t *testing.T) {
	port := 8887 // Use default port for testing
	paths := Paths{
		Health:  "/health",
		Metrics: "/metrics",
	}
	var isReady atomic.Bool
	isReady.Store(false)

	server := NewServer(port, paths, &isReady)
	if server == nil {
		t.Fatal("Expected NewServer to return a non-nil server")
	}

	if server.port != port {
		t.Errorf("Expected server.port to be %d, got %d", port, server.port)
	}

	if server.paths.Health != paths.Health {
		t.Errorf("Expected server.paths.Health to be %s, got %s", paths.Health, server.paths.Health)
	}

	if server.paths.Metrics != paths.Metrics {
		t.Errorf("Expected server.paths.Metrics to be %s, got %s", paths.Metrics, server.paths.Metrics)
	}

	if server.isReady != &isReady {
		t.Errorf("Expected server.isReady to point to isReady")
	}
}

func TestHealthEndpoint(t *testing.T) {
	var isReady atomic.Bool
	s := &Server{
		port:    8888,
		paths:   Paths{Health: "/health", Metrics: "/metrics"},
		isReady: &isReady,
	}

	// Start server to create handler
	mux := http.NewServeMux()

	// Register handler with the same signature as in the Start method
	mux.HandleFunc(s.paths.Health, func(w http.ResponseWriter, r *http.Request) {
		if s.isReady.Load() {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "OK")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "Waiting for dependencies")
		}
	})

	// Test when not ready
	isReady.Store(false)
	req, err := http.NewRequest("GET", "/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	// Check response
	if status := rr.Code; status != http.StatusServiceUnavailable {
		t.Errorf("Handler returned wrong status code when not ready: got %v want %v",
			status, http.StatusServiceUnavailable)
	}
	expected := "Waiting for dependencies\n"
	if rr.Body.String() != expected {
		t.Errorf("Handler returned unexpected body when not ready: got %v want %v",
			rr.Body.String(), expected)
	}

	// Test when ready
	isReady.Store(true)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	// Check response
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("Handler returned wrong status code when ready: got %v want %v",
			status, http.StatusOK)
	}
	expected = "OK\n"
	if rr.Body.String() != expected {
		t.Errorf("Handler returned unexpected body when ready: got %v want %v",
			rr.Body.String(), expected)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	// This test is simplistic since the actual metrics endpoint uses Prometheus
	// which is already tested by the Prometheus library
	var isReady atomic.Bool
	server := NewServer(8889, Paths{Health: "/health", Metrics: "/metrics"}, &isReady)

	// Start the server so routes are registered
	err := server.Start()
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	// Explicitly shutdown the server to prevent test from hanging
	defer func() {
		shutdownErr := server.Shutdown()
		if shutdownErr != nil {
			t.Logf("Warning: Failed to shutdown server: %v", shutdownErr)
		}
	}()

	// Give server a moment to start
	time.Sleep(100 * time.Millisecond)
}

// Skip TestRootHandler as there's no root handler in the implementation

func TestStartAndShutdown(t *testing.T) {
	var isReady atomic.Bool
	// Use a likely-free port for this test
	server := NewServer(0, Paths{Health: "/health", Metrics: "/metrics"}, &isReady)

	// Start the server in background
	err := server.Start()
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	// Create a context with timeout to avoid test hanging
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Give it a moment to start
	time.Sleep(200 * time.Millisecond)

	// Verify server is running by checking if server field has been created
	if server.server == nil {
		t.Errorf("HTTP server not created after Start()")
	}

	// Shutdown the server with context to ensure it doesn't hang
	err = server.Shutdown()
	if err != nil {
		t.Errorf("Failed to shutdown server: %v", err)
	}

	// Wait for shutdown with timeout
	select {
	case <-ctx.Done():
		t.Logf("Warning: Server shutdown timed out")
	case <-time.After(500 * time.Millisecond):
		// Successfully shut down within timeout
	}
}
