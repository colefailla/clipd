// Package server implements the clipd daemon: accept a connection,
// authenticate it, read a clipboard payload, hand it to the host clipboard.
//
// The daemon runs in the foreground and never self-daemonizes. On macOS,
// launchd owns backgrounding, log redirection and restart-on-crash, so
// duplicating any of that here would only add ways to disagree with launchd.
package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/colefailla/clipd/internal/auth"
	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/protocol"
)

const (
	// minThroughput is the slowest transfer rate the payload deadline
	// assumes. The handshake gets a flat timeout, but the body cannot: a
	// legitimate 10 MiB paste over a congested link must not be killed at the
	// same deadline that a 40-byte one gets. 256 KiB/s is far below any real
	// LAN and still bounds a byte-trickling client.
	minThroughput = 256 << 10

	// defaultMaxConcurrent bounds copies being carried out at once: reading a
	// payload and handing it to the clipboard.
	//
	// It is acquired after authentication, not before accepting, because the
	// two halves of a connection's life cost wildly different amounts. Holding
	// a socket open is a goroutine and some buffers; performing a copy reads a
	// payload into memory and forks a clipboard helper. Rationing them together
	// meant a peer that had proved nothing could exhaust the budget for peers
	// that had — which is precisely the denial of service this split closes.
	//
	// It also bounds concurrent pbcopy processes, which nothing else does:
	// memBudget counts bytes, and a thousand one-byte copies weigh nothing
	// while still being a thousand forks.
	defaultMaxConcurrent = 128

	// defaultMaxConnections bounds sockets in any state, the backstop against
	// unbounded goroutine growth.
	//
	// Far larger than defaultMaxConcurrent because it guards something far
	// cheaper. Making an unauthenticated flood expensive means having enough
	// room that filling it is hard: at roughly 30 KiB per connection in the
	// worst case this is tens of megabytes, which is a fair price for turning a
	// trivial attack into a sustained one.
	defaultMaxConnections = 1024

	// handshakeTimeout bounds the phase before a valid token arrives.
	//
	// Separate from the configurable timeout, which a user may raise for a slow
	// link, because the two bound different things: a payload transfer can
	// legitimately be slow, while a handshake and a first frame are a few round
	// trips and never are. Leaving them shared meant raising the timeout to
	// accommodate a slow network also lengthened how long an unproven peer
	// could sit on a slot. It is only ever used to shorten, never to extend, so
	// a timeout below it still wins.
	handshakeTimeout = 2 * time.Second

	// unauthSoftLimit is how many connections may sit in the pre-authentication
	// phase before per-host rationing begins.
	//
	// Below it nothing is rationed. A burst of parallel copies from one machine
	// is ordinary use — `xargs -P24` ending in clipd is a reasonable thing to
	// write — and a limiter that throttled it would trade a denial of service by
	// an attacker for one by the owner.
	//
	// Scaled with defaultMaxConnections rather than fixed, so that rationing
	// still begins at a quarter of capacity.
	unauthSoftLimit = defaultMaxConnections / 4

	// maxUnauthPerIP bounds how many pre-authentication connections one source
	// address may hold once unauthSoftLimit is reached.
	//
	// Without a bound of this shape the connection budget is exhaustible by
	// anyone who can reach the port: a peer that completes the TCP handshake and
	// then sends nothing occupies a slot until its deadline expires, having
	// proved nothing about itself. This is sshd's MaxStartups in miniature —
	// rationing engages only under pressure, and then favours hosts that are not
	// already holding a share. One host can therefore stall at most
	// unauthSoftLimit slots of defaultMaxConcurrent, leaving the rest servable.
	//
	// A slot is released the moment a valid token arrives, so an authenticated
	// client never accumulates against this at all.
	maxUnauthPerIP = 8

	// defaultMemoryBudget bounds buffered payload bytes when nothing else is
	// configured. Must agree with config.DefaultMaxMemoryBytes.
	//
	// It is the fallback for callers constructing a Server directly; the daemon
	// passes an explicit value resolved from the config file.
	defaultMemoryBudget int64 = 64 << 20

	// shutdownGrace is how long Shutdown waits for in-flight copies before
	// giving up on them.
	shutdownGrace = 5 * time.Second

	// warnWindow and warnBudget bound how many peer-driven warnings reach the
	// log per window.
	//
	// launchd appends the daemon's log file forever with no rotation, and every
	// rejected connection would otherwise write a line: a peer sending malformed
	// frames in a loop turns "the clipboard is unavailable" into "the disk is
	// full". Suppressed lines are counted and reported, so throttling hides the
	// volume of an attack, never the fact of one.
	warnWindow = time.Minute
	warnBudget = 20
)

