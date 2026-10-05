package directory

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
)

// This synthetic loopback server exercises actual TLS and LDAP binding without
// developer credentials, a fixed port, sleeps, or external directory access.
func TestReaderAuthenticatedTLS(t *testing.T) {
	t.Parallel()
	config := testConfiguration()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Synthetic LDAP test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	config.CAFile = filepath.Join(directory, "ca.pem")
	config.PasswordFile = filepath.Join(directory, "password")
	if err = os.WriteFile(config.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config.PasswordFile, []byte("synthetic-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	server := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}})
	config.URL = "ldaps://" + listener.Addr().String()
	configPath := filepath.Join(directory, "directory.json")
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- serveEmptyDirectory(t.Context(), server, config.BindDN) }()
	snapshot, err := (Reader{Config: loaded}).Read(t.Context(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Accounts[0].Exists || snapshot.Groups[0].Exists || snapshot.RecoveryEpoch != 4 {
		t.Fatalf("complete empty directory=%#v", snapshot)
	}
	if serverErr := <-served; serverErr != nil {
		t.Fatal(serverErr)
	}
	// The password is loaded anew and empty values are never anonymously bound.
	if err = os.WriteFile(config.PasswordFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = credentials(config); err == nil {
		t.Fatal("empty bind password accepted")
	}
}

func serveEmptyDirectory(ctx context.Context, listener net.Listener, bindDN string) error {
	connection, err := listener.Accept()
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	if deadlineErr := connection.SetDeadline(time.Now().Add(10 * time.Second)); deadlineErr != nil {
		return deadlineErr
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	bound := false
	queries := 0
	for {
		packet, readErr := ber.ReadPacket(connection)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && bound && queries == 3 {
				return nil
			}
			return readErr
		}
		if len(packet.Children) != 2 {
			return errors.New("invalid synthetic request")
		}
		operation := packet.Children[1]
		responseTag := ber.Tag(5)
		switch uint64(operation.Tag) {
		case 0:
			if bound || len(operation.Children) != 3 || operation.Children[1].Value != bindDN || operation.Children[2].Data.String() != "synthetic-password" {
				return errors.New("invalid synthetic bind")
			}
			bound = true
			responseTag = 1
		case 3:
			if !bound {
				return errors.New("directory searched before authentication")
			}
			queries++
		default:
			return fmt.Errorf("unexpected LDAP operation %d", operation.Tag)
		}
		response := ber.NewSequence("")
		response.AppendChild(packet.Children[0])
		result := ber.Encode(ber.ClassApplication, ber.TypeConstructed, responseTag, nil, "")
		result.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, 0, ""))
		result.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
		result.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
		response.AppendChild(result)
		if _, err = connection.Write(response.Bytes()); err != nil {
			return err
		}
	}
}
