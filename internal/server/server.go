// Package server implements the clipd daemon: accept a connection from the
// listening socket, decide what kind of message it is, and carry it out.
//
// The daemon runs in the foreground and never self-daemonizes. On macOS,
// launchd owns backgrounding, log redirection and restart-on-crash, so
// duplicating any of that here would only add ways to disagree with launchd.
//
// # What this package is not responsible for
//
// There is no authentication, no encryption and no peer identity, and their
// absence is the design rather than a gap in it. The daemon listens on a UNIX
// domain socket in the user's home directory, and that socket reaches other
// machines only by being forwarded over SSH. By the time bytes arrive here,
// SSH has already encrypted the channel, verified the host key, and
// authenticated the user, and the filesystem permissions on the socket have
// already decided who is allowed to write to it. A token checked at this layer
// would be a second, weaker copy of a decision that has already been made
// correctly — and a token stored on the remote machine to satisfy it would be
// a stealable secret where today there is only an ephemeral capability.
//
// What remains here is resource control. Every peer is authorised, but an
// authorised peer can still be a runaway loop, so connections, concurrency,
// payload size and time are all bounded.
package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/drop"
	"github.com/colefailla/clipd/internal/protocol"
)

const (
	// defaultMaxConcurrent bounds the messages being carried out at once:
	// buffering a clipboard payload and forking pbcopy, or extracting an
	// archive to disk.
	//
	// Small, because the peers are the user's own SSH sessions rather than a
	// network of clients. It is also the second half of the daemon's memory
	// bound: a clipboard payload is buffered whole, so the ceiling is roughly
	// this times the payload limit — about 80 MiB at the defaults, and
	// transiently more, since the buffer grows by doubling and briefly holds
	// both halves while it does. Raising the payload limit a long way is a
	// reason to lower this. A drop is streamed to disk and costs nothing here.
	defaultMaxConcurrent = 8

	// defaultMaxConnections bounds sockets in any state, the backstop against
	// unbounded goroutine growth if something on the far side opens
	// connections faster than it finishes them.
	defaultMaxConnections = 64

	// idleTimeout bounds how long a connection may go without producing a
	// byte.
	//
	// It is an idle deadline rather than a total one because the two failure
	// modes look nothing alike: a large paste over a slow link is legitimately
	// slow but always progressing, while a peer that opens a socket and says
	// nothing is holding a slot for free. Extending the deadline on every read
	// distinguishes them without having to guess a transfer rate.
	idleTimeout = 30 * time.Second

	// shutdownGrace is how long the daemon waits for in-flight work before
	// giving up on it.
	shutdownGrace = 5 * time.Second

	// maxAcceptBackoff caps the retry delay after a transient accept failure.
	maxAcceptBackoff = time.Second

	// warnWindow and warnBudget bound how many peer-driven warnings reach the
	// log per window.
	//
	// launchd appends the daemon's log file forever with no rotation, and every
	// rejected message would otherwise write a line: a loop sending malformed
	// frames turns "the clipboard is unavailable" into "the disk is full".
	// Suppressed lines are counted and reported, so throttling hides the volume
	// of a problem, never the fact of one.
	warnWindow = time.Minute
	warnBudget = 20
)

// Options configures a Server.
type Options struct {
	// Clipboard receives raw messages. Required.
	Clipboard clipboard.Clipboard

	// DropDir is where drop requests write their files. Required for drops;
	// when empty, drop requests are refused and everything else still works.
	DropDir string

	// MaxPayload caps a single clipboard message. Required.
	MaxPayload int64

	// MaxDropBytes and MaxDropFiles cap one drop. Zero means the drop
	// package's defaults.
	MaxDropBytes int64
	MaxDropFiles int

	// MaxConcurrent bounds simultaneous work. Zero means defaultMaxConcurrent.
	MaxConcurrent int

	// Logger receives operational logs. Clipboard and file contents are never
	// logged.
	Logger *slog.Logger
}

