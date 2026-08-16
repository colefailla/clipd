package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/protocol"
	"github.com/colefailla/clipd/internal/transport"
)

const testToken = "test-token-of-sufficient-length"

// harness is a server listening on a loopback port, exercised over real TLS
// on real TCP so the tests cover the handshake, framing, deadlines and
// connection handling the daemon actually uses in production.
type harness struct {
	t    *testing.T
	addr string
	clip *clipboard.Fake
	pin  []byte
	// srv is exposed so tests can assert on internal admission bookkeeping,
	// which has no observable effect on the wire until it starts rejecting.
	srv *Server
}

type harnessOption func(*Options)

func withMaxPayload(n int64) harnessOption { return func(o *Options) { o.MaxPayload = n } }
func withTimeout(d time.Duration) harnessOption {
	return func(o *Options) { o.Timeout = d }
}
func withClipboardError(err error) harnessOption {
	return func(o *Options) { o.Clipboard.(*clipboard.Fake).Err = err }
}
func withMaxConcurrent(n int) harnessOption { return func(o *Options) { o.MaxConcurrent = n } }
func withMaxMemory(n int64) harnessOption   { return func(o *Options) { o.MaxMemory = n } }

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()

	tlsConfig, pin, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate test certificate: %v", err)
	}

	clip := &clipboard.Fake{}
	options := Options{
		Token:      testToken,
		TLS:        tlsConfig,
		Clipboard:  clip,
		MaxPayload: 1 << 20,
		Timeout:    2 * time.Second,
	}
	for _, opt := range opts {
		opt(&options)
	}

	srv, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Port 0 lets the kernel pick, so tests never collide on a fixed port.
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v, want nil on shutdown", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after cancellation")
		}
	})

	return &harness{t: t, addr: ln.Addr().String(), clip: clip, pin: pin, srv: srv}
}

// send performs a full client exchange and returns the server's response.
func (h *harness) send(token string, payload []byte) (protocol.Status, string) {
	h.t.Helper()

	conn := h.dial()
	defer conn.Close()

	if err := protocol.WriteRequest(conn, token, payload); err != nil {
		h.t.Fatalf("WriteRequest: %v", err)
	}
	status, message, err := protocol.ReadResponse(conn)
	if err != nil {
		h.t.Fatalf("ReadResponse: %v", err)
	}
	return status, message
}

// sendRaw writes arbitrary bytes and reads whatever comes back.
func (h *harness) sendRaw(frame []byte) (protocol.Status, string, error) {
	h.t.Helper()

	conn := h.dial()
	defer conn.Close()

	if _, err := conn.Write(frame); err != nil {
		h.t.Fatalf("write: %v", err)
	}
	return readResponse(conn)
}

// dial opens a connection and completes the TLS handshake, so tests operate
// on the same encrypted stream a real client uses.
func (h *harness) dial() net.Conn {
	h.t.Helper()
	return dialTLS(h.t, h.addr, h.pin, 10*time.Second)
}

// dialPlaintext connects without TLS, for testing what a v1 client sees.
func (h *harness) dialPlaintext() net.Conn {
	h.t.Helper()
	conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		h.t.Fatalf("set deadline: %v", err)
	}
	return conn
}

func dialTLS(t *testing.T, addr string, pin []byte, deadline time.Duration) net.Conn {
	t.Helper()
	conn, err := tryDialTLS(addr, pin, deadline)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return conn
}

// tryDialTLS is dialTLS returning an error instead of failing the test, for
// goroutines other than the one running the test function — t.Fatalf is
// documented as unusable from those.
func tryDialTLS(addr string, pin []byte, deadline time.Duration) (net.Conn, error) {
	rawConn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	if err := rawConn.SetDeadline(time.Now().Add(deadline)); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	clientConfig, err := transport.ClientConfig(pin)
	if err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("client config: %w", err)
	}
	conn := tls.Client(rawConn, clientConfig)
	if err := conn.HandshakeContext(context.Background()); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	return conn, nil
}

