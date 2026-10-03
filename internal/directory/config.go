// Package directory verifies direct Active Directory grants over authenticated TLS.
package directory

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/go-ldap/ldap/v3"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// Config contains public operator configuration and secret file references.
type Config struct {
	URL          string                  `json:"url"`
	BaseDN       string                  `json:"baseDn"`
	BindDN       string                  `json:"bindDn"`
	PasswordFile string                  `json:"passwordFile"`
	CAFile       string                  `json:"caFile"`
	Mapping      domain.DirectoryMapping `json:"mapping"`
}

// Load reads a bounded strict configuration; it never logs its contents.
func Load(path string) (Config, error) {
	var config Config
	data, err := readFile(path, 4<<20)
	if err != nil {
		return config, errors.New("read directory configuration failed")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		return Config{}, errors.New("directory configuration is invalid JSON")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("directory configuration has trailing data")
	}
	return config, Validate(config)
}

// Validate rejects unencrypted transport, ambiguous mappings, and unbounded input.
func Validate(config Config) error {
	endpoint, err := url.Parse(config.URL)
	if err != nil || endpoint.Scheme != "ldaps" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" {
		return errors.New("directory requires an explicit LDAPS endpoint")
	}
	if config.PasswordFile == "" || config.CAFile == "" {
		return errors.New("directory password and CA files are required")
	}
	for _, dn := range []string{config.BaseDN, config.BindDN} {
		parsed, parseErr := ldap.ParseDN(dn)
		if parseErr != nil || len(parsed.RDNs) == 0 {
			return errors.New("directory base and bind distinguished names are required")
		}
	}
	return ValidateMapping(config.Mapping)
}

// ValidateMapping is shared by the LDAP reader and persistence boundary.
func ValidateMapping(mapping domain.DirectoryMapping) error {
	if mapping.SourceID == "" || len(mapping.SourceID) > 128 || mapping.Revision < 1 || len(mapping.Namespaces) == 0 || len(mapping.Namespaces) > 320 || len(mapping.Bindings) > 1280 || len(mapping.Identities) > 10000 {
		return errors.New("directory mapping bounds are invalid")
	}
	namespaces := map[string]bool{}
	for _, id := range mapping.Namespaces {
		if !domain.IsID(id) || namespaces[id] {
			return errors.New("directory namespace IDs must be distinct UUIDs")
		}
		namespaces[id] = true
	}
	for _, id := range mapping.ApprovedTransitions {
		if !namespaces[id] {
			return errors.New("directory transition approval is outside configured namespaces")
		}
	}
	groups := map[string]bool{}
	for _, binding := range mapping.Bindings {
		if !domain.IsID(binding.GroupID) || groups[binding.GroupID] || !namespaces[binding.NamespaceID] || !domain.ValidRole(binding.Role) {
			return errors.New("directory group must map one-to-one to a configured namespace role")
		}
		groups[binding.GroupID] = true
	}
	accounts, principals, aliases := map[string]bool{}, map[string]bool{}, map[string]string{}
	for _, identity := range mapping.Identities {
		if !domain.IsID(identity.DirectoryID) || !domain.IsID(identity.PrincipalID) || accounts[identity.DirectoryID] || principals[identity.PrincipalID] || len(identity.DisplayName) > 512 || len(identity.Aliases) > 16 {
			return errors.New("directory identities must have distinct directory and principal UUIDs")
		}
		accounts[identity.DirectoryID] = true
		principals[identity.PrincipalID] = true
		for _, alias := range IdentityAliases(identity) {
			if alias.Issuer == "" || len(alias.Issuer) > 512 || alias.Subject == "" || len(alias.Subject) > 512 || strings.ContainsRune(alias.Issuer, 0) || strings.ContainsRune(alias.Subject, 0) {
				return errors.New("directory alias is invalid")
			}
			key := alias.Issuer + "\x00" + alias.Subject
			if existing := aliases[key]; existing != "" && existing != identity.DirectoryID {
				return errors.New("directory alias maps to multiple identities")
			}
			aliases[key] = identity.DirectoryID
		}
	}
	return nil
}

// IdentityAliases includes the approved canonical Control sign-in identity.
func IdentityAliases(identity domain.DirectoryIdentity) []domain.DirectoryAlias {
	result := append([]domain.DirectoryAlias{{Issuer: identity.Issuer, Subject: identity.Subject}}, identity.Aliases...)
	slices.SortFunc(result, func(a, b domain.DirectoryAlias) int {
		if c := strings.Compare(a.Issuer, b.Issuer); c != 0 {
			return c
		}
		return strings.Compare(a.Subject, b.Subject)
	})
	return slices.Compact(result)
}

// Digest binds snapshots to the exact public mapping, excluding secret material.
func Digest(mapping domain.DirectoryMapping) (string, error) {
	data, err := json.Marshal(mapping)
	if err != nil {
		return "", fmt.Errorf("encode directory mapping: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func readFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("directory input exceeds size limit")
	}
	return data, nil
}

func credentials(config Config) (*tls.Config, string, error) {
	ca, err := readFile(config.CAFile, 1<<20)
	if err != nil {
		return nil, "", errors.New("read directory CA failed")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, "", errors.New("directory CA contains no certificates")
	}
	password, err := readFile(config.PasswordFile, 65536)
	if err != nil {
		return nil, "", errors.New("read directory bind password failed")
	}
	secret := strings.TrimSuffix(strings.TrimSuffix(string(password), "\n"), "\r")
	if secret == "" || strings.ContainsRune(secret, 0) {
		return nil, "", errors.New("directory bind password is empty or invalid")
	}
	endpoint, err := url.Parse(config.URL)
	if err != nil {
		return nil, "", errors.New("directory endpoint is invalid")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: endpoint.Hostname(), RootCAs: roots}, secret, nil
}
