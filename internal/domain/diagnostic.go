package domain

import (
	"context"
	"errors"

	"github.com/ryancswallace/jobman/diagnostic"
)

// ErrFeatureUnavailable means an optional source contract is not configured.
var ErrFeatureUnavailable = errors.New("feature unavailable")

// DiagnosticSnapshot binds the public core metadata contract to current authority.
type DiagnosticSnapshot struct {
	NamespaceReadAuthority
	Snapshot diagnostic.SharedSnapshot `json:"snapshot"`
}

// DiagnosticRepository performs metadata-only authorized bounded reads.
type DiagnosticRepository interface {
	ReadDiagnosticSnapshot(context.Context, Principal, string, diagnostic.SharedSelection) (DiagnosticSnapshot, error)
}
