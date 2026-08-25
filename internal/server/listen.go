package server

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Listen opens the daemon's listening socket.
//
// The address decides the transport, and with it the security model:
//
//	/path/to/clipd.sock   a UNIX domain socket, reachable only through the
//	                      filesystem — and, via `ssh -R`, from a machine the
//	                      user has already authenticated to
//	host:port             a TCP socket, reachable by anything that can route
//	                      to it
//
// The socket is the supported configuration and the default. TCP exists
// because a listener is occasionally wanted for a local test or a container,
// and it is deliberately not the path anything is documented to use: nothing
// in this daemon authenticates, so a TCP listener on anything but the loopback
// interface hands the clipboard to the network.
//
// Splitting on whether the address looks like a path is taken from
// wincent/clipper, which does the same thing for the same reason: it collapses
// two settings into one, and makes the transport obvious from the value.
func Listen(address string) (net.Listener, error) {
	if !IsSocketPath(address) {
		ln, err := net.Listen("tcp", address)
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", address, err)
		}
		return ln, nil
	}

	path, err := ExpandPath(address)
	if err != nil {
		return nil, err
	}
	// Checked before binding, because the kernel's answer when the path is too
	// long is EINVAL — "bind: invalid argument" — which says nothing about
	// length and sends people looking at permissions instead. The limit is the
	// size of sun_path in the sockaddr struct: 104 bytes on darwin, 108 on
	// Linux, minus the terminating NUL.
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is %d bytes, over the %d-byte limit this platform allows; choose a shorter path",
			path, len(path), maxSocketPath)
	}
	if err := prepareSocketPath(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// Belt and braces over the process umask: the socket is the entire access
	// control for this daemon, so its mode is not left to inherited state.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restrict %s: %w", path, err)
	}
	return ln, nil
}

// prepareSocketPath clears a socket file left behind by a previous run.
//
// A UNIX socket is a filesystem entry that outlives the process holding it, so
// a daemon killed with SIGKILL — or a Mac that lost power — leaves one behind.
// Binding then fails with "address already in use", which is indistinguishable
// from a second daemon actually running, and the daemon crash-loops under
// launchd's KeepAlive until someone deletes the file by hand.
//
// Dialling it settles the question. If something answers, a live daemon owns
// the path and this one has no business taking it. If nothing does, the file
// is a corpse and can be removed. This is wincent/clipper's approach, and it
// is the fix for a failure mode that is otherwise very hard to diagnose from
// the symptom.
func prepareSocketPath(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		// Not a socket: refuse rather than delete. Whatever it is, the user put
		// it there, and removing an unrelated file because it occupies a path
		// the daemon wanted is not a trade the daemon gets to make.
		return fmt.Errorf("%s exists and is not a socket; move it or choose another address", path)
	}
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		return fmt.Errorf("a clipd daemon is already listening on %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}

// IsSocketPath reports whether an address names a filesystem path rather than
// a host and port.
func IsSocketPath(address string) bool {
	return strings.HasPrefix(address, "/") ||
		strings.HasPrefix(address, "~") ||
		strings.HasPrefix(address, ".")
}

// ExpandPath resolves a leading ~ and makes the path absolute.
//
// launchd starts agents with / as the working directory, so a relative path
// that worked in a shell would resolve somewhere else entirely under the
// daemon. Absolutising here means the value in the config means the same thing
// in both places.
func ExpandPath(path string) (string, error) {
	if strings.HasPrefix(path, "~") {
		u, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		path = u.HomeDir + path[1:]
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	return abs, nil
}
