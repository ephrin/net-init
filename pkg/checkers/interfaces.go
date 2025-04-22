package checkers

import (
	"context"
	"net/http"
)

// CheckFunc defines the signature for dependency check functions
type CheckFunc func(context.Context, interface{}, *http.Client) error

// DependencyInfo defines the minimum interface needed for checks
type DependencyInfo interface {
	GetTarget() string
	GetType() string
	GetArgs() []string
	GetRaw() string
}