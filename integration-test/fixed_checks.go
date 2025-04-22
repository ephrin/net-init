// This file is a direct copy of the standard checks for testing purposes
package checks

import (
	"context"
	"net/http"
	"time"

	"github.com/ephrin/net-init/pkg/checkers"
)

// DefaultCustomCheckTimeout is used for exec checks
var DefaultCustomCheckTimeout = 10 * time.Second

// CheckTCP verifies a TCP dependency is available
func CheckTCP(ctx context.Context, dep interface{}, client *http.Client) error {
	// Convert the interface to DependencyInfo if possible
	if depInfo, ok := dep.(checkers.DependencyInfo); ok {
		return checkers.CheckTCP(ctx, depInfo, client)
	}
	return checkers.CheckTCP(ctx, dep, client)
}

// CheckUDP verifies a UDP dependency is available
func CheckUDP(ctx context.Context, dep interface{}, client *http.Client) error {
	// Convert the interface to DependencyInfo if possible
	if depInfo, ok := dep.(checkers.DependencyInfo); ok {
		return checkers.CheckUDP(ctx, depInfo, client)
	}
	return checkers.CheckUDP(ctx, dep, client)
}

// CheckHTTP verifies an HTTP/HTTPS dependency is available
func CheckHTTP(ctx context.Context, dep interface{}, client *http.Client) error {
	// Convert the interface to DependencyInfo if possible
	if depInfo, ok := dep.(checkers.DependencyInfo); ok {
		return checkers.CheckHTTP(ctx, depInfo, client)
	}
	return checkers.CheckHTTP(ctx, dep, client)
}

// CheckExec verifies a command can be executed successfully
func CheckExec(ctx context.Context, dep interface{}, client *http.Client) error {
	// Convert the interface to DependencyInfo if possible
	if depInfo, ok := dep.(checkers.DependencyInfo); ok {
		return checkers.CheckExec(ctx, depInfo, client)
	}
	return checkers.CheckExec(ctx, dep, client)
}