func readResponse(conn net.Conn) (protocol.Status, string, error) {
	status, message, err := protocol.ReadResponse(conn)
	return status, message, err
}

func TestCopySucceeds(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	payload := []byte("total 8\n-rw-r--r--  1 user  staff  0 Jan  1 00:00 file\n")

	status, message := h.send(testToken, payload)
	if !status.OK() {
		t.Fatalf("status = %v (%s), want ok", status, message)
	}
	if got := h.clip.Data(); !bytes.Equal(got, payload) {
		t.Errorf("clipboard = %q, want %q", got, payload)
	}
	if !strings.Contains(message, "copied") {
		t.Errorf("message = %q, want an acknowledgement mentioning the byte count", message)
	}
}

// TestPayloadIsPreservedVerbatim is the promise the README makes: whatever
// bytes go in come out, with no trimming, re-encoding or newline fixups.
func TestPayloadIsPreservedVerbatim(t *testing.T) {
	t.Parallel()

	payloads := map[string][]byte{
		"trailing newline":    []byte("docker ps output\n"),
		"no trailing newline": []byte("no newline here"),
		"multiple newlines":   []byte("a\n\n\nb\n"),
		"leading whitespace":  []byte("   indented\n\tby a tab\n"),
		"crlf":                []byte("windows\r\nline\r\nendings\r\n"),
		"nul bytes":           {'a', 0x00, 'b', 0x00},
		"invalid utf-8":       {0xff, 0xfe, 0x80},
		"ansi escapes":        []byte("\x1b[31mred\x1b[0m\n"),
		"empty":               {},
	}

	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			status, message := h.send(testToken, payload)
			if !status.OK() {
				t.Fatalf("status = %v (%s)", status, message)
			}
			if got := h.clip.Data(); !bytes.Equal(got, payload) {
				t.Errorf("clipboard = %q, want %q", got, payload)
			}
		})
	}
}

func TestBadTokenIsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
	}{
		{"wrong token", "not-the-right-token-at-all"},
		{"prefix of the real token", testToken[:10]},
		{"real token with a suffix", testToken + "x"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			status, _ := h.send(tc.token, []byte("secret data"))
			if status != protocol.StatusAuthFailed {
				t.Errorf("status = %v, want auth failed", status)
			}
			if h.clip.WriteCount() != 0 {
				t.Error("an unauthenticated request reached the clipboard")
			}
		})
	}
}

func TestPayloadAtLimitIsAccepted(t *testing.T) {
	t.Parallel()

	const limit = 4096
	h := newHarness(t, withMaxPayload(limit))

	payload := bytes.Repeat([]byte("x"), limit)
	status, message := h.send(testToken, payload)
	if !status.OK() {
		t.Fatalf("status = %v (%s), want a payload of exactly the limit to be accepted", status, message)
	}
	if len(h.clip.Data()) != limit {
		t.Errorf("clipboard holds %d bytes, want %d", len(h.clip.Data()), limit)
	}
}

func TestOversizedPayloadIsRejected(t *testing.T) {
	t.Parallel()

	const limit = 4096
	h := newHarness(t, withMaxPayload(limit))

	status, message := h.send(testToken, bytes.Repeat([]byte("x"), limit+1))
	if status != protocol.StatusPayloadTooLarge {
		t.Fatalf("status = %v (%s), want payload too large", status, message)
	}
	if h.clip.WriteCount() != 0 {
		t.Error("an oversized payload reached the clipboard")
	}
	if !strings.Contains(message, "4096") {
		t.Errorf("message = %q, want it to state the limit", message)
	}
}

