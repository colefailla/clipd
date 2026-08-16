package client_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/client"
	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/protocol"
	"github.com/colefailla/clipd/internal/server"
	"github.com/colefailla/clipd/internal/transport"
)

// These tests exercise the client against a real server over a loopback
// socket. Mocking the connection would test the mock; the interesting
// failures — refused connections, half-open peers, a peer that is not clipd —
// only exist at the socket level.

const testToken = "test-token-of-sufficient-length"

func newServer(t *testing.T, maxPayload int64) (addr string, clip *clipboard.Fake, pin []byte) {
	t.Helper()

	tlsConfig, pin, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate test certificate: %v", err)
	}

	clip = &clipboard.Fake{}
	srv, err := server.New(server.Options{
		Token:      testToken,
		TLS:        tlsConfig,
		Clipboard:  clip,
		MaxPayload: maxPayload,
		Timeout:    2 * time.Second,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ln, err := server.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server.Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	})

	return ln.Addr().String(), clip, pin
}

// clientTLS builds a pinned configuration for a test server.
func clientTLS(t *testing.T, pin []byte) *tls.Config {
	t.Helper()
	cfg, err := transport.ClientConfig(pin)
	if err != nil {
		t.Fatalf("client config: %v", err)
	}
	return cfg
}

func TestCopyRoundTrip(t *testing.T) {
	t.Parallel()

	addr, clip, pin := newServer(t, 1<<20)
	payload := []byte("CONTAINER ID   IMAGE     STATUS\nabc123         nginx     Up 2 hours\n")

	res, err := client.Copy(context.Background(), client.Options{
		Address: addr,
		Token:   testToken,
		TLS:     clientTLS(t, pin),
		Timeout: 5 * time.Second,
	}, payload)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if res.Bytes != len(payload) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(payload))
	}
	if res.Message == "" {
		t.Error("the server's acknowledgement was empty")
	}
	if got := clip.Data(); !bytes.Equal(got, payload) {
		t.Errorf("clipboard = %q, want %q", got, payload)
	}
}

func TestCopyEmptyPayload(t *testing.T) {
	t.Parallel()

	addr, clip, pin := newServer(t, 1<<20)

	res, err := client.Copy(context.Background(), client.Options{
		Address: addr,
		Token:   testToken,
		TLS:     clientTLS(t, pin),
		Timeout: 5 * time.Second,
	}, []byte{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if res.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0", res.Bytes)
	}
	if len(clip.Data()) != 0 {
		t.Errorf("clipboard = %q, want empty", clip.Data())
	}
}

func TestCopyLargePayload(t *testing.T) {
	t.Parallel()

	addr, clip, pin := newServer(t, 8<<20)
	// Large enough to span many TCP segments, which is where a naive
	// single-Write or single-Read implementation falls over.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4<<20/16)

	if _, err := client.Copy(context.Background(), client.Options{
		Address: addr,
		Token:   testToken,
		TLS:     clientTLS(t, pin),
		Timeout: 30 * time.Second,
	}, payload); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := clip.Data(); !bytes.Equal(got, payload) {
		t.Errorf("clipboard holds %d bytes, want %d identical bytes", len(got), len(payload))
	}
}

func TestCopyAuthFailure(t *testing.T) {
	t.Parallel()

	addr, clip, pin := newServer(t, 1<<20)

	_, err := client.Copy(context.Background(), client.Options{
		Address: addr,
		Token:   "the-wrong-token-entirely-here",
		TLS:     clientTLS(t, pin),
		Timeout: 5 * time.Second,
	}, []byte("data"))
	if err == nil {
		t.Fatal("Copy succeeded with a bad token")
	}

	var cerr *client.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error type = %T, want *client.Error", err)
	}
	if cerr.Kind != client.KindAuth {
		t.Errorf("Kind = %v, want auth", cerr.Kind)
	}
	if clip.WriteCount() != 0 {
		t.Error("the clipboard was written despite a failed authentication")
	}
}