// Options configures a Server.
type Options struct {
	// Token is the expected shared secret. Required.
	Token string

	// TLS is the server's TLS configuration. Required: there is no plaintext
	// mode. A flag to disable encryption would be set once during some late
	// night of debugging and never unset, and supporting both would double
	// the connection-handling paths for a two-machine tool.
	TLS *tls.Config

	// Clipboard receives accepted payloads. Required.
	Clipboard clipboard.Clipboard

	// MaxPayload is the largest accepted payload in bytes.
	MaxPayload int64

	// MaxMemory bounds the total payload bytes buffered across all connections
	// at once. Zero means a default derived from MaxPayload.
	//
	// It must be at least MaxPayload, or a copy the server has just declared
	// acceptable could never be given room to run.
	MaxMemory int64

	// Timeout bounds the handshake and the acknowledgement write.
	Timeout time.Duration

	// MaxConcurrent bounds copies performed at once. It is taken after
	// authentication, so it does not bound sockets — see defaultMaxConnections.
	MaxConcurrent int

	// Logger receives operational logs. Clipboard contents are never logged.
	Logger *slog.Logger
}

// Server accepts clipd connections. The zero value is not usable; use New.
type Server struct {
	token      string
	tlsConfig  *tls.Config
	clip       clipboard.Clipboard
	maxPayload int64
	timeout    time.Duration
	log        *slog.Logger

	// mem bounds the payload bytes buffered across all handlers at once.
	mem *memBudget

	// connSem bounds sockets in any state; sem bounds copies actually being
	// performed, and is taken only once a peer has authenticated.
	connSem chan struct{}
	sem     chan struct{}

	// unauth counts, per source address, the connections that have not yet
	// presented a valid token; unauthTotal is their sum.
	unauthMu    sync.Mutex
	unauth      map[string]int
	unauthTotal int

	warnLimit warnLimiter

	wg sync.WaitGroup

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

// warnLimiter is a fixed-window rate limiter for peer-driven log lines.
//
// A token bucket would be smoother, but a fixed window is a dozen lines and
// clipd has no third-party dependencies to borrow one from. The distinction
// does not matter for a limiter whose only job is to keep a hostile peer from
// growing a file without bound.
type warnLimiter struct {
	mu          sync.Mutex
	windowStart time.Time
	emitted     int
	suppressed  int
}

// allow reports whether a warning may be logged now. When a window rolls over
// it also returns how many lines were dropped during the previous one, so the
// caller can account for them rather than losing them silently.
func (l *warnLimiter) allow(now time.Time) (ok bool, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.windowStart) >= warnWindow {
		dropped = l.suppressed
		l.windowStart = now
		l.emitted = 0
		l.suppressed = 0
	}
	if l.emitted < warnBudget {
		l.emitted++
		return true, dropped
	}
	l.suppressed++
	return false, dropped
}