// TestOversizedPayloadIsRejectedBeforeTheBody is the memory-safety case: the
// declared length is attacker-controlled, so the server must refuse on the
// header alone rather than allocating for a body it will discard. The client
// here declares a terabyte and sends nothing.
func TestOversizedPayloadIsRejectedBeforeTheBody(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withMaxPayload(1024))

	frame := append([]byte{}, protocol.Magic[:]...)
	frame = append(frame, protocol.CurrentVersion, byte(len(testToken)))
	frame = append(frame, testToken...)
	frame = binary.BigEndian.AppendUint64(frame, 1<<40)

	start := time.Now()
	status, _, err := h.sendRaw(frame)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if status != protocol.StatusPayloadTooLarge {
		t.Errorf("status = %v, want payload too large", status)
	}
	// The server answered without waiting for a body that was never coming.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("server took %s to reject; it appears to have waited for the body", elapsed)
	}
	if h.clip.WriteCount() != 0 {
		t.Error("the clipboard was touched")
	}
}

func TestMalformedRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		frame []byte
	}{
		{
			name:  "not clipd at all",
			frame: []byte("GET / HTTP/1.1\r\nHost: mac\r\n\r\n"),
		},
		{
			name: "unsupported version",
			frame: func() []byte {
				f := append([]byte{}, protocol.Magic[:]...)
				return append(f, 0x99, 0x04, 't', 'o', 'k', 'n')
			}(),
		},
		{
			name: "zero-length token",
			frame: func() []byte {
				f := append([]byte{}, protocol.Magic[:]...)
				return append(f, protocol.CurrentVersion, 0x00)
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			status, _, err := h.sendRaw(tc.frame)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			if status != protocol.StatusMalformed {
				t.Errorf("status = %v, want malformed", status)
			}
			if h.clip.WriteCount() != 0 {
				t.Error("a malformed request reached the clipboard")
			}
		})
	}
}

// TestTruncatedPayloadIsRejected covers a client that declares more than it
// sends and then hangs up.
func TestTruncatedPayloadIsRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	frame := append([]byte{}, protocol.Magic[:]...)
	frame = append(frame, protocol.CurrentVersion, byte(len(testToken)))
	frame = append(frame, testToken...)
	frame = binary.BigEndian.AppendUint64(frame, 100)
	frame = append(frame, []byte("only twelve!")...)

	conn := h.dial()
	defer conn.Close()
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Half-close so the server sees EOF rather than waiting out its deadline.
	// Both *net.TCPConn and *tls.Conn provide this; the interface avoids
	// caring which one the harness handed back.
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := conn.(closeWriter); ok {
		if err := cw.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	} else {
		t.Fatal("connection does not support a half close")
	}

	status, _, err := readResponse(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if status != protocol.StatusMalformed {
		t.Errorf("status = %v, want malformed", status)
	}
	if h.clip.WriteCount() != 0 {
		t.Error("a truncated payload reached the clipboard")
	}
}

// TestSlowClientIsServed proves the reads are assembled correctly when the
// frame arrives in many small pieces rather than one packet.
func TestSlowClientIsServed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	payload := []byte("dribbled out one byte at a time\n")

	var frame bytes.Buffer
	if err := protocol.WriteRequest(&frame, testToken, payload); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}

	conn := h.dial()
	defer conn.Close()

	for _, b := range frame.Bytes() {
		if _, err := conn.Write([]byte{b}); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	status, message, err := readResponse(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !status.OK() {
		t.Fatalf("status = %v (%s)", status, message)
	}
	if got := h.clip.Data(); !bytes.Equal(got, payload) {
		t.Errorf("clipboard = %q, want %q", got, payload)
	}
}

// TestIdleConnectionIsClosed covers the resource-exhaustion case: a client
// that connects and then says nothing must not hold a handler forever.
func TestIdleConnectionIsClosed(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withTimeout(300*time.Millisecond))

	conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// Read until the server gives up on us. Without a deadline on the server
	// side this blocks until the test times out.
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Error("the server responded to a client that sent nothing")
	}
}

// TestBareConnectIsHarmless covers what `clipd status` does when probing
// reachability: connect, then hang up without sending anything.
func TestBareConnectIsHarmless(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The daemon must still be serving afterwards.
	status, message := h.send(testToken, []byte("still alive\n"))
	if !status.OK() {
		t.Fatalf("status = %v (%s) after a bare connect", status, message)
	}
}

