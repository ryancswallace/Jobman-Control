package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func serveDirectory(ctx context.Context, root string) error {
	return serveDirectoryProfile(ctx, root, primaryProfile())
}

func serveDirectoryProfile(ctx context.Context, root string, profile fixtureProfile) error {
	if err := verifyDirectoryProfile(root, profile); err != nil {
		return err
	}
	certName := "control-server"
	if profile.secondary() {
		certName = "directory-server"
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Join(root, certName+".crt"), filepath.Join(root, certName+".key"))
	if err != nil {
		return err
	}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:"+profile.ldapPort)
	if err != nil {
		return err
	}
	defer listener.Close()
	return serveDirectoryOnProfile(ctx, tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}), root, profile)
}

func serveDirectoryOn(ctx context.Context, listener net.Listener, root string) error {
	return serveDirectoryOnProfile(ctx, listener, root, primaryProfile())
}

func serveDirectoryOnProfile(ctx context.Context, listener net.Listener, root string, profile fixtureProfile) error {
	if err := verifyDirectoryProfile(root, profile); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// Only one local Control instance uses this synthetic fixture. A short
		// connection deadline bounds an unresponsive client without unbounded workers.
		if connectionErr := serveDirectoryConnectionProfile(ctx, connection, root, profile); connectionErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
	}
}

func serveDirectoryConnectionProfile(ctx context.Context, connection net.Conn, root string, profile fixtureProfile) error {
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	password, err := readBounded(filepath.Join(root, "directory-password"), 1024)
	if err != nil {
		return err
	}
	var state fixtureState
	if operationErr := readJSON(filepath.Join(root, "directory-state.json"), &state); operationErr != nil {
		return operationErr
	}
	if operationErr := validateState(state); operationErr != nil {
		return operationErr
	}
	bound := false
	for range 128 {
		packet, readErr := readLDAPFrame(connection)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		if len(packet.Children) != 2 {
			return errors.New("invalid fixture LDAP message")
		}
		id, operation := packet.Children[0], packet.Children[1]
		if operation.ClassType != ber.ClassApplication {
			return errors.New("invalid fixture LDAP operation")
		}
		switch uint64(operation.Tag) {
		case 0:
			if bound || len(operation.Children) != 3 || operation.Children[1].Value != profile.bindDN || subtle.ConstantTimeCompare(operation.Children[2].Data.Bytes(), password) != 1 {
				if writeErr := writeLDAPResult(connection, id, 1, 49); writeErr != nil {
					return writeErr
				}
				return errors.New("fixture LDAP bind denied")
			}
			bound = true
			if operationErr := writeLDAPResult(connection, id, 1, 0); operationErr != nil {
				return operationErr
			}
		case 2:
			return nil
		case 3:
			if !bound || len(operation.Children) != 8 || operation.Children[0].Value != profile.baseDN {
				return errors.New("fixture LDAP query denied")
			}
			guid := filterGUID(operation.Children[6])
			dn, attributes := directoryEntryProfile(state, guid, profile)
			if dn != "" {
				response := ber.NewSequence("")
				response.AppendChild(id)
				entry := ber.Encode(ber.ClassApplication, ber.TypeConstructed, 4, nil, "")
				entry.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, dn, ""))
				list := ber.NewSequence("")
				names := make([]string, 0, len(attributes))
				for name := range attributes {
					names = append(names, name)
				}
				slices.Sort(names)
				for _, name := range names {
					attribute := ber.NewSequence("")
					attribute.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, name, ""))
					values := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "")
					for _, value := range attributes[name] {
						values.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, string(value), ""))
					}
					attribute.AppendChild(values)
					list.AppendChild(attribute)
				}
				entry.AppendChild(list)
				response.AppendChild(entry)
				if _, err = connection.Write(response.Bytes()); err != nil {
					return err
				}
			}
			if operationErr := writeLDAPResult(connection, id, 5, 0); operationErr != nil {
				return operationErr
			}
		default:
			return errors.New("unsupported fixture LDAP operation")
		}
	}
	return errors.New("fixture LDAP query budget exhausted")
}

func validateState(state fixtureState) error {
	// The integrated scale fixture needs 25 additional viewers alongside the
	// original Alice/Bob accounts, and 10 new namespace groups. Keep the test
	// directory finite; this does not change the production LDAP reader limits.
	if state.Revision < 1 || len(state.Users) > 32 || len(state.Groups) > 32 {
		return errors.New("invalid synthetic directory bounds")
	}
	seen := map[string]bool{}
	for _, user := range state.Users {
		if !domain.IsID(user.DirectoryID) || seen[user.DirectoryID] {
			return errors.New("invalid synthetic user")
		}
		seen[user.DirectoryID] = true
	}
	groups := map[string]bool{}
	for _, group := range state.Groups {
		if !domain.IsID(group.ID) || groups[group.ID] || seen[group.ID] || len(group.Members) > 32 {
			return errors.New("invalid synthetic group")
		}
		groups[group.ID] = true
		for _, member := range group.Members {
			if !seen[member] {
				return errors.New("unknown synthetic member")
			}
		}
	}
	return nil
}

