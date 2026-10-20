// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// MySQL/MariaDB client/server protocol subset used by the proxy: packet
// framing, the server greeting (Initial Handshake v10), ERR packets and the
// in-band TLS upgrade (SSLRequest). Reference: the MariaDB "Connection"
// protocol documentation (packets, Initial handshake packet, SSLRequest,
// Handshake response). Nothing here authenticates.

const (
	// maxHandshakePacket bounds packets read during the handshake phase.
	// Greetings and handshake responses (with connection attributes) are
	// far smaller; larger packets are rejected instead of buffered.
	maxHandshakePacket = 64 << 10
	// maxAuthRounds bounds auth-switch/auth-more-data exchanges.
	maxAuthRounds = 16

	capClientProtocol41 = 0x00000200
	capClientSSL        = 0x00000800

	// sslRequestLen is the payload length of an SSLRequest packet:
	// capabilities(4) max packet(4) charset(1) filler(23, of which MariaDB
	// uses the last 4 for extended capabilities).
	sslRequestLen = 32
)

// Error codes sent by the proxy itself.
const (
	erConCountError  = 1040 // ER_CON_COUNT_ERROR, "Too many connections"
	erUnknownError   = 1105 // ER_UNKNOWN_ERROR
	erHandshakeError = 1043 // ER_HANDSHAKE_ERROR, "Bad handshake"
)

var errProtocol = errors.New("mysql protocol error")

