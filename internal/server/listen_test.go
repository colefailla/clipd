package server

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/protocol"
)

func TestExchangePingRequiresTheExactPong(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		reply string
		ok    bool
	}{
		{name: "exact", reply: protocol.StatusOK + protocol.Pong + "\n", ok: true},
		{name: "suffix", reply: protocol.StatusOK + protocol.Pong + "-not-clipd\n"},
		{name: "missing newline", reply: protocol.StatusOK + protocol.Pong},
		{name: "error status", reply: protocol.StatusError + protocol.Pong + "\n"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, peer := net.Pipe()
			defer client.Close()
			go func() {
				defer peer.Close()
				request := make([]byte, len(protocol.Ping))
				if _, err := io.ReadFull(peer, request); err == nil {
					_, _ = io.WriteString(peer, tc.reply)
				}
			}()

			err := exchangePing(client)
			if tc.ok && err != nil {
				t.Fatalf("exchangePing: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("exchangePing accepted an inexact response")
			}
		})
	}
}

// TestListenRemovesAStaleSocket pins the recovery that makes the daemon
// restartable.
//
// A UNIX socket is a filesystem entry that outlives the process that bound it,
// so a daemon killed with SIGKILL leaves one behind. Without this, the next
// start fails with "address already in use" and launchd's KeepAlive turns that
// into a restart loop that only a manual rm breaks.
func TestListenRemovesAStaleSocket(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	path := filepath.Join(dir, "s.sock")

	// Bind and close without unlinking, which is what a killed process leaves.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	if unix, ok := ln.(*net.UnixListener); ok {
		unix.SetUnlinkOnClose(false)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close seed listener: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stale socket was not left behind: %v", err)
	}

	got, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	defer got.Close()
}

// TestListenRefusesALiveSocket is the other half: a socket a daemon answers on
// belongs to that daemon, and taking it would leave two fighting over one path.
//
// The daemon has to actually be serving, because "live" now means it identified
// itself. A bound socket with nothing behind it is a different case, covered
// below.
func TestListenRefusesALiveSocket(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	path := filepath.Join(dir, "s.sock")

	first, err := Listen(path)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	srv, err := New(Options{Clipboard: &clipboard.Fake{}, MaxPayload: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, first)

	_, err = Listen(path)
	if err == nil {
		t.Fatal("Listen took over a live socket")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("err = %v, want it to say a daemon is already listening", err)
	}
}

// TestListenRefusesAForeignSocket covers the case a bare dial could not see.
//
// Something is bound to the path and accepting connections, but it does not
// answer as clipd. Deleting it would be taking over another program's socket,
// and a forwarded connection delivered to it would hand that program the
// clipboard content — so the daemon refuses to start instead.
func TestListenRefusesAForeignSocket(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	path := filepath.Join(dir, "s.sock")

	other, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	defer other.Close()

	_, err = Listen(path)
	if err == nil {
		t.Fatal("Listen took over a socket belonging to something else")
	}
	if !strings.Contains(err.Error(), "did not answer as clipd") {
		t.Errorf("err = %v, want it to say the listener is not clipd", err)
	}
	// The other listener's socket is still its own.
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Errorf("the foreign socket was disturbed: %v, %v", info, err)
	}
}

// TestListenRefusesToDeleteANonSocket: whatever is at that path, the user put
// it there. Removing an unrelated file because it occupies a wanted name is
// not a trade the daemon gets to make.
func TestListenRefusesToDeleteANonSocket(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	path := filepath.Join(dir, "important.txt")
	if err := os.WriteFile(path, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if _, err := Listen(path); err == nil {
		t.Fatal("Listen bound over a regular file")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "do not delete" {
		t.Errorf("the file was disturbed: %q, %v", got, err)
	}
}

func TestListenRestrictsSocketPermissions(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	path := filepath.Join(dir, "s.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The socket's mode is the daemon's entire access control: it is what
	// keeps other accounts on a shared remote host from writing to the
	// clipboard through the forwarded socket.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("socket mode = %04o, want no group or other access", perm)
	}
}

func TestListenRefusesWritableSocketDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory permission bits are not meaningful on Windows")
	}

	dir := filepath.Join(shortTempDir(t), "shared")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The process umask may have narrowed Mkdir's requested mode.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	path := filepath.Join(dir, "clipd.sock")
	if ln, err := Listen(path); err == nil {
		ln.Close()
		t.Fatal("Listen accepted a socket path another account can pre-bind")
	} else if !strings.Contains(err.Error(), "writable by another account") {
		t.Fatalf("Listen error = %v, want an unsafe-directory explanation", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("Listen created a socket before rejecting its parent: %v", err)
	}
}

func TestExpandPathResolvesTilde(t *testing.T) {
	t.Parallel()

	got, err := ExpandPath("~/.clipd.sock")
	if err != nil {
		t.Fatalf("ExpandPath: %v", err)
	}
	if strings.HasPrefix(got, "~") {
		t.Errorf("ExpandPath left the tilde in place: %q", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ExpandPath returned a relative path: %q", got)
	}
}

// TestListenRefusesTCPAddresses: clipd listens only on a UNIX socket. A
// host:port, loopback or not, is refused with a reason rather than bound, or
// taken as the name of a socket file in the working directory.
func TestListenRefusesTCPAddresses(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0", "0.0.0.0:0", ":0"} {
		ln, err := Listen(addr)
		if err == nil {
			ln.Close()
			t.Errorf("Listen(%q) bound a TCP address", addr)
			continue
		}
		if !strings.Contains(err.Error(), "not a socket path") {
			t.Errorf("Listen(%q) failed with %v, want it to explain why", addr, err)
		}
	}
}