// TestPlaintextClientGetsAnExplanation covers the most likely upgrade
// failure: a v1 client, which speaks the frame protocol directly, connecting
// to a v2 daemon. Without the magic sniff this is a bare connection close and
// an unexplained hang.
func TestPlaintextClientGetsAnExplanation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	conn := h.dialPlaintext()
	defer conn.Close()

	// Exactly what clipd v1 puts on the wire.
	if err := protocol.WriteRequest(conn, testToken, []byte("v1 payload")); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}

	status, message, err := protocol.ReadResponse(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if status != protocol.StatusMalformed {
		t.Errorf("status = %v, want malformed", status)
	}
	if !strings.Contains(strings.ToLower(message), "tls") {
		t.Errorf("message = %q, want it to explain that TLS is required", message)
	}
	if h.clip.WriteCount() != 0 {
		t.Error("an unencrypted request reached the clipboard")
	}
}

// TestWrongPinIsRejected proves the client verifies the server's identity: a
// pin for a different key must abort before any secret is sent.
func TestWrongPinIsRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// A pin belonging to some other server entirely.
	_, otherPin, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate second certificate: %v", err)
	}

	rawConn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()
	_ = rawConn.SetDeadline(time.Now().Add(10 * time.Second))

	clientConfig, err := transport.ClientConfig(otherPin)
	if err != nil {
		t.Fatalf("client config: %v", err)
	}
	conn := tls.Client(rawConn, clientConfig)
	err = conn.HandshakeContext(context.Background())
	if err == nil {
		t.Fatal("handshake succeeded against a server with a different key")
	}

	var mismatch *transport.PinMismatchError
	if !errors.As(err, &mismatch) {
		t.Errorf("error = %v (%T), want a PinMismatchError", err, err)
	}
	if h.clip.WriteCount() != 0 {
		t.Error("the clipboard was touched despite a failed handshake")
	}
}

// TestRejectionSurvivesAnInFlightBody pins the drain behaviour: when the
// server rejects on the header alone while the client is still streaming a
// large body, the status frame must still reach the client. Without the
// drain, closing with unread bytes aborts the connection with RST, which can
// destroy the response before the client reads it.
func TestRejectionSurvivesAnInFlightBody(t *testing.T) {
	t.Parallel()

	// Larger than any loopback socket buffer, so the client genuinely blocks
	// mid-write while the server rejects.
	body := bytes.Repeat([]byte("x"), 4<<20)

	tests := []struct {
		name  string
		token string
		want  protocol.Status
	}{
		{"auth failure", "not-the-right-token-at-all", protocol.StatusAuthFailed},
		{"payload too large", testToken, protocol.StatusPayloadTooLarge},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, withMaxPayload(1024))

			conn := h.dial()
			defer conn.Close()

			// WriteRequest sends header and body in one call; the server
			// rejects on the header and must drain the rest.
			_ = protocol.WriteRequest(conn, tc.token, body)

			status, _, err := protocol.ReadResponse(conn)
			if err != nil {
				t.Fatalf("the rejection was lost: %v", err)
			}
			if status != tc.want {
				t.Errorf("status = %v, want %v", status, tc.want)
			}
			if h.clip.WriteCount() != 0 {
				t.Error("a rejected request reached the clipboard")
			}
		})
	}
}

func TestClipboardFailureIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withClipboardError(errors.New("pasteboard unavailable")))

	status, message := h.send(testToken, []byte("data"))
	if status != protocol.StatusInternalError {
		t.Errorf("status = %v, want internal error", status)
	}
	// The client is told the operation failed, not the internals of why.
	if strings.Contains(message, "pasteboard unavailable") {
		t.Errorf("message leaked backend detail: %q", message)
	}
}