// TestCopyServerSideLimit covers a payload the client would send but the
// server refuses.
//
// The payload is deliberately larger than any socket buffer. The server
// rejects on the declared length and closes without reading the body, so the
// client's write fails partway through. It must still report "payload too
// large" rather than the broken pipe that failure produces: with a smaller
// payload the whole thing fits in the buffer, the write completes, and the
// race never appears — which is how this passed on macOS and failed on Linux.
func TestCopyServerSideLimit(t *testing.T) {
	t.Parallel()

	addr, _, pin := newServer(t, 1024)

	_, err := client.Copy(context.Background(), client.Options{
		Address: addr,
		Token:   testToken,
		TLS:     clientTLS(t, pin),
		Timeout: 5 * time.Second,
	}, bytes.Repeat([]byte("x"), 4<<20))
	if err == nil {
		t.Fatal("Copy succeeded despite exceeding the server's limit")
	}

	var cerr *client.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error type = %T, want *client.Error", err)
	}
	if cerr.Kind != client.KindTooLarge {
		t.Errorf("Kind = %v, want payload", cerr.Kind)
	}
	if !errors.Is(err, client.ErrTooLarge) {
		t.Error("error does not unwrap to ErrTooLarge")
	}
}

func TestCopyConnectionRefused(t *testing.T) {
	t.Parallel()

	// Bind and release a port so the address is valid but nothing is behind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	// Nothing will answer, so any valid pin will do.
	_, pin, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate certificate: %v", err)
	}

	_, err = client.Copy(context.Background(), client.Options{
		Address: addr,
		Token:   testToken,
		TLS:     clientTLS(t, pin),
		Timeout: 2 * time.Second,
	}, []byte("data"))
	if err == nil {
		t.Fatal("Copy succeeded against a closed port")
	}

	var cerr *client.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error type = %T, want *client.Error", err)
	}
	if cerr.Kind != client.KindConnect {
		t.Errorf("Kind = %v, want connect", cerr.Kind)
	}
	// The message has to name the address: "connection refused" alone leaves
	// the user guessing which host was even tried.
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error = %q, want it to name the address", err)
	}
}

// TestCopyAgainstNonClipdPeer covers pointing clipd at the wrong port: the
// peer accepts and hangs up without a clipd response.
func TestCopyAgainstNonClipdPeer(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Drain and hang up, like a service that does not understand us.
			go func() {
				_, _ = io.Copy(io.Discard, io.LimitReader(conn, 1024))
				conn.Close()
			}()
		}
	}()

	_, pin, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate certificate: %v", err)
	}

	_, err = client.Copy(context.Background(), client.Options{
		Address: ln.Addr().String(),
		Token:   testToken,
		TLS:     clientTLS(t, pin),
		Timeout: 3 * time.Second,
	}, []byte("data"))
	if err == nil {
		t.Fatal("Copy succeeded against a peer that is not clipd")
	}

	var cerr *client.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error type = %T, want *client.Error", err)
	}
	// The failure now lands in the handshake rather than at the
	// acknowledgement: a peer that is not clipd is not speaking TLS either.
	if cerr.Kind != client.KindTLS {
		t.Errorf("Kind = %v, want tls", cerr.Kind)
	}
}

// TestCopyAgainstV1Daemon covers the upgrade case from the other direction: a
// v2 client reaching a daemon that has not been upgraded. The v1 server reads
// the ClientHello as a malformed frame and answers in cleartext, which Go
// surfaces as a typed RecordHeaderError — so the client can say what is
// actually wrong instead of reporting a raw TLS error.
func TestCopyAgainstV1Daemon(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		// Exactly what clipd v1 does with bytes that are not a clipd frame.
		_ = protocol.WriteResponse(conn, protocol.StatusMalformed, "protocol: not a clipd request")
		// Consume the rest of the ClientHello before closing. Closing with
		// unread data makes the kernel abort the connection with RST, which on
		// some client platforms destroys the buffered response this test is
		// about; a real network delivers the reply, so the fake must too.
		_, _ = io.Copy(io.Discard, io.LimitReader(conn, 64<<10))
	}()

	_, pin, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate certificate: %v", err)
	}

	_, err = client.Copy(context.Background(), client.Options{
		Address: ln.Addr().String(),
		Token:   testToken,
		TLS:     clientTLS(t, pin),
		Timeout: 3 * time.Second,
	}, []byte("data"))
	if err == nil {
		t.Fatal("Copy succeeded against a plaintext v1 daemon")
	}

	var cerr *client.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error type = %T, want *client.Error", err)
	}
	if cerr.Kind != client.KindTLS {
		t.Errorf("Kind = %v, want tls", cerr.Kind)
	}
	if !strings.Contains(err.Error(), "v1") {
		t.Errorf("error = %q, want it to name v1 as the likely cause", err)
	}
}

