package directory

import (
	"bytes"
	"io"
	"net"
	"testing"
)

func TestDirectoryResponseBounds(t *testing.T) {
	t.Parallel()
	valid := []byte{0x30, 0x05, 0x04, 0x03, 'a', 'b', 'c'}
	frame, err := readDirectoryFrame(bytes.NewReader(valid), len(valid))
	if err != nil || !bytes.Equal(frame, valid) {
		t.Fatalf("valid frame=%x,%v", frame, err)
	}
	for _, packet := range [][]byte{
		{0x30, 0x84, 0x7f, 0xff, 0xff, 0xff}, // Reject giant declaration before body allocation.
		{0x30, 0x80},                         // LDAP forbids indefinite lengths.
		{0x30, 0x02, 0x04, 0x7f},             // Primitive declaration crosses the bounded envelope.
		{0x30, 0x05, 0x04, 0x01, 'x'},        // Truncated frame.
	} {
		if _, readErr := readDirectoryFrame(bytes.NewReader(packet), maximumDirectoryBytes); readErr == nil {
			t.Fatalf("accepted unsafe frame=%x", packet)
		}
	}
	if _, err = readDirectoryFrame(bytes.NewReader(valid), len(valid)-1); err == nil {
		t.Fatal("connection byte limit ignored")
	}
	nested := []byte{0x04, 0}
	for range 18 {
		nested = append([]byte{0x30, byte(len(nested))}, nested...)
	}
	if _, err = readDirectoryFrame(bytes.NewReader(nested), maximumDirectoryBytes); err == nil {
		t.Fatal("deep BER nesting accepted")
	}
	connection := &boundedLDAPConnection{Conn: &readOnlyConnection{reader: bytes.NewReader(append(append([]byte{}, valid...), valid...))}, remaining: len(valid)}
	if _, err = io.ReadAll(connection); err == nil {
		t.Fatal("multiple packets exceeded total budget")
	}
}

type readOnlyConnection struct {
	net.Conn
	reader io.Reader
}

func (connection *readOnlyConnection) Read(data []byte) (int, error) {
	return connection.reader.Read(data)
}
