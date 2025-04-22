package dependencies

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/ephrin/net-init/pkg/checkers"
	"github.com/prometheus/client_golang/prometheus"
)

// Dependency represents a network or executable dependency to wait for
type Dependency struct {
	Raw       string                // Raw dependency string as specified by user
	Type      string                // Type of dependency (tcp, http, etc.)
	Target    string                // Target address/path
	Args      []string              // Arguments (for exec type)
	IsReady   atomic.Bool           // Whether the dependency is ready
	CheckFunc checkers.CheckFunc    // Function to check dependency status
	Metric    prometheus.Gauge      // Prometheus metric for this dependency
}

// GetTarget implements the DependencyInfo interface
func (d *Dependency) GetTarget() string {
	return d.Target
}

// GetType implements the DependencyInfo interface
func (d *Dependency) GetType() string {
	return d.Type
}

// GetArgs implements the DependencyInfo interface
func (d *Dependency) GetArgs() []string {
	return d.Args
}

// GetRaw implements the DependencyInfo interface
func (d *Dependency) GetRaw() string {
	return d.Raw
}

// NewDependencies parses raw dependency strings into Dependency objects
func NewDependencies(depStrings []string, statusGauge *prometheus.GaugeVec) ([]Dependency, error) {
	deps := make([]Dependency, 0, len(depStrings))

	for _, depRaw := range depStrings {
		dep, err := parseSingleDependency(depRaw, statusGauge)
		if err != nil {
			return nil, fmt.Errorf("failed to parse dependency '%s': %w", depRaw, err)
		}
		deps = append(deps, dep)
	}

	return deps, nil
}

// parseSingleDependency parses one dependency string entry.
func parseSingleDependency(depRaw string, statusGauge *prometheus.GaugeVec) (Dependency, error) {
	dep := Dependency{Raw: depRaw}
	parts := strings.SplitN(depRaw, "://", 2)

	if len(parts) == 1 { // Default to TCP
		target := parts[0]
		if !strings.Contains(target, ":") {
			return dep, fmt.Errorf("invalid default TCP dependency format (missing port?): '%s'", depRaw)
		}
		dep.Type = "tcp"
		dep.Target = target
	} else {
		depType := strings.ToLower(parts[0])
		target := parts[1]
		if target == "" {
			return dep, fmt.Errorf("missing target for dependency type '%s' in '%s'", depType, depRaw)
		}
		dep.Type = depType
		dep.Target = target
	}

	// Assign check function based on type
	switch dep.Type {
	case "tcp":
		if !strings.Contains(dep.Target, ":") {
			return dep, fmt.Errorf("invalid TCP dependency format (missing port?): '%s'", depRaw)
		}
		dep.CheckFunc = checkers.CheckTCP
	case "udp":
		if !strings.Contains(dep.Target, ":") {
			return dep, fmt.Errorf("invalid UDP dependency format (missing port?): '%s'", depRaw)
		}
		dep.CheckFunc = checkers.CheckUDP
	case "http", "https":
		dep.CheckFunc = checkers.CheckHTTP
	case "exec":
		cmdParts := strings.Fields(dep.Target)
		if len(cmdParts) == 0 {
			return dep, fmt.Errorf("invalid exec dependency format (empty command): '%s'", depRaw)
		}
		dep.Target = cmdParts[0] // The command/script path
		dep.Args = cmdParts[1:]  // The arguments
		dep.CheckFunc = checkers.CheckExec
	default:
		return dep, fmt.Errorf("unsupported dependency type '%s' in '%s'", dep.Type, depRaw)
	}

	// Initialize metric
	dep.Metric = statusGauge.WithLabelValues(dep.Raw)

	return dep, nil
}

// CheckAllReady checks if all dependencies are ready
func CheckAllReady(deps []Dependency) bool {
	if len(deps) == 0 {
		return true
	}

	for i := range deps {
		if !deps[i].IsReady.Load() {
			return false
		}
	}

	return true
}
