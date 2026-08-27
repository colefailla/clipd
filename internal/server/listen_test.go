package server

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestListenRefusesALiveSocket is the other half: a socket that answers
// belongs to a running daemon, and taking it would leave two daemons fighting
// over one path.
func TestListenRefusesALiveSocket(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	path := filepath.Join(dir, "s.sock")

	first, err := Listen(path)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer first.Close()

	_, err = Listen(path)
	if err == nil {
		t.Fatal("Listen took over a live socket")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("err = %v, want it to say a daemon is already listening", err)
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

func TestIsSocketPath(t *testing.T) {
	t.Parallel()

	sockets := []string{"/var/run/clipd.sock", "~/.clipd.sock", "./clipd.sock"}
	hosts := []string{"localhost:8199", "127.0.0.1:8199", "[::1]:8199", ":8199"}

	for _, s := range sockets {
		if !IsSocketPath(s) {
			t.Errorf("IsSocketPath(%q) = false, want true", s)
		}
	}
	for _, h := range hosts {
		if IsSocketPath(h) {
			t.Errorf("IsSocketPath(%q) = true, want false", h)
		}
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

// TestListenRefusesNonLoopbackTCP: the daemon has no authentication, so a
// reachable TCP address would hand the clipboard to whatever can route to it.
// That configuration is refused rather than warned about.
func TestListenRefusesNonLoopbackTCP(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{
		"0.0.0.0:0", ":0", "192.168.1.5:0", "[::]:0", "example.com:0",
	} {
		ln, err := Listen(addr)
		if err == nil {
			ln.Close()
			t.Errorf("Listen(%q) bound a reachable address", addr)
			continue
		}
		if !strings.Contains(err.Error(), "no authentication") {
			t.Errorf("Listen(%q) failed with %v, want it to explain why", addr, err)
		}
	}
}

// TestListenAllowsLoopbackTCP keeps the case the restriction exists to
// preserve: hosts where SSH cannot forward a UNIX socket still need an
// endpoint, and a loopback port is one.
func TestListenAllowsLoopbackTCP(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		ln, err := Listen(addr)
		if err != nil {
			t.Errorf("Listen(%q): %v", addr, err)
			continue
		}
		ln.Close()
	}
}