// readPacket reads one packet of at most max payload bytes. Multi-packet
// payloads (length 0xffffff) are not needed in the handshake and are
// rejected.
func readPacket(r io.Reader, max int) (seq byte, payload []byte, err error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	if n > max {
		return 0, nil, fmt.Errorf("%w: packet of %d bytes exceeds limit %d", errProtocol, n, max)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return h[3], payload, nil
}

// writePacket writes one packet; payload must be < 16 MiB.
func writePacket(w io.Writer, seq byte, payload []byte) error {
	if len(payload) >= 1<<24-1 {
		return fmt.Errorf("%w: payload too large", errProtocol)
	}
	b := make([]byte, 4+len(payload))
	b[0], b[1], b[2], b[3] = byte(len(payload)), byte(len(payload)>>8), byte(len(payload)>>16), seq
	copy(b[4:], payload)
	_, err := w.Write(b)
	return err
}

// errPayload builds an ERR packet payload. sqlState is included only when
// the client negotiated CLIENT_PROTOCOL_41; before the handshake the server
// (and the proxy) sends it without, as MariaDB does for "Too many
// connections".
func errPayload(code uint16, sqlState, msg string, proto41 bool) []byte {
	var b bytes.Buffer
	b.WriteByte(0xff)
	_ = binary.Write(&b, binary.LittleEndian, code)
	if proto41 {
		b.WriteByte('#')
		b.WriteString(sqlState)
	}
	b.WriteString(msg)
	return b.Bytes()
}

// greeting is the parsed part of an Initial Handshake v10 packet.
type greeting struct {
	ServerVersion string
	ConnectionID  uint32
	// capOffset is the offset of the lower two capability bytes.
	capOffset    int
	Capabilities uint16 // lower 16 bits only
}

// parseGreeting parses a server greeting. An ERR packet (0xff) is reported
// as an error carrying the server message.
func parseGreeting(p []byte) (greeting, error) {
	if len(p) == 0 {
		return greeting{}, fmt.Errorf("%w: empty greeting", errProtocol)
	}
	if p[0] == 0xff {
		code, msg := parseErr(p)
		return greeting{}, fmt.Errorf("server refused connection: error %d: %s", code, msg)
	}
	if p[0] != 10 {
		return greeting{}, fmt.Errorf("%w: unsupported protocol version %d", errProtocol, p[0])
	}
	nul := bytes.IndexByte(p[1:], 0)
	if nul < 0 {
		return greeting{}, fmt.Errorf("%w: unterminated server version", errProtocol)
	}
	g := greeting{ServerVersion: string(p[1 : 1+nul])}
	pos := 1 + nul + 1
	// connection id (4), auth-plugin-data part 1 (8), filler (1)
	if len(p) < pos+4+8+1+2 {
		return greeting{}, fmt.Errorf("%w: truncated greeting", errProtocol)
	}
	g.ConnectionID = binary.LittleEndian.Uint32(p[pos:])
	pos += 4 + 8 + 1
	g.capOffset = pos
	g.Capabilities = binary.LittleEndian.Uint16(p[pos:])
	return g, nil
}

// parseErr extracts code and message from an ERR payload, with or without
// a SQL state marker.
func parseErr(p []byte) (uint16, string) {
	if len(p) < 3 {
		return 0, ""
	}
	code := binary.LittleEndian.Uint16(p[1:3])
	msg := p[3:]
	if len(msg) >= 6 && msg[0] == '#' {
		msg = msg[6:]
	}
	return code, string(msg)
}

// sendInitialErr writes an ERR packet as the first packet (sequence 0)
// instead of a greeting, the way MariaDB rejects connections before the
// handshake. Errors are ignored: the connection is closed afterwards anyway.
func sendInitialErr(c net.Conn, code uint16, sqlState, msg string) {
	_ = writePacket(c, 0, errPayload(code, sqlState, msg, false))
}

// mysqlTLSUpgrade performs the proxy side of the in-band TLS upgrade:
//
//  1. read the backend greeting and forward it with CLIENT_SSL set;
//  2. read the client's first packet (sequence 1). An SSLRequest (32 bytes
//     with CLIENT_SSL) triggers a TLS server handshake on the client
//     connection; the client then sends its HandshakeResponse as sequence 2
//     inside TLS. The proxy forwards it to the plaintext backend as
//     sequence 1 with CLIENT_SSL cleared, because the backend never saw the
//     SSLRequest;
//  3. relay the authentication exchange, translating sequence numbers
//     (backend s -> client s+1, client c -> backend c-1) until the backend
//     sends OK (0x00) or ERR (0xff). After that every command restarts at
//     sequence 0, so the rest of the session is relayed as raw bytes.
//
// A client that does not request TLS is refused with an ERR packet unless
// allowPlain; with allowPlain its HandshakeResponse is forwarded unchanged.
//
// The returned connection is the one to relay the session on (the TLS
// connection or client itself). Deadlines are the caller's responsibility.
func mysqlTLSUpgrade(client, backend net.Conn, cfg *tls.Config, allowPlain bool) (net.Conn, error) {
	seq, p, err := readPacket(backend, maxHandshakePacket)
	if err != nil {
		return nil, fmt.Errorf("backend greeting: %w", err)
	}
	g, gerr := parseGreeting(p)
	if gerr != nil {
		if len(p) > 0 && p[0] == 0xff {
			_ = writePacket(client, seq, p) // pass the server's refusal on
		}
		return nil, fmt.Errorf("backend greeting: %w", gerr)
	}
	if seq != 0 {
		return nil, fmt.Errorf("%w: greeting sequence %d", errProtocol, seq)
	}
	binary.LittleEndian.PutUint16(p[g.capOffset:], g.Capabilities|capClientSSL)
	if err := writePacket(client, 0, p); err != nil {
		return nil, fmt.Errorf("client greeting: %w", err)
	}

	seq, p, err = readPacket(client, maxHandshakePacket)
	if err != nil {
		return nil, fmt.Errorf("client handshake: %w", err)
	}
	if seq != 1 || len(p) < 4 {
		return nil, fmt.Errorf("%w: unexpected client packet (sequence %d, %d bytes)", errProtocol, seq, len(p))
	}
	caps := binary.LittleEndian.Uint32(p)
	if caps&capClientProtocol41 == 0 {
		_ = writePacket(client, 2, errPayload(erHandshakeError, "08S01", "dbcm-proxy: pre-4.1 protocol not supported", false))
		return nil, fmt.Errorf("%w: client without CLIENT_PROTOCOL_41", errProtocol)
	}
	if caps&capClientSSL == 0 || len(p) != sslRequestLen {
		if caps&capClientSSL != 0 {
			return nil, fmt.Errorf("%w: CLIENT_SSL set in a full handshake response", errProtocol)
		}
		if !allowPlain {
			_ = writePacket(client, 2, errPayload(erHandshakeError, "08S01", "dbcm-proxy: TLS is required for this endpoint", true))
			return nil, errors.New("client did not request TLS")
		}
		if err := writePacket(backend, 1, p); err != nil {
			return nil, fmt.Errorf("backend handshake: %w", err)
		}
		return client, nil
	}

	tc := tls.Server(client, cfg)
	if err := tc.Handshake(); err != nil {
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	seq, p, err = readPacket(tc, maxHandshakePacket)
	if err != nil {
		return nil, fmt.Errorf("client handshake response: %w", err)
	}
	if seq != 2 || len(p) <= sslRequestLen {
		return nil, fmt.Errorf("%w: unexpected handshake response (sequence %d, %d bytes)", errProtocol, seq, len(p))
	}
	caps = binary.LittleEndian.Uint32(p)
	binary.LittleEndian.PutUint32(p, caps&^capClientSSL)
	if err := writePacket(backend, 1, p); err != nil {
		return nil, fmt.Errorf("backend handshake: %w", err)
	}

	for range maxAuthRounds {
		seq, p, err = readPacket(backend, maxHandshakePacket)
		if err != nil {
			return nil, fmt.Errorf("backend auth: %w", err)
		}
		if len(p) == 0 {
			return nil, fmt.Errorf("%w: empty auth packet", errProtocol)
		}
		if err := writePacket(tc, seq+1, p); err != nil {
			return nil, fmt.Errorf("client auth: %w", err)
		}
		switch p[0] {
		case 0x00: // OK: authenticated
			return tc, nil
		case 0xff: // ERR: authentication failed, already forwarded
			code, msg := parseErr(p)
			return nil, fmt.Errorf("backend rejected authentication: error %d: %s", code, msg)
		}
		if p[0] == 0x01 && len(p) == 2 && p[1] == 0x03 {
			// caching_sha2_password fast-auth success: OK follows without a
			// client packet.
			continue
		}
		cseq, cp, err := readPacket(tc, maxHandshakePacket)
		if err != nil {
			return nil, fmt.Errorf("client auth: %w", err)
		}
		if cseq == 0 {
			return nil, fmt.Errorf("%w: client auth sequence 0", errProtocol)
		}
		if err := writePacket(backend, cseq-1, cp); err != nil {
			return nil, fmt.Errorf("backend auth: %w", err)
		}
	}
	return nil, fmt.Errorf("%w: more than %d authentication rounds", errProtocol, maxAuthRounds)
}