func TestConcurrentCopies(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withMaxConcurrent(4))

	const clients = 24
	var wg sync.WaitGroup
	errs := make(chan error, clients)

	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := tryDialTLS(h.addr, h.pin, 20*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			if err := protocol.WriteRequest(conn, testToken, []byte{byte(i)}); err != nil {
				errs <- err
				return
			}
			status, message, err := protocol.ReadResponse(conn)
			if err != nil {
				errs <- err
				return
			}
			if !status.OK() {
				errs <- errors.New(message)
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent copy failed: %v", err)
	}

	if h.clip.WriteCount() != clients {
		t.Errorf("clipboard writes = %d, want %d", h.clip.WriteCount(), clients)
	}
}

func TestShutdownStopsAccepting(t *testing.T) {
	t.Parallel()

	tlsConfig, _, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate test certificate: %v", err)
	}
	srv, err := New(Options{
		Token:      testToken,
		TLS:        tlsConfig,
		Clipboard:  &clipboard.Fake{},
		MaxPayload: 1 << 20,
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	// Confirm it is up before shutting it down.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial before shutdown: %v", err)
	}
	conn.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve after cancellation = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}

	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Error("the listener still accepts connections after shutdown")
	}
}

func TestShutdownMethodIsIdempotent(t *testing.T) {
	t.Parallel()

	tlsConfig, _, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate test certificate: %v", err)
	}
	srv, err := New(Options{
		Token:      testToken,
		TLS:        tlsConfig,
		Clipboard:  &clipboard.Fake{},
		MaxPayload: 1 << 20,
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve(context.Background(), ln) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("first Shutdown: %v", err)
	}
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()

	valid := func() Options {
		return Options{
			Token:      testToken,
			Clipboard:  &clipboard.Fake{},
			MaxPayload: 1 << 20,
			Timeout:    time.Second,
		}
	}

	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{"no token", func(o *Options) { o.Token = "" }},
		{"short token", func(o *Options) { o.Token = "short" }},
		{"no clipboard", func(o *Options) { o.Clipboard = nil }},
		{"zero payload limit", func(o *Options) { o.MaxPayload = 0 }},
		{"negative payload limit", func(o *Options) { o.MaxPayload = -1 }},
		{"zero timeout", func(o *Options) { o.Timeout = 0 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := valid()
			tc.mutate(&opts)
			if _, err := New(opts); err == nil {
				t.Error("New accepted invalid options")
			}
		})
	}
}

// TestPayloadTimeoutScalesWithSize checks the deadline policy directly: a
// large payload on a slow link must not be cut off at the handshake timeout.
func TestPayloadTimeoutScalesWithSize(t *testing.T) {
	t.Parallel()

	tlsConfig, _, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate test certificate: %v", err)
	}
	srv, err := New(Options{
		Token:      testToken,
		TLS:        tlsConfig,
		Clipboard:  &clipboard.Fake{},
		MaxPayload: 100 << 20,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	small := srv.payloadTimeout(1024)
	if small != 5*time.Second {
		t.Errorf("timeout for a small payload = %s, want the base 5s", small)
	}
	large := srv.payloadTimeout(10 << 20)
	if large <= small {
		t.Errorf("timeout for 10 MiB (%s) is not greater than for 1 KiB (%s)", large, small)
	}
}

// TestSilentConnectionsCannotStarveTheServer is the regression test for the
// slot-exhaustion denial of service: peers that completed a TCP handshake and
// then sent nothing used to occupy the entire connection budget until their
// deadlines expired, stalling real copies for the whole timeout — longer than a
// client's own deadline, so copies did not merely slow down, they failed.
//
// The flood here is the size of the old budget. Everything runs from 127.0.0.1,
// so this exercises the enlarged pool rather than per-host rationing; that
// rationing is tested in TestAdmissionRationsOnlyUnderPressure, which needs
// distinct source addresses that loopback cannot portably provide.
func TestSilentConnectionsCannotStarveTheServer(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withTimeout(3*time.Second))

	const hogs = 16
	for range hogs {
		conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
	}
	// Let the server accept them all before measuring.
	time.Sleep(300 * time.Millisecond)

	// A real copy must still go through promptly rather than waiting out the
	// hogs' deadlines.
	start := time.Now()
	status, message := h.send(testToken, []byte("still working"))
	elapsed := time.Since(start)

	if !status.OK() {
		t.Fatalf("copy during connection flood returned %v: %s", status, message)
	}
	if elapsed > time.Second {
		t.Errorf("copy took %v while %d silent connections were held; the server is being starved",
			elapsed, hogs)
	}
	if got := string(h.clip.Data()); got != "still working" {
		t.Errorf("clipboard = %q, want %q", got, "still working")
	}
}

