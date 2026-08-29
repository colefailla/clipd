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

	// maxConnLifetime is the absolute cap on one connection, whatever the idle
	// deadline says.
	//
	// Generous, because a large drop over a slow link is legitimately slow:
	// 256 MiB at a megabit takes most of half an hour. It exists for the case
	// the idle deadline cannot see — a peer that stays just inside the idle
	// window forever, holding a work slot for free while always "progressing".
	maxConnLifetime = 30 * time.Minute

	// drainTimeout bounds the rejection drain, which has no reason to be slow:
	// the sender is already transmitting, and the drain only exists to let the
	// rejection land before the socket closes.
	drainTimeout = 5 * time.Second

	// maxClipboardBudget caps what a busy daemon can be made to buffer at once.
	// A clipboard payload is held whole in memory, so the ceiling that matters
	// is the payload limit times the work slots, not either one alone.
	maxClipboardBudget int64 = 2 << 30

	// shutdownGrace is how long the daemon waits for in-flight work before
	// giving up on it.
	shutdownGrace = 5 * time.Second

	// maxAcceptBackoff caps the retry delay after a transient accept failure.
	maxAcceptBackoff = time.Second

	// drainCap bounds how much of a rejected message is read and thrown away
	// before the connection closes. See drainRejected.
	drainCap = 256 << 10

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
	// Divided rather than multiplied, so the check itself cannot overflow.
	if opts.MaxPayload > maxClipboardBudget/int64(maxConcurrent) {
		return nil, fmt.Errorf(
			"server: a %d byte payload across %d concurrent messages could buffer %d bytes, over the %d byte ceiling; lower max_payload_bytes or max_concurrent",
			opts.MaxPayload, maxConcurrent, opts.MaxPayload*int64(maxConcurrent), maxClipboardBudget)
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
	// data takes, up to that reader's absolute cap.
	if err := conn.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
		s.warnPeer("set deadline failed", "error", err)
		return
	}
	idle := newIdleReader(conn)
	reader := bufio.NewReader(idle)

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

	if structured {
		s.handleStructured(ctx, conn, idle, reader)
		return
	}
	// A raw stream is always work: it buffers a payload and forks pbcopy.
	if !s.acquire(ctx, conn) {
		return
	}
	defer func() { <-s.sem }()
	s.handleClipboard(ctx, conn, idle, reader)
}

// acquire takes a work slot, or tells the peer why it could not.
func (s *Server) acquire(ctx context.Context, conn net.Conn) bool {
	select {
	case s.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		s.respondError(conn, "busy: shutting down")
		return false
	}
}

// handleClipboard implements the default path: everything on the connection is
// clipboard content, read verbatim to EOF.
//
// This is the case that makes `nc` a complete client, so it must stay the
// behaviour for any stream that does not explicitly ask for something else.
func (s *Server) handleClipboard(ctx context.Context, conn net.Conn, idle *idleReader, r io.Reader) {
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
		s.respondError(conn, fmt.Sprintf("payload exceeds the %d byte limit", s.maxPayload))
		drainRejected(idle, r)
		return
	}

	// Bound the clipboard write so a wedged helper cannot pin this handler.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idleTimeout)
	defer cancel()
	if err := s.clip.Write(writeCtx, buf.Bytes()); err != nil {
		s.log.Error("clipboard write failed", "error", err)
		s.respondError(conn, "clipboard write failed")
		return
	}

	// Debug, not Info. launchd appends this file forever with no rotation, so
	// a line per copy is unbounded growth that buries the rare failure in
	// routine success. The acknowledgement below already tells the user, at
	// the point of use, which is where the answer is actually wanted.
	s.log.Debug("clipboard updated", "bytes", buf.Len())
	s.respondOK(conn, fmt.Sprintf("copied %d bytes", buf.Len()))
}

// handleStructured reads the JSON envelope after the magic prefix and
// dispatches on its type.
func (s *Server) handleStructured(ctx context.Context, conn net.Conn, idle *idleReader, r *bufio.Reader) {
	req, err := protocol.ReadRequest(r)
	if err != nil {
		if isTimeout(err) {
			s.warnPeer("connection timed out mid-frame")
			return
		}
		s.warnPeer("malformed request", "error", err)
		s.respondError(conn, err.Error())
		return
	}

	// Answered before a work slot is taken, because a ping does no work and the
	// moment a caller most needs an answer is when every slot is busy. It is
	// also not logged: `clipd status` sends one, and a line per status check
	// would grow a file launchd never rotates.
	if req.Type == protocol.TypePing {
		s.respondOK(conn, protocol.Pong)
		return
	}

	if !s.acquire(ctx, conn) {
		return
	}
	defer func() { <-s.sem }()

	switch req.Type {
	case protocol.TypeDrop:
		s.handleDrop(conn, idle, r, req.Name)
	default:
		s.warnPeer("unknown request type", "type", req.Type)
		s.respondError(conn, fmt.Sprintf("unknown request type %q", req.Type))
	}
}

