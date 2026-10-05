package directory

import (
	"errors"
	"io"
	"net"
)

const (
	maximumDirectoryBytes  = 32 << 20
	maximumDirectoryPacket = 2 << 20
)

// boundedLDAPConnection validates bounded definite-length LDAP BER framing
// before the library can allocate/decode a response. It does not alter global
// BER package limits shared by other consumers. LDAP RFC4511 requires definite
// lengths; our requests need only ordinary single-octet tags and depth below16.
type boundedLDAPConnection struct {
	net.Conn
	remaining int
	pending   []byte
}

func (connection *boundedLDAPConnection) Read(output []byte) (int, error) {
	if len(output) == 0 {
		return 0, nil
	}
	if len(connection.pending) == 0 {
		frame, err := readDirectoryFrame(connection.Conn, connection.remaining)
		if err != nil {
			return 0, err
		}
		connection.remaining -= len(frame)
		connection.pending = frame
	}
	n := copy(output, connection.pending)
	connection.pending = connection.pending[n:]
	return n, nil
}

func readDirectoryFrame(reader io.Reader, budget int) ([]byte, error) {
	if budget < 2 {
		return nil, errors.New("directory response byte budget exhausted")
	}
	header := make([]byte, 2, 6)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	if header[0] != 0x30 {
		return nil, errors.New("directory response is not an LDAP message")
	}
	if header[1]&0x80 != 0 {
		count := int(header[1] & 0x7f)
		if count < 1 || count > 4 || budget < 2+count {
			return nil, errors.New("directory response length is invalid")
		}
		header = append(header, make([]byte, count)...)
		if _, err := io.ReadFull(reader, header[2:]); err != nil {
			return nil, err
		}
	}
	length, offset, err := directoryBERLength(header)
	if err != nil {
		return nil, err
	}
	if length > maximumDirectoryPacket-offset || length > budget-offset {
		return nil, errors.New("directory response exceeds byte limit")
	}
	frame := make([]byte, offset+length)
	copy(frame, header)
	if _, err = io.ReadFull(reader, frame[offset:]); err != nil {
		return nil, err
	}
	nodes := 0
	if validationErr := validateDirectoryBER(frame, 0, &nodes); validationErr != nil {
		return nil, validationErr
	}
	return frame, nil
}

func directoryBERLength(data []byte) (length, offset int, err error) {
	if len(data) < 2 || data[0]&0x1f == 0x1f {
		return 0, 0, errors.New("directory response tag is invalid")
	}
	if data[1]&0x80 == 0 {
		return int(data[1]), 2, nil
	}
	count := int(data[1] & 0x7f)
	if count < 1 || count > 4 || len(data) < 2+count {
		return 0, 0, errors.New("directory response length is invalid")
	}
	for _, value := range data[2 : 2+count] {
		if length > maximumDirectoryPacket>>8 {
			return 0, 0, errors.New("directory response exceeds packet bound")
		}
		length = length<<8 | int(value)
	}
	return length, 2 + count, nil
}

func validateDirectoryBER(data []byte, depth int, nodes *int) error {
	if depth > 16 {
		return errors.New("directory response nesting exceeds bound")
	}
	for len(data) > 0 {
		*nodes++
		if *nodes > 16384 {
			return errors.New("directory response element count exceeds bound")
		}
		length, offset, err := directoryBERLength(data)
		if err != nil {
			return err
		}
		if length > len(data)-offset {
			return errors.New("directory response crosses a packet boundary")
		}
		if data[0]&0x20 != 0 {
			if validationErr := validateDirectoryBER(data[offset:offset+length], depth+1, nodes); validationErr != nil {
				return validationErr
			}
		}
		data = data[offset+length:]
	}
	return nil
}
