package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

type fixtureKeys struct {
	workerPublic     ed25519.PublicKey
	workerThumbprint string
	public           ed25519.PublicKey
	thumbprint       string
	brokerPublic     ed25519.PublicKey
	brokerThumbprint string
	tokenKey         []byte
}

func generateMaterial(root, host string) (fixtureKeys, error) {
	return generateMaterialProfile(root, host, primaryProfile())
}

func generateMaterialProfile(root, host string, profile fixtureProfile) (fixtureKeys, error) {
	var result fixtureKeys
	if profile.validate() != nil {
		return result, errors.New("invalid material profile")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return result, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return result, errors.New("fixture directory must be private and regular")
	}
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return result, err
	}
	now := time.Now().UTC()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SYNTHETIC Dashboard Lab only"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caPublic, caPrivate)
	if err != nil {
		return result, err
	}
	if operationErr := writeKeyPair(root, "fixture-ca", caDER, caPrivate); operationErr != nil {
		return result, operationErr
	}
	names := []string{"control-server", "dashboard-client", "broker-client"}
	if profile.secondary() {
		names = append(names, "worker-client", "directory-server")
	}
	for index, name := range names {
		public, private, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			return result, keyErr
		}
		certificate := &x509.Certificate{SerialNumber: big.NewInt(int64(index + 2)), Subject: pkix.Name{CommonName: "SYNTHETIC " + name}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(7 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
		if index == 0 || name == "directory-server" {
			certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			certificate.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			certificate.DNSNames = []string{"localhost"}
			if ip := net.ParseIP(host); ip != nil && name != "directory-server" {
				certificate.IPAddresses = append(certificate.IPAddresses, ip)
			} else if name != "directory-server" {
				certificate.DNSNames = append(certificate.DNSNames, host)
			}
		} else {
			certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, certificateErr := x509.CreateCertificate(rand.Reader, certificate, ca, public, caPrivate)
		if certificateErr != nil {
			return result, certificateErr
		}
		if operationErr := writeKeyPair(root, name, der, private); operationErr != nil {
			return result, operationErr
		}
		if index > 0 && name != "directory-server" {
			hash := sha256.Sum256(der)
			switch index {
			case 1:
				result.thumbprint = base64.RawURLEncoding.EncodeToString(hash[:])
			case 3:
				result.workerThumbprint = base64.RawURLEncoding.EncodeToString(hash[:])
			default:
				result.brokerThumbprint = base64.RawURLEncoding.EncodeToString(hash[:])
			}
		}
	}
	signers := []string{"dashboard", "broker"}
	if profile.secondary() {
		signers = append(signers, "worker")
	}
	for _, name := range signers {
		public, private, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			return result, keyErr
		}
		switch name {
		case "dashboard":
			result.public = public
		case "worker":
			result.workerPublic = public
		default:
			result.brokerPublic = public
		}
		encoded, encodeErr := x509.MarshalPKCS8PrivateKey(private)
		if encodeErr != nil {
			return result, encodeErr
		}
		if operationErr := writePrivate(filepath.Join(root, name+"-signing-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})); operationErr != nil {
			return result, operationErr
		}
		publicDER, encodeErr := x509.MarshalPKIXPublicKey(public)
		if encodeErr != nil {
			return result, encodeErr
		}
		if operationErr := writePrivate(filepath.Join(root, name+"-signing-public.pem"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})); operationErr != nil {
			return result, operationErr
		}
	}
	result.tokenKey = make([]byte, 32)
	if _, err = rand.Read(result.tokenKey); err != nil {
		return result, err
	}
	password := make([]byte, 32)
	if _, err = rand.Read(password); err != nil {
		return result, err
	}
	if operationErr := writePrivate(filepath.Join(root, "directory-password"), []byte(base64.RawURLEncoding.EncodeToString(password))); operationErr != nil {
		return result, operationErr
	}
	return result, nil
}

func writeKeyPair(root, name string, der []byte, key ed25519.PrivateKey) error {
	if err := writePrivate(filepath.Join(root, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(root, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
}