// TestAdmissionRationsOnlyUnderPressure tests the per-host limiter directly.
//
// Driving it over real sockets would need many distinct source addresses, which
// loopback does not offer portably — macOS has only 127.0.0.1 unless an alias is
// configured — and a test that flooded from one address could not tell a
// working limiter from a broken one, since victim and attacker would share a
// host key.
func TestAdmissionRationsOnlyUnderPressure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	s := h.srv

	// Below the soft limit, one host may hold far more than its rationed
	// share: an unattacked daemon throttles nobody.
	for i := range unauthSoftLimit {
		if !s.admit("10.0.0.1") {
			t.Fatalf("connection %d from a single host was rationed below the soft limit", i)
		}
	}

	// At the soft limit that host is over its share and gets refused.
	if s.admit("10.0.0.1") {
		t.Error("a host already holding its share was admitted under pressure")
	}

	// A host that is not hoarding still gets in, which is the whole point:
	// pressure from one peer must not lock everyone else out.
	if !s.admit("10.0.0.2") {
		t.Error("an innocent host was refused because another host was flooding")
	}

	// Releasing back below the soft limit lifts rationing again.
	for range unauthSoftLimit {
		s.release("10.0.0.1")
	}
	if !s.admit("10.0.0.1") {
		t.Error("rationing did not lift after the flood drained")
	}

	s.release("10.0.0.1")
	s.release("10.0.0.2")
	s.release("10.0.0.1")

	s.unauthMu.Lock()
	defer s.unauthMu.Unlock()
	if s.unauthTotal != 0 || len(s.unauth) != 0 {
		t.Errorf("after releasing everything: unauthTotal = %d, %d host entries; want 0 and 0",
			s.unauthTotal, len(s.unauth))
	}
}

// TestParallelCopiesFromOneHostAreNotRationed guards the other side of the
// trade: admission control must not throttle a legitimate burst, which is the
// failure mode of a flat per-host cap.
func TestParallelCopiesFromOneHostAreNotRationed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	const clients = unauthSoftLimit - 4
	var wg sync.WaitGroup
	errs := make(chan error, clients)

	// Released together so they contend in the pre-authentication phase at
	// once, which is exactly what a flat cap would reject.
	start := make(chan struct{})
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			conn := dialTLS(t, h.addr, h.pin, 20*time.Second)
			defer conn.Close()
			if err := protocol.WriteRequest(conn, testToken, []byte("x")); err != nil {
				errs <- err
				return
			}
			status, message, err := protocol.ReadResponse(conn)
			if err != nil {
				errs <- err
				return
			}
			if !status.OK() {
				errs <- errors.New(message)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("parallel copy from one host was rejected: %v", err)
	}
	if h.clip.WriteCount() != clients {
		t.Errorf("clipboard writes = %d, want %d", h.clip.WriteCount(), clients)
	}
}

