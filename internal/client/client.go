// Package client sends a clipboard payload to a clipd daemon.
//
// Every invocation is a short-lived process: dial, send, wait for the
// acknowledgement, exit. There is no connection pooling, no retry loop and no
// background state, because on the sending machine clipd is an ordinary Unix
// filter that happens to end in a socket rather than a file descriptor.
package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/colefailla/clipd/internal/protocol"
	"github.com/colefailla/clipd/internal/transport"
)

// minThroughput mirrors the server's assumption so both ends agree on how
// long a large payload may reasonably take. See server.minThroughput.
const minThroughput = 256 << 10

// Kind classifies a copy failure. The CLI maps it to a process exit code so
// scripts can branch on $? instead of matching error text.
type Kind int

const (
	// KindConnect covers everything up to a usable connection: DNS, refused
	// connections, unreachable hosts, dial timeouts.
	KindConnect Kind = iota + 1

	// KindAuth means the daemon rejected the token.
	KindAuth

	// KindTooLarge means the payload exceeded a limit — the local one before
	// sending, or the server's.
	KindTooLarge

	// KindProtocol means the peer did not speak clipd correctly.
	KindProtocol

	// KindServer means the daemon accepted the request but failed to apply it.
	KindServer

	// KindTLS means the encrypted channel could not be established: a
	// fingerprint mismatch, an expired certificate, or a peer that is not
	// speaking TLS. Kept separate from KindAuth because the remedy differs —
	// a wrong fingerprint is fixed with `clipd configure -fingerprint`, a
	// wrong token with `-token`.
	KindTLS
)

func (k Kind) String() string {
	switch k {
	case KindConnect:
		return "connect"
	case KindAuth:
		return "auth"
	case KindTooLarge:
		return "payload"
	case KindProtocol:
		return "protocol"
	case KindServer:
		return "server"
	case KindTLS:
		return "tls"
	default:
		return "unknown"
	}
}

// Error is a classified copy failure.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func newError(kind Kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Err: fmt.Errorf(format, args...)}
}

// ErrTooLarge is the sentinel behind every payload-limit failure.
var ErrTooLarge = errors.New("payload exceeds the maximum size")

// Options configures a copy.
type Options struct {
	// Address is the daemon's host:port. clipd never interprets it further,
	// which is what keeps it transport-agnostic: a LAN IP, a .local name or a
	// VPN hostname all work without code changes.
	Address string

	// Token is the shared secret.
	Token string

	// TLS pins the server's public key. Required: there is no plaintext mode.
	TLS *tls.Config

	// Timeout bounds the dial, the handshake and the acknowledgement.
	Timeout time.Duration
}

// Result describes a successful copy.
type Result struct {
	// Bytes is the payload size that was accepted.
	Bytes int

	// Message is the daemon's human-readable acknowledgement, shown under
	// --verbose.
	Message string
}