func TestCopyTimesOutOnSilentPeer(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	held := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Hold the connection open and never answer.
		held <- conn
	}()

	_, pin, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate certificate: %v", err)
	}

	start := time.Now()
	_, err = client.Copy(context.Background(), client.Options{
		Address: ln.Addr().String(),
		Token:   testToken,
		TLS:     clientTLS(t, pin),
		Timeout: 500 * time.Millisecond,
	}, []byte("data"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Copy succeeded against a peer that never responded")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Copy took %s; the deadline did not apply", elapsed)
	}

	select {
	case conn := <-held:
		conn.Close()
	default:
	}
}

func TestCopyRejectsMissingConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts client.Options
		want client.Kind
	}{
		{"no address", client.Options{Token: testToken}, client.KindConnect},
		{"no token", client.Options{Address: "127.0.0.1:1"}, client.KindAuth},
		// No pin means nothing to verify the server against, which is not
		// better than plaintext and so is refused outright.
		{"no TLS config", client.Options{Address: "127.0.0.1:1", Token: testToken}, client.KindTLS},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := client.Copy(context.Background(), tc.opts, []byte("data"))
			var cerr *client.Error
			if !errors.As(err, &cerr) {
				t.Fatalf("error = %v (%T), want *client.Error", err, err)
			}
			if cerr.Kind != tc.want {
				t.Errorf("Kind = %v, want %v", cerr.Kind, tc.want)
			}
		})
	}
}

func TestCopyHonoursCancelledContext(t *testing.T) {
	t.Parallel()

	addr, _, pin := newServer(t, 1<<20)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.Copy(ctx, client.Options{
		Address: addr,
		Token:   testToken,
		TLS:     clientTLS(t, pin),
		Timeout: 5 * time.Second,
	}, []byte("data")); err == nil {
		t.Fatal("Copy succeeded with a cancelled context")
	}
}

func TestReadInput(t *testing.T) {
	t.Parallel()

	t.Run("under the limit", func(t *testing.T) {
		t.Parallel()
		got, err := client.ReadInput(strings.NewReader("hello\n"), 1024)
		if err != nil {
			t.Fatalf("ReadInput: %v", err)
		}
		if string(got) != "hello\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("exactly at the limit", func(t *testing.T) {
		t.Parallel()
		input := strings.Repeat("x", 100)
		got, err := client.ReadInput(strings.NewReader(input), 100)
		if err != nil {
			t.Fatalf("ReadInput at the limit: %v", err)
		}
		if len(got) != 100 {
			t.Errorf("got %d bytes, want 100", len(got))
		}
	})

	// Truncation would corrupt the user's data in the least visible way
	// possible, so one byte over the limit is a hard failure.
	t.Run("one byte over the limit", func(t *testing.T) {
		t.Parallel()
		_, err := client.ReadInput(strings.NewReader(strings.Repeat("x", 101)), 100)
		if err == nil {
			t.Fatal("ReadInput accepted an oversized input")
		}
		if !errors.Is(err, client.ErrTooLarge) {
			t.Errorf("error = %v, want ErrTooLarge", err)
		}
		var cerr *client.Error
		if !errors.As(err, &cerr) || cerr.Kind != client.KindTooLarge {
			t.Errorf("error = %v, want Kind payload", err)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		t.Parallel()
		got, err := client.ReadInput(strings.NewReader(""), 100)
		if err != nil {
			t.Fatalf("ReadInput: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("read error", func(t *testing.T) {
		t.Parallel()
		_, err := client.ReadInput(errReader{}, 100)
		if err == nil {
			t.Fatal("ReadInput ignored a read error")
		}
	})

	t.Run("invalid limit", func(t *testing.T) {
		t.Parallel()
		if _, err := client.ReadInput(strings.NewReader("x"), 0); err == nil {
			t.Error("ReadInput accepted a zero limit")
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }
