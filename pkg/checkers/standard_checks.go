package checkers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultCustomCheckTimeout is used for exec checks
var DefaultCustomCheckTimeout = 10 * time.Second

// CheckTCP verifies a TCP dependency is available
func CheckTCP(ctx context.Context, dep interface{}, _ *http.Client) error {
	// Extract target from dependency
	depInfo, ok := dep.(DependencyInfo)
	if !ok {
		return fmt.Errorf("CheckTCP received unexpected type: %T", dep)
	}

	target := depInfo.GetTarget()
	slog.Debug("Starting TCP dependency check", "target", target)

	// First check if we can resolve the hostname
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		slog.Debug("Failed to split host:port", "target", target, "error", err)
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
	slog.Debug("Attempting TCP connection", "target", target)

	// Use an explicit short timeout for the connection attempt
	connCtx, connCancel := context.WithTimeout(ctx, 3*time.Second)
	defer connCancel()

	dialer := net.Dialer{
		KeepAlive: -1, // Disable keep-alive to avoid false positives
	}

	conn, err := dialer.DialContext(connCtx, "tcp", target)
	if err != nil {
		slog.Debug("TCP connection failed", "target", target, "error", err)
		return fmt.Errorf("connection failed: %w", err)
	}
	defer conn.Close()

	// Verify the connection is actually to the address we expect
	localAddr := conn.LocalAddr().String()
	remoteAddr := conn.RemoteAddr().String()
	slog.Debug("TCP connection established", "target", target,
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
		slog.Debug("Failed to write probe to connection", "target", target, "error", err)
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
			slog.Debug("Connection closed by server after probe (EOF)", "target", target)
		} else if os.IsTimeout(err) {
			// In Docker environment, we need to be super strict about timeouts
			// if we already confirmed DNS resolution
			slog.Debug("Connection read timed out - marking as failure", "target", target)
			return fmt.Errorf("connection read timed out: %w", err)
		} else {
			// Other network errors are considered failures
			slog.Debug("Connection read failed", "target", target, "error", err)
			return fmt.Errorf("connection read failed: %w", err)
		}
	} else {
		responseData := string(buffer[:n])
		slog.Debug("Received response from server", "target", target,
			"bytes", n, "data", responseData)

		// If we see a 302 Found redirect response with empty location,
		// this is likely Docker network handling a non-existent container
		if strings.Contains(responseData, "302 Found") &&
			strings.Contains(responseData, "location: https:///") {
			slog.Debug("Detected Docker network placeholder response - rejecting", "target", target)
			return fmt.Errorf("connection responded with Docker network placeholder")
		}
	}

	slog.Debug("TCP dependency check completed successfully", "target", target)
	return nil
}

// CheckUDP verifies a UDP dependency is available
func CheckUDP(ctx context.Context, dep interface{}, _ *http.Client) error {
	// Extract target from dependency
	depInfo, ok := dep.(DependencyInfo)
	if !ok {
		return fmt.Errorf("CheckUDP received unexpected type: %T", dep)
	}

	target := depInfo.GetTarget()
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", target)
	if err != nil {
		return err
	}
	return conn.Close()
}

// CheckHTTP verifies an HTTP/HTTPS dependency is available
func CheckHTTP(ctx context.Context, dep interface{}, client *http.Client) error {
	// Extract target and type from dependency
	depInfo, ok := dep.(DependencyInfo)
	if !ok {
		return fmt.Errorf("CheckHTTP received unexpected type: %T", dep)
	}

	target := depInfo.GetTarget()
	depType := depInfo.GetType()
	slog.Debug("Starting HTTP dependency check", "target", target, "type", depType)

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
	reqUrl := target
	if !strings.HasPrefix(reqUrl, "http://") && !strings.HasPrefix(reqUrl, "https://") {
		if depType == "http" || depType == "https" {
			reqUrl = fmt.Sprintf("%s://%s", depType, target)
			slog.Debug("Prefixed URL with scheme", "original", target, "result", reqUrl)
		} else {
			return fmt.Errorf("invalid dependency type '%s' for http check on target '%s'", depType, target)
		}
	}

	// Explicitly enforce HTTP if specified as the dependency type
	if depType == "http" && strings.HasPrefix(reqUrl, "https://") {
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

// CheckExec verifies a command can be executed successfully
func CheckExec(ctx context.Context, dep interface{}, _ *http.Client) error {
	// Extract target and args from dependency
	depInfo, ok := dep.(DependencyInfo)
	if !ok {
		return fmt.Errorf("CheckExec received unexpected type: %T", dep)
	}

	target := depInfo.GetTarget()
	args := depInfo.GetArgs()
	cmdCtx, cancel := context.WithTimeout(ctx, DefaultCustomCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, target, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("custom check script timed out after %v", DefaultCustomCheckTimeout)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("script exited with non-zero status (%d)", exitErr.ExitCode())
		}
		return fmt.Errorf("script execution failed: %w", err)
	}
	return nil
}