// New validates options and constructs a Server.
func New(opts Options) (*Server, error) {
	if err := auth.Validate(opts.Token); err != nil {
		return nil, err
	}
	if opts.TLS == nil {
		return nil, errors.New("server: no TLS configuration")
	}
	if opts.Clipboard == nil {
		return nil, errors.New("server: no clipboard backend")
	}
	if opts.MaxPayload < 1 {
		return nil, fmt.Errorf("server: max payload %d must be positive", opts.MaxPayload)
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("server: timeout %s must be positive", opts.Timeout)
	}
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent < 1 {
		maxConcurrent = defaultMaxConcurrent
	}
	maxMemory := opts.MaxMemory
	if maxMemory < 1 {
		// Never below one payload: a budget that cannot hold a single
		// permitted copy would reject work the size check just allowed.
		maxMemory = max(defaultMemoryBudget, opts.MaxPayload)
	}
	if maxMemory < opts.MaxPayload {
		return nil, fmt.Errorf("server: memory budget %d is smaller than the %d byte payload limit",
			maxMemory, opts.MaxPayload)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Server{
		token:      opts.Token,
		tlsConfig:  opts.TLS,
		clip:       opts.Clipboard,
		maxPayload: opts.MaxPayload,
		timeout:    opts.Timeout,
		log:        logger,
		sem:        make(chan struct{}, maxConcurrent),
		// Never below maxConcurrent, or work slots would be unreachable: every
		// copy holds a connection slot for its whole life.
		connSem: make(chan struct{}, max(defaultMaxConnections, maxConcurrent)),
		unauth:  make(map[string]int),
		mem:     newMemBudget(maxMemory),
	}, nil
}

// warnPeer logs a warning caused by a remote peer, subject to the rate budget.
//
// Every warning in a connection handler goes through here rather than to the
// logger directly: they are all reachable by anyone who can open a socket, and
// so are all capable of growing an unrotated file without bound.
func (s *Server) warnPeer(msg string, args ...any) {
	ok, dropped := s.warnLimit.allow(time.Now())
	if dropped > 0 {
		s.log.Warn("suppressed peer-driven warnings to bound log growth",
			"dropped", dropped, "window", warnWindow.String())
	}
	if ok {
		s.log.Warn(msg, args...)
	}
}

// admit reserves a pre-authentication slot for host, reporting false when the
// server is under pressure and that host is already holding its share.
func (s *Server) admit(host string) bool {
	s.unauthMu.Lock()
	defer s.unauthMu.Unlock()

	// Rationing engages only once enough connections are waiting to prove
	// themselves for exhaustion to be a live possibility.
	if s.unauthTotal >= unauthSoftLimit && s.unauth[host] >= maxUnauthPerIP {
		return false
	}
	s.unauth[host]++
	s.unauthTotal++
	return true
}

// release returns a pre-authentication slot. The map entry is deleted at zero
// so that a long-lived daemon does not accumulate one entry per address it has
// ever been contacted by.
func (s *Server) release(host string) {
	s.unauthMu.Lock()
	defer s.unauthMu.Unlock()
	if n := s.unauth[host]; n <= 1 {
		delete(s.unauth, host)
	} else {
		s.unauth[host] = n - 1
	}
	if s.unauthTotal > 0 {
		s.unauthTotal--
	}
}

// remoteHost is the address portion of a peer's endpoint, so that the
// per-source limit counts hosts rather than ephemeral ports.
func remoteHost(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// Listen opens a listener on addr. It is separate from Serve so callers can
// report the resolved address (which matters when the port is 0) and so tests
// can supply their own listener.
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

// Serve accepts connections until ctx is cancelled or the listener fails.
//
// It closes ln before returning. A cancelled context is a clean shutdown and
// yields a nil error.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("server: already shut down")
	}
	s.listener = ln
	s.mu.Unlock()

	// Cancellation reaches a blocked Accept only by closing the listener.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.closeListener()
		case <-stopped:
		}
	}()
	defer close(stopped)

	s.log.Info("clipd daemon ready",
		"address", ln.Addr().String(),
		"clipboard", s.clip.Name(),
		"max_payload_bytes", s.maxPayload)

	for {
		// Acquire capacity before accepting. Blocking here applies back
		// pressure through the kernel's accept queue instead of piling up
		// goroutines for connections we are not ready to service.
		//
		// This is the cheap budget, not the copy budget. Gating accepts on the
		// copy budget is what let unauthenticated peers stall real clients:
		// once they filled it the daemon stopped accepting altogether, and a
		// legitimate connection sat unseen in the kernel backlog until its own
		// deadline expired.
		select {
		case s.connSem <- struct{}{}:
		case <-ctx.Done():
			return s.drain(ctx)
		}

		conn, err := ln.Accept()
		if err != nil {
			<-s.connSem
			if ctx.Err() != nil || s.isClosed() {
				return s.drain(ctx)
			}
			// A transient accept error (EMFILE, ECONNABORTED) should not kill
			// a daemon the user expects to stay up until launchd stops it.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return fmt.Errorf("accept: %w", err)
		}

		// A connection accepted in the instant before the listener closed
		// would otherwise register itself after Shutdown began waiting, which
		// is both a WaitGroup misuse and a copy that could be cut off
		// mid-write. Registering under the same lock that sets the closed
		// flag makes the two mutually exclusive.
		if !s.track() {
			conn.Close()
			<-s.connSem
			return s.drain(ctx)
		}
		go func() {
			defer s.wg.Done()
			defer func() { <-s.connSem }()
			s.handle(ctx, conn)
		}()
	}
}