// Server accepts clipd connections. The zero value is not usable; use New.
type Server struct {
	clip         clipboard.Clipboard
	dropDir      string
	maxPayload   int64
	maxDropBytes int64
	maxDropFiles int
	log          *slog.Logger

	// connSem bounds sockets in any state; sem bounds messages actually being
	// carried out.
	connSem chan struct{}
	sem     chan struct{}

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
// does not matter for a limiter whose only job is to keep a misbehaving peer
// from growing a file without bound.
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
	if opts.Clipboard == nil {
		return nil, errors.New("server: no clipboard backend")
	}
	if opts.MaxPayload < 1 {
		return nil, fmt.Errorf("server: max payload %d must be positive", opts.MaxPayload)
	}
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent < 1 {
		maxConcurrent = defaultMaxConcurrent
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Server{
		clip:         opts.Clipboard,
		dropDir:      opts.DropDir,
		maxPayload:   opts.MaxPayload,
		maxDropBytes: opts.MaxDropBytes,
		maxDropFiles: opts.MaxDropFiles,
		log:          logger,
		sem:          make(chan struct{}, maxConcurrent),
		// Never below maxConcurrent, or work slots would be unreachable: every
		// message holds a connection slot for its whole life.
		connSem: make(chan struct{}, max(defaultMaxConnections, maxConcurrent)),
	}, nil
}

// warnPeer logs a warning caused by a remote peer, subject to the rate budget.
//
// Every warning in a connection handler goes through here rather than to the
// logger directly: they are all reachable by anything that can write to the
// socket, and so are all capable of growing an unrotated file without bound.
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

// Serve accepts connections until ctx is cancelled or the listener fails.
//
// It closes ln before returning. A cancelled context is a clean shutdown and
// yields a nil error.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		// The contract is that Serve closes ln before returning; keeping it
		// open here would leave the socket bound and connections piling up in
		// a backlog nobody drains.
		_ = ln.Close()
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

	var backoff time.Duration
	for {
		// Acquire capacity before accepting. Blocking here applies back
		// pressure through the kernel's accept queue instead of piling up
		// goroutines for connections we are not ready to service.
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
			// A transient accept error should not kill a daemon the user
			// expects to stay up until launchd stops it. ECONNABORTED is
			// retried inside the runtime and never surfaces here; what does
			// surface is fd exhaustion (EMFILE/ENFILE), which the daemon rides
			// out with the same capped backoff net/http uses.
			if isTemporaryAcceptErr(err) {
				if backoff == 0 {
					backoff = 5 * time.Millisecond
				} else {
					backoff = min(backoff*2, maxAcceptBackoff)
				}
				s.log.Warn("accept failed; retrying", "error", err, "backoff", backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return s.drain(ctx)
				}
				continue
			}
			return fmt.Errorf("accept: %w", err)
		}
		backoff = 0

		// A connection accepted in the instant before the listener closed
		// would otherwise register itself after Shutdown began waiting, which
		// is both a WaitGroup misuse and work that could be cut off mid-write.
		// Registering under the same lock that sets the closed flag makes the
		// two mutually exclusive.
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
	// bounded window to finish the message they are in the middle of.
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
// none of them panic, and none of them log clipboard or file contents.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// An idle deadline from the first byte, refreshed by the reader below on
	// every successful read. A peer that connects and says nothing cannot hold
	// a slot open; a peer that is genuinely sending can take as long as the
	// data takes.
	if err := conn.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
		s.warnPeer("set deadline failed", "error", err)
		return
	}
	reader := bufio.NewReader(&idleReader{conn: conn, r: conn})

	structured, err := protocol.Sniff(reader)
	if err != nil {
		if isTimeout(err) {
			// A peer that blew its deadline gets no response: writing one
			// would refresh the deadline and hand it another window, which is
			// the slot-holding behaviour the deadline exists to prevent.
			s.warnPeer("connection timed out before a request arrived")
			return
		}
		s.warnPeer("read failed", "error", err)
		return
	}

	// Capacity is acquired here, once it is known there is real work. Both
	// paths below either buffer a payload in memory or write files to disk.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.respond(conn, "busy: shutting down")
		return
	}

	if structured {
		s.handleStructured(ctx, conn, reader)
		return
	}
	s.handleClipboard(ctx, conn, reader)
}

