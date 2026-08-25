package auth

import (
	"context"
	"errors"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// DevelopmentAuthenticator returns one fixed identity. Configuration keeps
// this authenticator on a loopback listener.
type DevelopmentAuthenticator struct {
	Principal domain.Principal
}

// Authenticate returns the configured development principal.
func (authenticator DevelopmentAuthenticator) Authenticate(
	context.Context,
	string,
) (domain.Principal, error) {
	if authenticator.Principal.Issuer == "" || authenticator.Principal.Subject == "" {
		return domain.Principal{}, errors.New("development principal is invalid")
	}

	return authenticator.Principal, nil
}