// track registers an in-flight handler, or reports false if the server has
// already begun shutting down.
func (s *Server) track() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	return true
}

// Shutdown stops accepting and waits for in-flight connections.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closeListener()
	return s.waitForHandlers(ctx)
}

func (s *Server) drain(ctx context.Context) error {
	s.closeListener()
	// ctx is already cancelled at this point, so give handlers their own
	// bounded window to finish the copy they are in the middle of.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := s.waitForHandlers(drainCtx); err != nil {
		s.log.Warn("shutdown timed out with connections still open")
	}
	s.log.Info("clipd daemon stopped")
	return nil
}

// waitForHandlers blocks until every registered handler is done.
//
// Callers must have closed the listener first: that is what stops track from
// admitting new handlers, and so what makes the wait conclusive.
func (s *Server) waitForHandlers(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) closeListener() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.listener != nil {
		_ = s.listener.Close()
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// handle services one connection. Every failure path closes the connection;
// none of them panic, and none of them log payload contents.
func (s *Server) handle(ctx context.Context, rawConn net.Conn) {
	defer rawConn.Close()
	remote := rawConn.RemoteAddr().String()

	// Admission happens before anything else, including the deadline: the
	// cheapest response to a host that is already holding its share of
	// unauthenticated slots is to close on it immediately.
	host := remoteHost(rawConn.RemoteAddr())
	if !s.admit(host) {
		s.warnPeer("too many unauthenticated connections from one host; rejecting",
			"remote", remote, "limit", maxUnauthPerIP)
		return
	}
	// Released on the first of: successful authentication, or the handler
	// returning. OnceFunc makes calling it on both paths harmless.
	releaseSlot := sync.OnceFunc(func() { s.release(host) })
	defer releaseSlot()

	// The deadline is set before the first read so a client that connects and
	// says nothing cannot hold the slot open indefinitely. It covers the TLS
	// handshake as well, since tls.Conn delegates deadlines downward.
	//
	// min, so that a timeout configured below the handshake bound still wins:
	// this exists to shorten the unproven phase, never to grant it more time
	// than the operator asked for.
	if err := rawConn.SetDeadline(time.Now().Add(min(s.timeout, handshakeTimeout))); err != nil {
		s.warnPeer("set deadline failed", "remote", remote, "error", err)
		return
	}

	conn, reader, ok := s.startTLS(ctx, rawConn, remote)
	if !ok {
		return
	}

	req, err := protocol.ReadPrologue(reader)
	if err != nil {
		// A bare TCP connect that closes without sending anything is how
		// `clipd status` probes reachability, and how most port scanners
		// behave. It is not worth a warning.
		if errors.Is(err, io.EOF) {
			s.log.Debug("connection closed before request", "remote", remote)
			return
		}
		// A peer that has already blown its deadline gets nothing: writing a
		// response would reset the deadline and hand it another window, which
		// is exactly the hold-a-slot-open behaviour the deadline exists to
		// prevent.
		if isTimeout(err) {
			s.warnPeer("connection timed out before a request arrived", "remote", remote)
			return
		}
		s.warnPeer("malformed request", "remote", remote, "error", err)
		s.respond(conn, protocol.StatusMalformed, err.Error())
		return
	}

	if !auth.Compare(s.token, req.Token) {
		// Deliberately vague to the client, specific in the local log. There
		// is no attempt counting or lockout: a 256-bit token makes online
		// guessing pointless, and a lockout would just be a denial-of-service
		// lever. The admission limit above is about slot exhaustion, not
		// guessing, and applies equally to peers that never guess at all.
		s.warnPeer("authentication failed", "remote", remote)
		s.respond(conn, protocol.StatusAuthFailed, "authentication failed")
		return
	}
	// The peer has proved it holds the token, so it no longer counts against
	// the per-host unauthenticated limit. Releasing here rather than at return
	// is what keeps a user running many copies at once from throttling
	// themselves: only unproven connections are rationed.
	releaseSlot()

	// Now that the peer is authenticated it may compete for the copy budget.
	// Everything above this point was deliberately cheap; everything below
	// reads a payload into memory and forks a clipboard helper.
	workCtx, workCancel := context.WithTimeout(ctx, s.timeout)
	select {
	case s.sem <- struct{}{}:
		workCancel()
		defer func() { <-s.sem }()
	case <-workCtx.Done():
		workCancel()
		s.warnPeer("no copy capacity available", "remote", remote, "limit", cap(s.sem))
		s.respond(conn, protocol.StatusInternalError, "server is busy: too many copies in progress")
		return
	}

	// The unproven phase is over, so the peer gets the full configured window
	// for the exchange it came to perform.
	if err := conn.SetDeadline(time.Now().Add(s.timeout)); err != nil {
		s.warnPeer("set deadline failed", "remote", remote, "error", err)
		return
	}

	payloadLen, err := protocol.ReadPayloadLen(reader)
	if err != nil {
		s.warnPeer("malformed request", "remote", remote, "error", err)
		s.respond(conn, protocol.StatusMalformed, err.Error())
		return
	}

	// The length is client-supplied, so it is checked before a single byte of
	// the body is read or a buffer sized for it — otherwise the limit would
	// be a suggestion rather than a memory bound.
	if payloadLen > uint64(s.maxPayload) {
		s.warnPeer("payload rejected", "remote", remote,
			"declared_bytes", payloadLen, "limit_bytes", s.maxPayload)
		s.respond(conn, protocol.StatusPayloadTooLarge,
			fmt.Sprintf("payload of %d bytes exceeds the server limit of %d bytes", payloadLen, s.maxPayload))
		return
	}

	// Reserve the memory before reading a byte of the body. Checking the
	// declared length against the payload limit bounds one copy; this bounds
	// all of them at once, which is the number that decides whether the daemon
	// survives a burst.
	//
	// The wait is bounded by the base timeout rather than the payload deadline:
	// a peer waiting on a busy server should be told so while its client is
	// still listening, not held until the transfer window it never got to use
	// runs out.
	memCtx, memCancel := context.WithTimeout(ctx, s.timeout)
	err = s.mem.acquire(memCtx, int64(payloadLen))
	memCancel()
	if err != nil {
		s.warnPeer("payload rejected for lack of memory budget", "remote", remote,
			"declared_bytes", payloadLen, "in_use_bytes", s.mem.inUse(), "error", err)
		// StatusInternalError rather than a new status code: this is the
		// server declining to do the work, which is what that status already
		// means to every client in existence. A new code would read as
		// "unexpected server status" on anything not yet upgraded.
		s.respond(conn, protocol.StatusInternalError,
			"server is busy: not enough memory budget for a payload this size")
		return
	}
	defer s.mem.release(int64(payloadLen))

	if err := conn.SetReadDeadline(time.Now().Add(s.payloadTimeout(payloadLen))); err != nil {
		s.warnPeer("set read deadline failed", "remote", remote, "error", err)
		return
	}

	payload, err := protocol.ReadPayload(reader, payloadLen)
	if err != nil {
		s.warnPeer("payload read failed", "remote", remote, "error", err)
		if isTimeout(err) {
			return
		}
		s.respond(conn, protocol.StatusMalformed, err.Error())
		return
	}

	// Bound the clipboard write so a wedged helper cannot pin this handler.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()
	if err := s.clip.Write(writeCtx, payload); err != nil {
		s.log.Error("clipboard write failed", "remote", remote, "error", err)
		s.respond(conn, protocol.StatusInternalError, "clipboard write failed")
		return
	}

	// Debug, not Info. launchd appends this file forever with no rotation, so
	// a line per copy is unbounded growth that buries the rare failure in
	// routine success. The client already reports a successful copy at the
	// point of use with -v, which is where the answer is actually wanted;
	// `clipd serve -v` turns these back on when diagnosing the daemon.
	s.log.Debug("clipboard updated", "remote", remote, "bytes", len(payload))
	s.respond(conn, protocol.StatusOK, fmt.Sprintf("copied %d bytes", len(payload)))
}

// startTLS completes the handshake, returning the encrypted connection and a
// reader over it. It reports false when the connection has been dealt with and
// the caller should stop.
//
// Before handing anything to crypto/tls it sniffs the first four bytes. A v1
// client speaks the frame protocol directly, and letting that hit the TLS
// handshake produces "first record does not look like a TLS handshake" on this
// side and a bare connection close on the other — leaving the user to guess.
// Recognising the magic costs one Peek and turns the most likely upgrade
// failure into a sentence that says what to do.
func (s *Server) startTLS(ctx context.Context, rawConn net.Conn, remote string) (net.Conn, *bufio.Reader, bool) {
	sniff := bufio.NewReader(rawConn)

	head, err := sniff.Peek(len(protocol.Magic))
	if err != nil {
		switch {
		case errors.Is(err, io.EOF):
			// A bare connect that closes without sending: port scanners, and
			// anything probing whether the port is open.
			s.log.Debug("connection closed before handshake", "remote", remote)
		case isTimeout(err):
			s.warnPeer("connection timed out before the handshake", "remote", remote)
		default:
			s.warnPeer("read failed before handshake", "remote", remote, "error", err)
		}
		return nil, nil, false
	}

	if [4]byte(head) == protocol.Magic {
		s.warnPeer("rejected an unencrypted request", "remote", remote)
		// Answered in the clear, because that is the only language this peer
		// speaks. It carries no secret — just an instruction.
		if err := rawConn.SetWriteDeadline(time.Now().Add(s.timeout)); err == nil {
			_ = protocol.WriteResponse(rawConn, protocol.StatusMalformed,
				"this daemon requires TLS; upgrade clipd on the client machine (see clipd version)")
		}
		return nil, nil, false
	}

	// The peeked bytes have been consumed from rawConn, so the handshake gets
	// a wrapper that replays them before continuing.
	conn := tls.Server(&peekedConn{Conn: rawConn, r: sniff}, s.tlsConfig)
	if err := conn.HandshakeContext(ctx); err != nil {
		s.warnPeer("TLS handshake failed", "remote", remote, "error", err)
		return nil, nil, false
	}
	return conn, bufio.NewReader(conn), true
}

// peekedConn is a net.Conn whose reads come from a reader that has already
// buffered some of the stream.
type peekedConn struct {
	net.Conn
	r io.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// respond writes a status frame, refreshing the deadline first so a slow
// reader on the client side cannot make the acknowledgement hang.
func (s *Server) respond(conn net.Conn, status protocol.Status, message string) {
	if err := conn.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil {
		return
	}
	if err := protocol.WriteResponse(conn, status, message); err != nil {
		s.log.Debug("response write failed", "remote", conn.RemoteAddr().String(), "error", err)
	}
}

// payloadTimeout scales the read deadline with the declared payload size.
func (s *Server) payloadTimeout(n uint64) time.Duration {
	return s.timeout + time.Duration(n/minThroughput)*time.Second
}

// isTimeout reports whether an error came from an expired deadline.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
