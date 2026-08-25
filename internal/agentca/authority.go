// Package agentca issues short-lived client certificates for per-user agents.
package agentca

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"time"
)

const agentURIBase = "urn:jobman:agent:"

// Credential contains a newly issued public client certificate. Private key
// material is generated and retained by the agent.
type Credential struct {
	CertificatePEM   string
	CACertificatePEM string
	Serial           string
	PublicKeyDigest  string
	ExpiresAt        time.Time
}

// Authority signs agent CSRs using an operator-managed CA.
type Authority struct {
	certificate    *x509.Certificate
	privateKey     crypto.Signer
	certificatePEM []byte
	now            func() time.Time
}

// Load reads and validates one CA certificate and private key pair.
func Load(certificateFile, keyFile string) (*Authority, error) {
	certificatePEM, err := os.ReadFile(certificateFile)
	if err != nil {
		return nil, fmt.Errorf("read agent CA certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read agent CA key: %w", err)
	}

	return Parse(certificatePEM, keyPEM, time.Now)
}

// Parse constructs an authority from PEM data. It is primarily useful for
// deterministic tests and embedded deployments.
func Parse(certificatePEM, keyPEM []byte, now func() time.Time) (*Authority, error) {
	certificateBlock, rest := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" || len(rest) != 0 {
		return nil, errors.New("agent CA certificate must contain exactly one PEM certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse agent CA certificate: %w", err)
	}
	if !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("agent CA certificate is not authorized to sign certificates")
	}
	keyBlock, rest := pem.Decode(keyPEM)
	if keyBlock == nil || len(rest) != 0 {
		return nil, errors.New("agent CA key must contain exactly one PEM private key")
	}
	privateKey, err := parsePrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	if !publicKeysEqual(certificate.PublicKey, privateKey.Public()) {
		return nil, errors.New("agent CA certificate and private key do not match")
	}
	if now == nil {
		return nil, errors.New("agent CA clock is required")
	}

	return &Authority{
		certificate: certificate, privateKey: privateKey,
		certificatePEM: append([]byte(nil), certificatePEM...), now: now,
	}, nil
}

// Issue validates a PKCS #10 CSR and returns a short-lived client certificate
// bound to agentID.
func (authority *Authority) Issue(csrPEM, agentID string, lifetime time.Duration) (Credential, error) {
	if authority == nil || lifetime <= 0 {
		return Credential{}, errors.New("agent certificate authority and positive lifetime are required")
	}
	block, rest := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
		return Credential{}, errors.New("agent CSR must contain exactly one PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return Credential{}, fmt.Errorf("parse agent CSR: %w", err)
	}
	if err = csr.CheckSignature(); err != nil {
		return Credential{}, fmt.Errorf("verify agent CSR signature: %w", err)
	}
	publicKeyDigest, err := PublicKeyDigest(csr.PublicKey)
	if err != nil {
		return Credential{}, err
	}
	serialBytes := make([]byte, 16)
	if _, err = rand.Read(serialBytes); err != nil {
		return Credential{}, fmt.Errorf("generate agent certificate serial: %w", err)
	}
	serialBytes[0] &= 0x7f
	serial := new(big.Int).SetBytes(serialBytes)
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	identityURI, err := url.Parse(agentURIBase + agentID)
	if err != nil {
		return Credential{}, fmt.Errorf("construct agent certificate identity: %w", err)
	}
	now := authority.now().UTC()
	expiresAt := now.Add(lifetime)
	if expiresAt.After(authority.certificate.NotAfter) {
		expiresAt = authority.certificate.NotAfter.UTC()
	}
	if !expiresAt.After(now) {
		return Credential{}, errors.New("agent CA certificate has expired")
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: csr.Subject,
		NotBefore: now.Add(-time.Minute), NotAfter: expiresAt,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:                  []*url.URL{identityURI},
		BasicConstraintsValid: true,
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, csr.PublicKey, authority.privateKey)
	if err != nil {
		return Credential{}, fmt.Errorf("sign agent certificate: %w", err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded})

	return Credential{
		CertificatePEM: string(certificate), CACertificatePEM: string(authority.certificatePEM),
		Serial: serial.Text(16), PublicKeyDigest: publicKeyDigest, ExpiresAt: expiresAt,
	}, nil
}

// CertificatePool returns a new pool containing the authority certificate.
func (authority *Authority) CertificatePool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(authority.certificate)

	return pool
}

// AgentID returns the agent identity encoded in a verified client certificate.
func AgentID(certificate *x509.Certificate) (string, error) {
	if certificate == nil || len(certificate.URIs) != 1 {
		return "", errors.New("agent certificate has no unique identity URI")
	}
	value := certificate.URIs[0].String()
	if len(value) <= len(agentURIBase) || value[:len(agentURIBase)] != agentURIBase {
		return "", errors.New("agent certificate identity URI is invalid")
	}

	return value[len(agentURIBase):], nil
}

// PublicKeyDigest returns the stable SHA-256 digest of a PKIX public key.
func PublicKeyDigest(publicKey any) (string, error) {
	encoded, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", fmt.Errorf("encode agent public key: %w", err)
	}
	sum := sha256.Sum256(encoded)

	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func parsePrivateKey(encoded []byte) (crypto.Signer, error) {
	values := []func([]byte) (any, error){
		x509.ParsePKCS8PrivateKey,
		func(value []byte) (any, error) { return x509.ParseECPrivateKey(value) },
		func(value []byte) (any, error) { return x509.ParsePKCS1PrivateKey(value) },
	}
	for _, parse := range values {
		value, err := parse(encoded)
		if err == nil {
			signer, ok := value.(crypto.Signer)
			if !ok {
				return nil, errors.New("agent CA private key cannot sign")
			}

			return signer, nil
		}
	}

	return nil, errors.New("agent CA private key format is unsupported")
}

func publicKeysEqual(left, right any) bool {
	leftEncoded, leftErr := x509.MarshalPKIXPublicKey(left)
	rightEncoded, rightErr := x509.MarshalPKIXPublicKey(right)
	if leftErr != nil || rightErr != nil {
		return false
	}

	return bytes.Equal(leftEncoded, rightEncoded)
}
