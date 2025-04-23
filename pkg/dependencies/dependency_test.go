package dependencies

import (
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestParseSingleDependency(t *testing.T) {
	// Setup Prometheus metric for test
	depStatus := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "test_dependency_up",
			Help: "Status of dependencies (1=up, 0=down).",
		},
		[]string{"dependency"},
	)

	// Test cases
	tests := []struct {
		name           string
		input          string
		expectedType   string
		expectedTarget string
		expectedArgs   []string
		expectError    bool
	}{
		{
			"ValidTCP",
			"tcp://db:5432",
			"tcp",
			"db:5432",
			nil,
			false,
		},
		{
			"ValidHTTP",
			"http://api/health",
			"http",
			"api/health",
			nil,
			false,
		},
		{
			"ValidUDP",
			"udp://metrics:8125",
			"udp",
			"metrics:8125",
			nil,
			false,
		},
		{
			"ValidExec",
			"exec:///bin/check.sh arg1 arg2",
			"exec",
			"/bin/check.sh",
			[]string{"arg1", "arg2"},
			false,
		},
		{
			"ValidImplicitTCP",
			"db:27017",
			"tcp",
			"db:27017",
			nil,
			false,
		},
		{
			"EmptyInput",
			"",
			"",
			"",
			nil,
			true,
		},
		{
			"UnknownScheme",
			"ftp://example.com",
			"",
			"",
			nil,
			true,
		},
		{
			"InvalidTCPFormat",
			"tcp://localhost",
			"",
			"",
			nil,
			true,
		},
		{
			"InvalidUDPFormat",
			"udp://localhost",
			"",
			"",
			nil,
			true,
		},
		{
			"EmptyExecCommand",
			"exec://",
			"",
			"",
			nil,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep, err := parseSingleDependency(tt.input, depStatus)

			// Check error conditions
			if tt.expectError {
				if err == nil {
					t.Errorf("NewDependency(%q) expected error, got nil", tt.input)
				}
				return
			}

			if err != nil {
				t.Errorf("NewDependency(%q) unexpected error: %v", tt.input, err)
				return
			}

			// Check dependency fields
			if dep.Type != tt.expectedType {
				t.Errorf("NewDependency(%q).Type = %q, want %q", tt.input, dep.Type, tt.expectedType)
			}

			if dep.Target != tt.expectedTarget {
				t.Errorf("NewDependency(%q).Target = %q, want %q", tt.input, dep.Target, tt.expectedTarget)
			}

			if !reflect.DeepEqual(dep.Args, tt.expectedArgs) {
				t.Errorf("NewDependency(%q).Args = %v, want %v", tt.input, dep.Args, tt.expectedArgs)
			}

			if dep.Raw != tt.input {
				t.Errorf("NewDependency(%q).Raw = %q, want %q", tt.input, dep.Raw, tt.input)
			}

			if dep.CheckFunc == nil {
				t.Errorf("NewDependency(%q).CheckFunc is nil", tt.input)
			}
		})
	}
}

func TestNewDependencies(t *testing.T) {
	// Setup Prometheus metric for test
	depStatus := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "test_dependency_up",
			Help: "Status of dependencies (1=up, 0=down).",
		},
		[]string{"dependency"},
	)

	// Test cases
	validInputs := []string{
		"tcp://db:5432",
		"http://api/health",
	}
	mixedInputs := []string{
		"tcp://db:5432",
		"invalid://scheme",
		"http://api/health",
	}

	t.Run("ValidInputs", func(t *testing.T) {
		deps, err := NewDependencies(validInputs, depStatus)
		if err != nil {
			t.Errorf("NewDependencies(%v) unexpected error: %v", validInputs, err)
			return
		}

		if len(deps) != len(validInputs) {
			t.Errorf("NewDependencies(%v) returned %d dependencies, want %d", validInputs, len(deps), len(validInputs))
		}
	})

	t.Run("SomeInvalidInputs", func(t *testing.T) {
		if _, err := NewDependencies(mixedInputs, depStatus); err == nil {
			t.Errorf("NewDependencies(%v) expected error for invalid input, got nil", mixedInputs)
		} else {
			// Verify error message mentions the invalid dependency
			if !strings.Contains(err.Error(), "invalid://scheme") {
				t.Errorf("NewDependencies(%v) error %v doesn't mention the invalid dependency", mixedInputs, err)
			}
		}
	})

	t.Run("NilInputs", func(t *testing.T) {
		deps, err := NewDependencies(nil, depStatus)
		if err != nil {
			t.Errorf("NewDependencies(nil) unexpected error: %v", err)
			return
		}

		if len(deps) != 0 {
			t.Errorf("NewDependencies(nil) returned %d dependencies, want 0", len(deps))
		}
	})

	t.Run("EmptyInputs", func(t *testing.T) {
		deps, err := NewDependencies([]string{}, depStatus)
		if err != nil {
			t.Errorf("NewDependencies([]) unexpected error: %v", err)
			return
		}

		if len(deps) != 0 {
			t.Errorf("NewDependencies([]) returned %d dependencies, want 0", len(deps))
		}
	})
}

func TestCheckAllReady(t *testing.T) {
	tests := []struct {
		name     string
		deps     []Dependency
		expected bool
	}{
		{
			"NoDeps",
			[]Dependency{},
			true,
		},
		{
			"AllReady",
			[]Dependency{
				{IsReady: createBoolWithValue(true)},
				{IsReady: createBoolWithValue(true)},
			},
			true,
		},
		{
			"OneNotReady",
			[]Dependency{
				{IsReady: createBoolWithValue(true)},
				{IsReady: createBoolWithValue(false)},
			},
			false,
		},
		{
			"AllNotReady",
			[]Dependency{
				{IsReady: createBoolWithValue(false)},
				{IsReady: createBoolWithValue(false)},
			},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CheckAllReady(tt.deps)
			if result != tt.expected {
				t.Errorf("CheckAllReady() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// Helper function to create atomic.Bool with initial value
func createBoolWithValue(val bool) atomic.Bool {
	var b atomic.Bool
	b.Store(val)
	return b
}
