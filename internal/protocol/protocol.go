// Package protocol implements the clipd wire format.
//
// The protocol is a single request/response round trip over a stream
// connection. A client sends one request frame containing the shared token
// and the clipboard payload; the server replies with one status frame and
// both sides close.
//
// Request frame (client -> server):
//
//	[4]  magic        "CLPD"
//	[1]  version      0x01
//	[1]  token_len    N (1..255)
//	[N]  token
//	[8]  payload_len  uint64, big-endian
//	[..] payload      payload_len bytes, verbatim
//
// Response frame (server -> client):
//
//	[1]  status       see the Status constants
//	[2]  message_len  uint16, big-endian
//	[..] message      human-readable, for diagnostics only
//
// There is no separate authentication handshake. The token travels in the
// same frame as the payload because the frame only ever crosses the wire
// inside TLS 1.3 with the server's key pinned: the channel is confidential
// and the server is authenticated before the first byte of it is written, so
// an extra challenge-response round trip would buy nothing. Keeping it to
// one frame keeps both sides trivial to reason about.
//
// This package deliberately contains no networking policy: no dials, no
// deadlines, no retries. It only reads and writes frames from io.Reader and
// io.Writer, which keeps it exhaustively testable and lets the server stage
// its reads so it can reject an oversized payload before allocating for it.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Magic is the first four bytes of every request frame. It exists so that a
// stray connection (a port scanner, a browser, a mistyped host) is rejected
// immediately instead of being interpreted as a malformed token.
var Magic = [4]byte{'C', 'L', 'P', 'D'}

const (
	// Version1 is the only wire version in existence. The field exists so a
	// future revision can be introduced without a flag day: a server can
	// recognise an unknown version and say so instead of misparsing.
	Version1 byte = 0x01

	// CurrentVersion is the version this build emits.
	CurrentVersion = Version1

	// MaxTokenLen is dictated by the single-byte token_len field.
	MaxTokenLen = 255

	// MaxMessageLen is dictated by the uint16 message_len field.
	MaxMessageLen = math.MaxUint16

	// PrologueSize covers magic, version and token_len: everything needed
	// before the variable-length token can be read.
	PrologueSize = 4 + 1 + 1

	// PayloadLenSize is the width of the payload_len field.
	PayloadLenSize = 8

	// ResponseHeaderSize covers status and message_len.
	ResponseHeaderSize = 1 + 2
)

// Status is the result code in a response frame.
type Status byte

const (
	StatusOK              Status = 0x00
	StatusAuthFailed      Status = 0x01
	StatusPayloadTooLarge Status = 0x02
	StatusMalformed       Status = 0x03
	StatusInternalError   Status = 0x04
)

// OK reports whether the status indicates a successful copy.
func (s Status) OK() bool { return s == StatusOK }

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusAuthFailed:
		return "authentication failed"
	case StatusPayloadTooLarge:
		return "payload too large"
	case StatusMalformed:
		return "malformed request"
	case StatusInternalError:
		return "internal server error"
	default:
		return fmt.Sprintf("unknown status 0x%02x", byte(s))
	}
}

// Framing errors. Callers use errors.Is to distinguish a peer that is not
// speaking clipd from one that is speaking it badly.
var (
	ErrBadMagic           = errors.New("protocol: not a clipd request")
	ErrUnsupportedVersion = errors.New("protocol: unsupported version")
	ErrEmptyToken         = errors.New("protocol: empty token")
	ErrTokenTooLong       = errors.New("protocol: token too long")
	ErrMessageTooLong     = errors.New("protocol: message too long")
)

// Request is the authenticated prologue of a request frame: everything up to
// but not including payload_len.
type Request struct {
	Version byte
	Token   []byte
}

// WriteRequest writes a complete request frame.
//
// The caller is expected to hand this a buffered writer wrapping the
// connection and to flush afterwards; the header is written as one slice so
// that a buffered writer emits header and payload in as few segments as the
// network stack allows.
func WriteRequest(w io.Writer, token string, payload []byte) error {
	switch {
	case len(token) == 0:
		return ErrEmptyToken
	case len(token) > MaxTokenLen:
		return fmt.Errorf("%w: %d bytes (max %d)", ErrTokenTooLong, len(token), MaxTokenLen)
	}

	header := make([]byte, 0, PrologueSize+len(token)+PayloadLenSize)
	header = append(header, Magic[:]...)
	header = append(header, CurrentVersion, byte(len(token)))
	header = append(header, token...)
	header = binary.BigEndian.AppendUint64(header, uint64(len(payload)))

	// io.Writer's contract requires Write to report an error on a short
	// write, so a single call per slice is sufficient here; the partial-write
	// hazard in this protocol lives on the read side.
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("write request header: %w", err)
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("write payload: %w", err)
		}
	}
	return nil
}

// ReadPrologue reads magic, version and the token.
//
// It stops before payload_len so the caller can authenticate first and drop
// an unauthorised connection without reading anything it did not ask for.
func ReadPrologue(r io.Reader) (*Request, error) {
	buf := make([]byte, PrologueSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read prologue: %w", err)
	}
	if [4]byte(buf[0:4]) != Magic {
		return nil, ErrBadMagic
	}

	version := buf[4]
	if version != Version1 {
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnsupportedVersion, version)
	}

	tokenLen := int(buf[5])
	if tokenLen == 0 {
		return nil, ErrEmptyToken
	}
	token := make([]byte, tokenLen)
	if _, err := io.ReadFull(r, token); err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	return &Request{Version: version, Token: token}, nil
}

// ReadPayloadLen reads the declared payload length.
//
// The value is attacker-controlled: callers must compare it against their
// configured limit before allocating a buffer for it.
func ReadPayloadLen(r io.Reader) (uint64, error) {
	buf := make([]byte, PayloadLenSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("read payload length: %w", err)
	}
	return binary.BigEndian.Uint64(buf), nil
}

// ReadPayload reads exactly n bytes.
//
// n must already have been validated against the caller's maximum; this
// function allocates n bytes up front and will happily do so for any n that
// fits in memory.
func ReadPayload(r io.Reader, n uint64) ([]byte, error) {
	if n == 0 {
		return []byte{}, nil
	}
	if n > math.MaxInt {
		return nil, fmt.Errorf("read payload: length %d exceeds addressable memory", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}
	return payload, nil
}

// WriteResponse writes a status frame. An over-long message is truncated
// rather than rejected: a diagnostic string is never worth failing a copy
// that already succeeded.
func WriteResponse(w io.Writer, status Status, message string) error {
	if len(message) > MaxMessageLen {
		message = message[:MaxMessageLen]
	}
	frame := make([]byte, 0, ResponseHeaderSize+len(message))
	frame = append(frame, byte(status))
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(message)))
	frame = append(frame, message...)

	if _, err := w.Write(frame); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

// ReadResponse reads a status frame.
func ReadResponse(r io.Reader) (Status, string, error) {
	header := make([]byte, ResponseHeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, "", fmt.Errorf("read response: %w", err)
	}
	status := Status(header[0])
	messageLen := binary.BigEndian.Uint16(header[1:3])
	if messageLen == 0 {
		return status, "", nil
	}
	message := make([]byte, messageLen)
	if _, err := io.ReadFull(r, message); err != nil {
		return status, "", fmt.Errorf("read response message: %w", err)
	}
	return status, string(message), nil
}
