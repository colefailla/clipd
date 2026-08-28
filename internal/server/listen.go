package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/colefailla/clipd/internal/protocol"
)

// pingTimeout bounds one identification exchange. Short, because the daemon
// answers a ping without doing any work: anything slower than this is not a
// healthy clipd whatever else it may be.
const pingTimeout = 2 * time.Second

// maxPingReply bounds what a ping will read back, so that a listener which is
// not clipd cannot make the caller buffer without limit.
const maxPingReply = 512

// ErrNotClipd reports that something accepted the connection but did not
// identify itself as clipd — including by saying nothing at all.
var ErrNotClipd = errors.New("something is listening, but it did not answer as clipd")

// Ping asks whatever is listening at address to identify itself, returning nil
// only when a clipd daemon answered.
//
// This replaces connecting and closing immediately, which looked like a live
// daemon whatever was on the other end — and which, on the wire, was
// indistinguishable from a raw clipboard message of zero bytes, so every
// `clipd status` cleared the clipboard it was reporting on.
//
// A dial failure is returned as it is; anything that goes wrong after the
// connection is accepted is wrapped in ErrNotClipd, so a caller can tell
// "nothing there" from "something there that is not this daemon". Deleting a
// socket file is only ever safe on the first.
func Ping(network, address string) error {
	conn, err := net.DialTimeout(network, address, pingTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := exchangePing(conn); err != nil {
		// Both errors stay in the chain: callers test for ErrNotClipd, and
		// probeSocket also needs to know whether the failure was a timeout.
		return fmt.Errorf("%w (%w)", ErrNotClipd, err)
	}
	return nil
}

// exchangePing sends the ping frame and checks the reply.
func exchangePing(conn net.Conn) error {
	if err := conn.SetDeadline(time.Now().Add(pingTimeout)); err != nil {
		return err
	}
	if _, err := conn.Write(protocol.Ping); err != nil {
		return err
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxPingReply)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !strings.HasPrefix(line, protocol.StatusOK+protocol.Pong) {
		return fmt.Errorf("unexpected reply %q", line)
	}
	return nil
}

// Listen opens the daemon's listening socket.
//
// The address decides the transport, and with it the security model:
//
//	/path/to/clipd.sock   a UNIX domain socket
//	127.0.0.1:8199        a TCP socket, loopback only
//
// Both are tunnel endpoints, not network services. The socket is the supported
// configuration; the loopback port exists for hosts where SSH cannot forward a
// socket at all — OpenSSH gained that ability only in 6.7, and its Windows
// build still lacks it — so a port is the only endpoint available there.
//
// A non-loopback address is refused rather than warned about. Nothing in this
// daemon authenticates, so binding one would hand the clipboard to everything
// that can route to it, and a configuration that dangerous should be
// impossible rather than discouraged.
//
// Splitting on whether the address looks like a path is taken from
// wincent/clipper, which does the same thing for the same reason: it collapses
// two settings into one, and makes the transport obvious from the value.
func Listen(address string) (net.Listener, error) {
	if !IsSocketPath(address) {
		if err := requireLoopback(address); err != nil {
			return nil, err
		}
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
// Asking it settles the question. If a clipd daemon answers, it owns the path
// and this one has no business taking it. If nothing accepts the connection,
// the file is a corpse and can be removed. This is wincent/clipper's approach,
// with the addition that the listener has to identify itself: a bare dial
// proves only that something accepted, which is also true of a daemon that is
// not clipd, and of a socket whose owner exited a moment ago.
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

	switch probeSocket(path) {
	case socketLive:
		return fmt.Errorf("a clipd daemon is already listening on %s", path)
	case socketForeign:
		// Something accepted and did not answer as clipd. Not ours to delete,
		// for the same reason a non-socket file is not: it belongs to whoever
		// put it there. A clipd daemon too busy to reply also lands here, which
		// is the safe way round — refusing to start is recoverable, deleting a
		// running daemon's socket is not.
		return fmt.Errorf("%s is in use by something that did not answer as clipd; stop it or choose another address", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}

// socketState is what probeSocket concluded about an existing socket file.
type socketState int

const (
	socketDead socketState = iota
	socketLive
	socketForeign
)

// probeAttempts and probeGap bound the retry in probeSocket.
const (
	probeAttempts = 3
	probeGap      = 50 * time.Millisecond
)

// probeSocket decides whether a socket file has a clipd daemon behind it.
//
// Retried, and the last attempt is what counts, because a single dial is not a
// reliable answer: on Darwin a socket whose owner has just exited can still
// accept a connection for a moment, which reports a live daemon that is already
// gone and leaves the new one refusing to start. A genuinely dead socket
// refuses every later attempt, so the last one is the honest one.
func probeSocket(path string) socketState {
	state := socketDead
	for attempt := 0; attempt < probeAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(probeGap)
		}
		switch err := Ping("unix", path); {
		case err == nil:
			return socketLive
		case errors.Is(err, ErrNotClipd):
			// A listener that accepts and then says nothing will say nothing on
			// the next attempt either, so there is no point spending another
			// timeout on it. The retry below exists for the socket that fails
			// fast — the one whose owner has just exited — not for this.
			if isTimeout(err) {
				return socketForeign
			}
			state = socketForeign
		default:
			// Nothing accepted the connection on this attempt.
			state = socketDead
		}
	}
	return state
}

// requireLoopback rejects any TCP address the network could reach.
//
// The check is on the literal string rather than on what a name resolves to:
// resolution needs DNS the daemon may not have at startup, and it can change
// underneath a host that was loopback when it was configured. "localhost" is
// accepted by name because it is loopback everywhere by definition; anything
// else must say so as an address.
func requireLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("address %q: %w", address, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	// An empty host is the most dangerous spelling of all: ":8199" binds every
	// interface, and reads like it binds none.
	shown := host
	if shown == "" {
		shown = "(empty, meaning every interface)"
	}
	return fmt.Errorf("address %q listens on %s, and clipd has no authentication: "+
		"use a socket path, or 127.0.0.1 if this host cannot forward one", address, shown)
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
