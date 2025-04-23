package dependencies

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Checker manages dependency check operations
type Checker struct {
	deps          []Dependency
	httpClient    *http.Client
	readyChan     chan struct{}
	readyOnce     *sync.Once
	allReady      *atomic.Bool
	overallMetric prometheus.Gauge
	retryInterval time.Duration
	waitGroup     *sync.WaitGroup
}

// NewChecker creates a dependency checker
func NewChecker(
	deps []Dependency,
	httpClient *http.Client,
	readyChan chan struct{},
	readyOnce *sync.Once,
	allReady *atomic.Bool,
	overallMetric prometheus.Gauge,
	retryInterval time.Duration,
	wg *sync.WaitGroup,
) *Checker {
	return &Checker{
		deps:          deps,
		httpClient:    httpClient,
		readyChan:     readyChan,
		readyOnce:     readyOnce,
		allReady:      allReady,
		overallMetric: overallMetric,
		retryInterval: retryInterval,
		waitGroup:     wg,
	}
}

// Start launches checks for all dependencies
func (c *Checker) Start(ctx context.Context) {
	// If no dependencies, mark as ready immediately
	if len(c.deps) == 0 {
		c.markAllReady()
		return
	}

	// Launch a check goroutine for each dependency
	for i := range c.deps {
		c.waitGroup.Add(1)
		go c.startCheck(ctx, &c.deps[i])
	}
}

// markAllReady marks the system as ready with no dependencies
func (c *Checker) markAllReady() {
	c.allReady.Store(true)
	c.overallMetric.Set(1)
	slog.Info("No dependencies specified, marking ready immediately.")
	c.readyOnce.Do(func() { close(c.readyChan) })
}

// startCheck launches a single dependency check goroutine
func (c *Checker) startCheck(ctx context.Context, dep *Dependency) {
	defer c.waitGroup.Done()
	slog.Info("Starting check", "dependency", dep.Raw)

	ticker := time.NewTicker(c.retryInterval)
	defer ticker.Stop()

	// Perform initial check
	err := c.performCheck(ctx, dep)
	isInitiallyReady := err == nil

	// Update status based on initial check
	if !isInitiallyReady {
		dep.Metric.Set(0)
	}
	c.handleStateChange(dep, isInitiallyReady, err)

	// Subsequent checks
	if !isInitiallyReady {
		c.monitorDependency(ctx, dep, ticker)
	} else {
		c.waitForCancellation(ctx, dep)
	}
}

// performCheck performs a single check on a dependency
func (c *Checker) performCheck(ctx context.Context, dep *Dependency) error {
	checkCtx, checkCancel := context.WithTimeout(ctx, c.retryInterval)
	defer checkCancel()
	return dep.CheckFunc(checkCtx, dep, c.httpClient)
}

// handleStateChange updates dependency state and metrics
func (c *Checker) handleStateChange(dep *Dependency, isReady bool, err error) bool {
	stateChanged := false
	applicationStarted := c.allReady.Load()

	if isReady { // Dependency is ready
		if !dep.IsReady.Load() {
			slog.Info("Dependency ready", "dependency", dep.Raw)
			dep.IsReady.Store(true)
			dep.Metric.Set(1)
			stateChanged = true
		}
	} else { // Dependency is not ready
		if dep.IsReady.Load() {
			// If the application has already started (all dependencies were ready before),
			// just log the issue but don't change the state
			if applicationStarted {
				slog.Warn("Dependency connection check failed (ignoring since application has started)",
					"dependency", dep.Raw, "error", err)
				// Don't change state or metric, application is already running
			} else {
				// During initial startup, mark dependency as not ready
				slog.Warn("Dependency NOT ready (was ready before)", "dependency", dep.Raw, "error", err)
				dep.IsReady.Store(false)
				dep.Metric.Set(0)
				stateChanged = true
			}
		} else {
			slog.Debug("Dependency not ready", "dependency", dep.Raw, "error", err)
		}
	}

	if stateChanged {
		c.checkOverallReady()
	}

	return stateChanged
}

// checkOverallReady updates the overall status
func (c *Checker) checkOverallReady() {
	if CheckAllReady(c.deps) {
		if !c.allReady.Load() {
			slog.Info("All dependencies are now ready!")
			c.allReady.Store(true)
			c.overallMetric.Set(1)
			c.readyOnce.Do(func() { close(c.readyChan) }) // Signal readiness once
		}
	} else {
		if c.allReady.Load() {
			slog.Warn("Overall status changed to NOT READY")
			c.allReady.Store(false)
			c.overallMetric.Set(0)
		}
	}
}

// monitorDependency handles periodic dependency checks
func (c *Checker) monitorDependency(ctx context.Context, dep *Dependency, ticker *time.Ticker) {
	for {
		select {
		case <-ctx.Done():
			c.handleCancellation(dep, ctx.Err())
			return
		case <-ticker.C:
			err := c.performCheck(ctx, dep)
			c.handleStateChange(dep, err == nil, err)
		}
	}
}

// waitForCancellation waits for context cancellation for initially ready dependencies
func (c *Checker) waitForCancellation(ctx context.Context, dep *Dependency) {
	<-ctx.Done()
	// Just log cancellation but don't change state - let application handle reconnection failures
	slog.Debug("Check stopping due to context cancellation", "dependency", dep.Raw, "reason", ctx.Err())
}

// handleCancellation handles cleanup when context is cancelled
func (c *Checker) handleCancellation(dep *Dependency, reason error) {
	// Just log cancellation but don't change dependency state
	slog.Debug("Check stopping due to context cancellation", "dependency", dep.Raw, "reason", reason)
	// No longer marking as not ready on cancellation:
	// Once dependencies are ready and application starts, let the application handle reconnection failures
}