// Copy sends payload to the daemon and waits for its acknowledgement.
//
// An empty payload is legal and clears the remote clipboard, matching what
// piping nothing into pbcopy does locally.
func Copy(ctx context.Context, opts Options, payload []byte) (Result, error) {
	if opts.Address == "" {
		return Result{}, newError(KindConnect, "no server address configured")
	}
	if opts.Token == "" {
		return Result{}, newError(KindAuth, "no token configured")
	}
	if opts.TLS == nil {
		return Result{}, newError(KindTLS, "no TLS configuration: a server fingerprint is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	// The context bounds the whole operation; the per-phase deadlines below
	// bound each step of it. Both exist because a context alone cannot
	// interrupt a blocked syscall on a net.Conn. The dial and the handshake
	// involve a handful of bytes each, so they get the flat timeout —
	// anything longer is a stall, not a slow link. Everything after the
	// handshake shares the size-scaled allowance.
	overall := timeout + payloadAllowance(len(payload))
	ctx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	dialer := net.Dialer{Timeout: timeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", opts.Address)
	if err != nil {
		return Result{}, newError(KindConnect, "connect to %s: %w", opts.Address, err)
	}
	defer rawConn.Close()

	if err := rawConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return Result{}, newError(KindConnect, "set deadline: %w", err)
	}

	// tls.Client plus an explicit handshake, rather than tls.Dial, so the
	// dial timeout and deadline handling above stay in one place and a
	// handshake failure stays distinguishable from an I/O failure.
	conn := tls.Client(rawConn, opts.TLS)
	if err := conn.HandshakeContext(ctx); err != nil {
		return Result{}, handshakeError(opts.Address, err)
	}

	// The handshake is done; one absolute deadline covers everything that
	// remains — body, flush and acknowledgement. The acknowledgement cannot
	// get its own flat window: Flush returning only means the last byte
	// entered the kernel's send buffer, and on a slow link the server may
	// still be receiving the body for most of the allowance after that.
	if err := conn.SetDeadline(time.Now().Add(overall)); err != nil {
		return Result{}, newError(KindConnect, "set deadline: %w", err)
	}

	// Buffering keeps the header out of its own packet, so a small copy
	// leaves as a single segment.
	w := bufio.NewWriter(conn)
	writeErr := protocol.WriteRequest(w, opts.Token, payload)
	if writeErr == nil {
		writeErr = w.Flush()
	}
	if writeErr != nil {
		// A rejected frame is a local fault; there is nothing to read back.
		if errors.Is(writeErr, protocol.ErrEmptyToken) || errors.Is(writeErr, protocol.ErrTokenTooLong) {
			return Result{}, &Error{Kind: KindAuth, Err: writeErr}
		}
		// Otherwise the write failed against the network, and the most common
		// reason is that the server rejected the request on the header alone
		// and closed before we finished sending the body. Its status frame is
		// already in our receive buffer, and it explains the failure far
		// better than "broken pipe" does.
		if status, message, err := protocol.ReadResponse(conn); err == nil && !status.OK() {
			return Result{}, statusError(status, message)
		}
		return Result{}, newError(KindConnect, "send payload to %s: %w", opts.Address, writeErr)
	}

	status, message, err := protocol.ReadResponse(conn)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// The daemon hung up without answering. The likeliest cause by far
			// is a mismatched token on a build that closed early, so say
			// something more useful than "unexpected EOF".
			return Result{}, newError(KindProtocol,
				"no acknowledgement from %s: the connection closed early (is the peer a clipd daemon?)", opts.Address)
		}
		return Result{}, newError(KindProtocol, "read acknowledgement from %s: %w", opts.Address, err)
	}

	if !status.OK() {
		return Result{}, statusError(status, message)
	}
	return Result{Bytes: len(payload), Message: message}, nil
}

// ReadInput reads all of r, refusing anything over max.
//
// It reads one byte past the limit to detect an oversized input, so an
// over-limit copy fails explicitly instead of silently arriving truncated.
// Truncation would corrupt the user's data in the least visible way possible.
func ReadInput(r io.Reader, max int64) ([]byte, error) {
	if max < 1 {
		return nil, fmt.Errorf("invalid maximum payload size %d", max)
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	if int64(len(data)) > max {
		return nil, &Error{
			Kind: KindTooLarge,
			Err:  fmt.Errorf("input is larger than the %d byte limit: %w (raise max_payload_bytes or CLIPD_MAX_PAYLOAD)", max, ErrTooLarge),
		}
	}
	return data, nil
}

func statusError(status protocol.Status, message string) *Error {
	detail := message
	if detail == "" {
		detail = status.String()
	}
	switch status {
	case protocol.StatusAuthFailed:
		return newError(KindAuth, "server rejected the token: %s", detail)
	case protocol.StatusPayloadTooLarge:
		return &Error{Kind: KindTooLarge, Err: fmt.Errorf("%s: %w", detail, ErrTooLarge)}
	case protocol.StatusMalformed:
		return newError(KindProtocol, "server rejected the request: %s", detail)
	case protocol.StatusInternalError:
		return newError(KindServer, "server error: %s", detail)
	default:
		return newError(KindProtocol, "unexpected server status: %s", detail)
	}
}

// handshakeError turns a TLS failure into something actionable.
func handshakeError(address string, err error) *Error {
	// A peer that answers a ClientHello with something that is not a TLS
	// record is almost always a clipd v1 daemon, which reads the handshake as
	// a malformed frame and replies in the clear. Go reports that as a typed
	// RecordHeaderError, so this does not depend on matching error text.
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return newError(KindTLS,
			"%s did not respond with TLS: it may be running clipd v1, which is unencrypted — upgrade the daemon on that machine", address)
	}

	var pinErr *transport.PinMismatchError
	if errors.As(err, &pinErr) {
		return newError(KindTLS,
			"%s presented an unexpected key: %w\nif you rotated the certificate, re-run `clipd configure -fingerprint`; if you did not, stop and investigate", address, pinErr)
	}

	var validityErr *transport.ValidityError
	if errors.As(err, &validityErr) {
		return newError(KindTLS, "%s: %w", address, validityErr)
	}

	return newError(KindTLS, "TLS handshake with %s failed: %w", address, err)
}

// payloadAllowance is the extra time granted for the body of a large payload.
func payloadAllowance(n int) time.Duration {
	return time.Duration(n/minThroughput) * time.Second
}