// handleClipboard implements the default path: everything on the connection is
// clipboard content, read verbatim to EOF.
//
// This is the case that makes `nc` a complete client, so it must stay the
// behaviour for any stream that does not explicitly ask for something else.
func (s *Server) handleClipboard(ctx context.Context, conn net.Conn, r io.Reader) {
	// One byte past the limit, so an oversized payload is detected rather than
	// silently truncated onto the clipboard. There is no declared length to
	// check first — the stream ends when the peer half-closes — so the cap has
	// to be enforced on what actually arrives.
	var buf bytes.Buffer
	n, err := io.CopyN(&buf, r, s.maxPayload+1)
	if err != nil && !errors.Is(err, io.EOF) {
		if isTimeout(err) {
			s.warnPeer("connection timed out mid-payload")
			return
		}
		s.warnPeer("payload read failed", "error", err)
		return
	}
	if n > s.maxPayload {
		s.warnPeer("payload rejected", "limit_bytes", s.maxPayload)
		s.respond(conn, fmt.Sprintf("payload exceeds the %d byte limit", s.maxPayload))
		return
	}

	// Bound the clipboard write so a wedged helper cannot pin this handler.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idleTimeout)
	defer cancel()
	if err := s.clip.Write(writeCtx, buf.Bytes()); err != nil {
		s.log.Error("clipboard write failed", "error", err)
		s.respond(conn, "clipboard write failed")
		return
	}

	// Debug, not Info. launchd appends this file forever with no rotation, so
	// a line per copy is unbounded growth that buries the rare failure in
	// routine success. The acknowledgement below already tells the user, at
	// the point of use, which is where the answer is actually wanted.
	s.log.Debug("clipboard updated", "bytes", buf.Len())
	s.respond(conn, fmt.Sprintf("copied %d bytes", buf.Len()))
}

// handleStructured reads the JSON envelope after the magic prefix and
// dispatches on its type.
func (s *Server) handleStructured(ctx context.Context, conn net.Conn, r *bufio.Reader) {
	req, err := protocol.ReadRequest(r)
	if err != nil {
		if isTimeout(err) {
			s.warnPeer("connection timed out mid-frame")
			return
		}
		s.warnPeer("malformed request", "error", err)
		s.respond(conn, err.Error())
		return
	}

	switch req.Type {
	case protocol.TypeDrop:
		s.handleDrop(conn, r)
	default:
		s.warnPeer("unknown request type", "type", req.Type)
		s.respond(conn, fmt.Sprintf("unknown request type %q", req.Type))
	}
}

// handleDrop extracts the tar stream following the envelope into the drop
// directory.
func (s *Server) handleDrop(conn net.Conn, r io.Reader) {
	if s.dropDir == "" {
		s.respond(conn, "drop is not configured on this daemon")
		return
	}
	res, err := drop.Extract(r, drop.Options{
		Dir:      s.dropDir,
		MaxBytes: s.maxDropBytes,
		MaxFiles: s.maxDropFiles,
	})
	if err != nil {
		// Warn rather than Error: every one of these is caused by what a peer
		// sent, so they are rate-limited like the rest of the peer-driven
		// lines. The names are safe to log — they have already been reduced
		// to basenames — but the contents never are, and never appear here.
		s.warnPeer("drop rejected", "error", err)
		s.respond(conn, err.Error())
		return
	}
	s.log.Info("files dropped", "count", len(res.Names), "bytes", res.Bytes, "dir", s.dropDir)
	s.respond(conn, fmt.Sprintf("dropped %s (%d bytes) into %s",
		strings.Join(res.Names, ", "), res.Bytes, s.dropDir))
}

// respond writes a single human-readable line back to the peer.
//
// The client is `nc`, which prints whatever it receives and exits once the
// far end closes. That makes this line the entire user interface for the
// result: without it a copy is silent whether it worked or not, and the
// connection appears to hang until the client is told to half-close. Keeping
// it plain text rather than JSON is what lets an unmodified `nc` display it.
func (s *Server) respond(conn net.Conn, message string) {
	if err := conn.SetWriteDeadline(time.Now().Add(idleTimeout)); err != nil {
		return
	}
	if _, err := fmt.Fprintf(conn, "clipd: %s\n", message); err != nil {
		s.log.Debug("response write failed", "error", err)
	}
}

// idleReader refreshes the connection deadline on every successful read, so
// that the deadline bounds silence rather than total transfer time.
type idleReader struct {
	conn net.Conn
	r    io.Reader
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		// Best effort: a failure to extend the deadline is not worth failing a
		// read that already succeeded. The old deadline still applies, so the
		// connection stays bounded either way.
		_ = i.conn.SetReadDeadline(time.Now().Add(idleTimeout))
	}
	return n, err
}

// isTimeout reports whether err is a deadline expiry.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isTemporaryAcceptErr recognises accept failures worth retrying rather than
// exiting over: file-descriptor exhaustion, and timeouts from any listener
// that happens to carry a deadline.
func isTemporaryAcceptErr(err error) bool {
	if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
