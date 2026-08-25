// Package auth establishes stable client identities without making
// authorization decisions.
package auth

import (
	"context"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// Authenticator validates one HTTP Authorization value.
type Authenticator interface {
	Authenticate(context.Context, string) (domain.Principal, error)
}