func filterGUID(packet *ber.Packet) []byte {
	if packet.ClassType == ber.ClassContext && packet.Tag == 3 && len(packet.Children) == 2 && packet.Children[0].Data.String() == "objectGUID" {
		return packet.Children[1].Data.Bytes()
	}
	for _, child := range packet.Children {
		if value := filterGUID(child); value != nil {
			return value
		}
	}
	return nil
}

func directoryEntryProfile(state fixtureState, guid []byte, profile fixtureProfile) (distinguishedName string, values map[string][][]byte) {
	attributes := func(id string) map[string][][]byte {
		return map[string][][]byte{"objectGUID": {windowsGUID(id)}, "uSNChanged": {[]byte(strconv.FormatInt(state.Revision, 10))}}
	}
	for _, user := range state.Users {
		if !bytes.Equal(windowsGUID(user.DirectoryID), guid) {
			continue
		}
		a := attributes(user.DirectoryID)
		flag := "512"
		if !user.Enabled {
			flag = "514"
		}
		a["userAccountControl"] = [][]byte{[]byte(flag)}
		a["accountExpires"] = [][]byte{[]byte("0")}
		return userDNProfile(user.DirectoryID, profile), a
	}
	for _, group := range state.Groups {
		if bytes.Equal(windowsGUID(group.ID), guid) {
			a := attributes(group.ID)
			a["member"] = [][]byte{}
			for _, member := range group.Members {
				a["member"] = append(a["member"], []byte(userDNProfile(member, profile)))
			}
			return "CN=" + group.ID + ",OU=Groups," + profile.baseDN, a
		}
	}
	return "", nil
}

func userDNProfile(id string, profile fixtureProfile) string {
	return "CN=" + id + ",OU=People," + profile.baseDN
}

func windowsGUID(id string) []byte {
	value, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(value) != 16 {
		return nil
	}
	slices.Reverse(value[:4])
	slices.Reverse(value[4:6])
	slices.Reverse(value[6:8])
	return value
}

func writeLDAPResult(writer io.Writer, id *ber.Packet, tag ber.Tag, code int) error {
	message := ber.NewSequence("")
	message.AppendChild(id)
	result := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tag, nil, "")
	result.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, ""))
	result.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	result.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	message.AppendChild(result)
	_, err := writer.Write(message.Bytes())
	return err
}

func readLDAPFrame(reader io.Reader) (*ber.Packet, error) {
	header := []byte{0, 0}
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	if header[0] != 0x30 {
		return nil, errors.New("fixture expects LDAP sequence")
	}
	length := int(header[1])
	if length&0x80 != 0 {
		n := length & 0x7f
		if n == 0 || n > 3 {
			return nil, errors.New("fixture LDAP length exceeds bound")
		}
		extra := make([]byte, n)
		if _, err := io.ReadFull(reader, extra); err != nil {
			return nil, err
		}
		header = append(header, extra...)
		length = 0
		for _, b := range extra {
			length = length<<8 | int(b)
		}
	}
	if length > 65536 {
		return nil, errors.New("fixture LDAP packet exceeds bound")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	if !validBERDepth(body, 0) {
		return nil, errors.New("fixture LDAP structure exceeds bound")
	}
	packet, err := ber.DecodePacketErr(append(header, body...))
	if err != nil {
		return nil, fmt.Errorf("decode fixture request: %w", err)
	}
	return packet, nil
}

func validBERDepth(data []byte, depth int) bool {
	if depth > 16 {
		return false
	}
	for len(data) > 0 {
		if len(data) < 2 || data[0]&31 == 31 {
			return false
		}
		constructed := data[0]&32 != 0
		header, length := 2, int(data[1])
		if length&128 != 0 {
			n := length & 127
			if n == 0 || n > 3 || len(data) < 2+n {
				return false
			}
			header += n
			length = 0
			for _, b := range data[2:header] {
				length = length<<8 | int(b)
			}
		}
		if length > len(data)-header {
			return false
		}
		if constructed && !validBERDepth(data[header:header+length], depth+1) {
			return false
		}
		data = data[header+length:]
	}
	return true
}