// TestAuthenticationReleasesTheAdmissionSlot checks the bookkeeping directly:
// a proven client must not still be counted against its host's share, or a
// long copy would ration the next one.
func TestAuthenticationReleasesTheAdmissionSlot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if _, message := h.send(testToken, []byte("done")); message == "" {
		t.Fatal("expected an acknowledgement")
	}

	// Give the handler a moment to return, then confirm nothing is still held.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.srv.unauthMu.Lock()
		total, entries := h.srv.unauthTotal, len(h.srv.unauth)
		h.srv.unauthMu.Unlock()
		if total == 0 && entries == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	h.srv.unauthMu.Lock()
	defer h.srv.unauthMu.Unlock()
	t.Errorf("after a completed copy, unauthTotal = %d and %d host entries remain, want 0 and 0",
		h.srv.unauthTotal, len(h.srv.unauth))
}

func TestWarnLimiterBoundsOutputAndReportsDrops(t *testing.T) {
	t.Parallel()

	var l warnLimiter
	now := time.Now()

	for i := range warnBudget {
		ok, dropped := l.allow(now)
		if !ok {
			t.Fatalf("warning %d was suppressed while still within the budget", i)
		}
		if dropped != 0 {
			t.Fatalf("warning %d reported %d drops before any window rolled", i, dropped)
		}
	}

	// Everything past the budget is dropped rather than logged.
	const excess = 500
	for range excess {
		if ok, _ := l.allow(now); ok {
			t.Fatal("a warning was logged after the budget was spent")
		}
	}

	// The next window reports what the previous one swallowed, so throttling
	// hides the volume of an attack but never the fact of one.
	ok, dropped := l.allow(now.Add(warnWindow))
	if !ok {
		t.Error("the first warning of a new window was suppressed")
	}
	if dropped != excess {
		t.Errorf("dropped = %d, want %d", dropped, excess)
	}
}

// TestCopiesQueueWithinTheMemoryBudget checks that a budget smaller than the
// concurrent demand slows copies down rather than failing them: the daemon's
// memory ceiling stops being connections times payload, without turning a
// burst into errors.
func TestCopiesQueueWithinTheMemoryBudget(t *testing.T) {
	t.Parallel()

	const payload = 64 << 10
	// Room for two payloads at a time, against eight arriving at once.
	h := newHarness(t, withMaxPayload(payload), withMaxMemory(2*payload), withTimeout(10*time.Second))

	const clients = 8
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	body := bytes.Repeat([]byte("x"), payload)

	start := make(chan struct{})
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			conn := dialTLS(t, h.addr, h.pin, 30*time.Second)
			defer conn.Close()
			if err := protocol.WriteRequest(conn, testToken, body); err != nil {
				errs <- err
				return
			}
			status, message, err := protocol.ReadResponse(conn)
			if err != nil {
				errs <- err
				return
			}
			if !status.OK() {
				errs <- errors.New(message)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("copy was rejected instead of queued: %v", err)
	}
	if h.clip.WriteCount() != clients {
		t.Errorf("clipboard writes = %d, want %d", h.clip.WriteCount(), clients)
	}
	if got := h.srv.mem.inUse(); got != 0 {
		t.Errorf("%d bytes of budget still reserved after every copy finished", got)
	}
}