// handleDrop extracts the tar stream following the envelope into the drop
// directory.
func (s *Server) handleDrop(conn net.Conn, idle *idleReader, r io.Reader, name string) {
	if s.dropDir == "" {
		s.respondError(conn, "drop is not configured on this daemon")
		return
	}
	opts := drop.Options{
		Dir:      s.dropDir,
		MaxBytes: s.maxDropBytes,
		MaxFiles: s.maxDropFiles,
	}
	// A name means the body is one file's bytes, sent by a pipeline that had
	// no file to hand to tar. Without one the body is an archive and the names
	// come from inside it. Exactly one of these may run: both read the same
	// stream to its end.
	var (
		res drop.Result
		err error
	)
	if name != "" {
		res, err = drop.Save(r, name, opts)
	} else {
		res, err = drop.Extract(r, opts)
	}
	if err != nil {
		// Warn rather than Error: every one of these is caused by what a peer
		// sent, so they are rate-limited like the rest of the peer-driven
		// lines. The names are safe to log — they have already been reduced
		// to basenames — but the contents never are, and never appear here.
		s.warnPeer("drop rejected", "error", err)
		s.respondError(conn, err.Error())
		drainRejected(idle, r)
		return
	}
	// Routine success is acknowledged to the sender. Keeping it at Debug avoids
	// growing launchd's unrotated log by one line for every ordinary drop.
	s.log.Debug("files dropped", "count", len(res.Names), "bytes", res.Bytes, "dir", s.dropDir)
	s.respondOK(conn, fmt.Sprintf("dropped %s (%d bytes) into %s",
		strings.Join(res.Names, ", "), res.Bytes, s.dropDir))
}

// drainRejected reads and discards what the sender is still transmitting, so
// that closing the connection does not destroy the rejection just written.
//
// A socket closed while data is still arriving is torn down abruptly, and the
// reply sitting in the sender's receive buffer goes with it. Whether the sender
// sees the message therefore depends on whether it happened to be idle at that
// instant — which in practice depends on how fast its disk is. The same command
// against the same directory reports the error six times out of ten and says
// nothing the other four. Reading the remainder first makes the close orderly
// and the message reliable.
//
// Bounded, because the alternative is unbounded: a sender 50 GB over the limit
// cannot be waited out, and reading it all would be a denial of service dressed
// as politeness. Past the cap the abrupt close is accepted and the message may
// still be lost. net/http makes the same trade at the same size, for the same
// reason.
//
// Bounded in time as well as bytes. Every read refreshes the idle deadline, so
// the byte cap alone says nothing about duration: a sender dripping one byte
// just inside each idle window reaches 256 KiB in about eighty-eight days, and
// holds a work slot for every one of them.
func drainRejected(idle *idleReader, r io.Reader) {
	idle.clamp(drainTimeout)
	_, _ = io.CopyN(io.Discard, r, drainCap)
}

// respondOK and respondError write the one line the peer sees.
//
// The status token in front of the message is what makes the result
// machine-readable. The reply used to be undifferentiated prose, so the
// generated shell function had no way to tell "dropped 3 files" from "archive
// exceeds the size limit" and every drop exited 0 either way — which made
// `clipd drop x && rm x` delete a file the daemon had refused.
func (s *Server) respondOK(conn net.Conn, message string) {
	s.respond(conn, protocol.StatusOK+message)
}

func (s *Server) respondError(conn net.Conn, message string) {
	s.respond(conn, protocol.StatusError+message)
}

// respond writes a single human-readable line back to the peer.
//
// The client is `nc`, which prints whatever it receives and exits once the
// far end closes. That makes this line the entire user interface for the
// result: without it a copy is silent whether it worked or not, and the
// connection appears to hang until the client is told to half-close. Keeping
// it plain text rather than JSON is what lets an unmodified `nc` display it.
func (s *Server) respond(conn net.Conn, line string) {
	if err := conn.SetWriteDeadline(time.Now().Add(idleTimeout)); err != nil {
		return
	}
	if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
		s.log.Debug("response write failed", "error", err)
	}
}

// idleReader refreshes the connection deadline on every successful read, so
// that the deadline bounds silence rather than total transfer time — and caps
// the connection as a whole, so that "always progressing" is not the same as
// "allowed to run forever".
type idleReader struct {
	conn net.Conn
	r    io.Reader

	// hard is the absolute end of this connection and is never extended, only
	// brought forward. Refreshing on every read is the right answer for a slow
	// but honest transfer and the wrong one for a peer that sends a byte just
	// inside each window: that peer progresses for ever, and holds a work slot
	// the whole time.
	hard time.Time
}

func newIdleReader(conn net.Conn) *idleReader {
	return &idleReader{conn: conn, r: conn, hard: time.Now().Add(maxConnLifetime)}
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		// Best effort: a failure to extend the deadline is not worth failing a
		// read that already succeeded. The old deadline still applies, so the
		// connection stays bounded either way.
		_ = i.conn.SetReadDeadline(i.next(idleTimeout))
	}
	return n, err
}

// next returns the earlier of "d from now" and the connection's hard deadline.
func (i *idleReader) next(d time.Duration) time.Time {
	if t := time.Now().Add(d); t.Before(i.hard) {
		return t
	}
	return i.hard
}

// clamp brings the hard deadline forward, for a phase of the connection that
// should be over quickly whatever the idle timeout would otherwise allow.
func (i *idleReader) clamp(d time.Duration) {
	if t := time.Now().Add(d); t.Before(i.hard) {
		i.hard = t
	}
	_ = i.conn.SetReadDeadline(i.hard)
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
