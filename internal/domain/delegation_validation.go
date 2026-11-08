package domain

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
)

// ValidateDelegationKey checks a bounded operator registration without storage access.
func ValidateDelegationKey(key DelegationKey) error {
	if key.ServiceID == "" || len(key.ServiceID) > 128 || key.KeyID == "" || len(key.KeyID) > 128 || key.Audience == "" || len(key.Audience) > 512 || len(key.PublicKey) != ed25519.PublicKeySize || len(key.CertificateThumbprints) == 0 || len(key.CertificateThumbprints) > 8 || len(key.NamespaceIDs) == 0 || len(key.NamespaceIDs) > 320 || len(key.Operations) == 0 || len(key.Operations) > 8 {
		return errors.New("delegation service registration is invalid")
	}
	for _, thumbprint := range key.CertificateThumbprints {
		decoded, err := base64.RawURLEncoding.DecodeString(thumbprint)
		if err != nil || len(decoded) != 32 {
			return errors.New("delegation certificate thumbprint is invalid")
		}
	}
	for _, namespaceID := range key.NamespaceIDs {
		if !IsID(namespaceID) {
			return errors.New("delegation namespace ID is invalid")
		}
	}
	for _, operation := range key.Operations {
		if !DelegationOperation(operation) {
			return errors.New("delegation permits only explicit monitoring reads")
		}
	}
	return nil
}