// TestMemoryBudgetIsReleasedOnFailure guards the accounting on the paths that
// do not end in a successful copy. A budget that leaked on error would shrink
// with every failure until the daemon wedged.
func TestMemoryBudgetIsReleasedOnFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withClipboardError(errors.New("pbcopy exploded")))

	for range 5 {
		status, _ := h.send(testToken, []byte("payload that will not stick"))
		if status != protocol.StatusInternalError {
			t.Fatalf("status = %v, want %v", status, protocol.StatusInternalError)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.srv.mem.inUse() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%d bytes of budget leaked across failed copies", h.srv.mem.inUse())
}

func TestNewRejectsABudgetSmallerThanOnePayload(t *testing.T) {
	t.Parallel()

	tlsConfig, _, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("ephemeral: %v", err)
	}
	_, err = New(Options{
		Token:      testToken,
		TLS:        tlsConfig,
		Clipboard:  &clipboard.Fake{},
		MaxPayload: 10 << 20,
		MaxMemory:  1 << 20,
		Timeout:    time.Second,
	})
	if err == nil {
		t.Fatal("a budget too small for one payload was accepted")
	}
	if !strings.Contains(err.Error(), "memory budget") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

func TestDefaultBudgetGrowsWithThePayloadLimit(t *testing.T) {
	t.Parallel()

	tlsConfig, _, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("ephemeral: %v", err)
	}
	// A payload limit above the default budget must raise the budget rather
	// than produce a server that rejects every copy at its own stated limit.
	const huge = 512 << 20
	srv, err := New(Options{
		Token:      testToken,
		TLS:        tlsConfig,
		Clipboard:  &clipboard.Fake{},
		MaxPayload: huge,
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.mem.capacity < huge {
		t.Errorf("budget = %d, want at least the payload limit of %d", srv.mem.capacity, huge)
	}
}

// TestUnauthenticatedPeersDoNotConsumeTheCopyBudget is the regression test for
// the budget split. The copy budget is deliberately tiny here and the flood is
// well below the rationing threshold, so the only thing keeping the real client
// alive is that silent peers never reach that budget at all. Before the split
// they held it, and two silent sockets were enough to stall everyone.
func TestUnauthenticatedPeersDoNotConsumeTheCopyBudget(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withMaxConcurrent(2), withTimeout(3*time.Second))

	const hogs = 32 // below unauthSoftLimit, so per-host rationing stays out of it
	for range hogs {
		conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
	}
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	status, message := h.send(testToken, []byte("unblocked"))
	elapsed := time.Since(start)

	if !status.OK() {
		t.Fatalf("copy returned %v: %s", status, message)
	}
	if elapsed > time.Second {
		t.Errorf("copy took %v behind %d silent peers; they are still holding copy capacity",
			elapsed, hogs)
	}
}

// TestUnprovenPeersGetTheShorterDeadline covers the other half: a peer that has
// not authenticated must not inherit a timeout raised for slow payload
// transfers. Otherwise raising the timeout for a bad link also lengthens how
// long an unproven peer can sit on resources.
func TestUnprovenPeersGetTheShorterDeadline(t *testing.T) {
	t.Parallel()

	// Far longer than handshakeTimeout, so the two are clearly distinguishable.
	h := newHarness(t, withTimeout(30*time.Second))

	conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	start := time.Now()
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("the server answered a peer that sent nothing")
	}
	elapsed := time.Since(start)

	if elapsed > handshakeTimeout+2*time.Second {
		t.Errorf("silent peer held for %v; it inherited the %v timeout instead of the %v handshake bound",
			elapsed, 30*time.Second, handshakeTimeout)
	}
}

// TestAuthenticatedClientKeepsTheFullTimeout guards the other direction: the
// shorter bound must apply only before the token arrives, or a slow payload
// transfer over a link the operator raised the timeout for would be cut off.
func TestAuthenticatedClientKeepsTheFullTimeout(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withTimeout(10*time.Second))
	payload := []byte("sent in slow motion after authenticating\n")

	var frame bytes.Buffer
	if err := protocol.WriteRequest(&frame, testToken, payload); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	raw := frame.Bytes()

	conn := h.dial()
	defer conn.Close()

	// Everything through the token in one write, so authentication completes
	// promptly and the deadline is extended...
	authLen := protocol.PrologueSize + len(testToken)
	if _, err := conn.Write(raw[:authLen]); err != nil {
		t.Fatalf("write prologue: %v", err)
	}
	// ...then stall for longer than the handshake bound before sending the
	// rest. A peer that had not authenticated would be gone by now.
	time.Sleep(handshakeTimeout + 500*time.Millisecond)
	if _, err := conn.Write(raw[authLen:]); err != nil {
		t.Fatalf("write body: %v", err)
	}

	status, message, err := readResponse(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !status.OK() {
		t.Fatalf("status = %v (%s)", status, message)
	}
	if got := h.clip.Data(); !bytes.Equal(got, payload) {
		t.Errorf("clipboard = %q, want %q", got, payload)
	}
}
