// Package server implements the clipd daemon: accept a connection from the
// listening socket, decide what kind of message it is, and carry it out.
//
// The daemon runs in the foreground and never self-daemonizes. On macOS,
// launchd owns backgrounding, log redirection and restart-on-crash, so
// duplicating any of that here would only add ways to disagree with launchd.
//
// SSH protects forwarded traffic and UNIX socket permissions authorize access.
// Connected peers remain untrusted, so payloads, work and time are bounded.
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
	"unicode/utf8"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/config"
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

	// writeTimeout bounds writing a reply to a peer that has stopped reading,
	// and a clipboard helper that has wedged. Reads have no idle bound: a
	// producer such as sort or a build can be silent for minutes and still
	// finish, so silence is not evidence of failure.
	writeTimeout = 30 * time.Second

	// maxConnLifetime is the default absolute cap on one connection.
	//
	// Generous, because a large drop over a slow link is legitimately slow:
	// 256 MiB at a megabit takes most of half an hour. Together with the
	// connection cap it bounds what a silent or dripping peer can hold.
	maxConnLifetime = 30 * time.Minute

	// replyReserve is the end of a connection's lifetime kept back from reading
	// and work, so a transfer that runs out of time can still be told why
	// without the lifetime itself being extended.
	replyReserve = 5 * time.Second

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

	MaxTransfer time.Duration

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
	maxTransfer  time.Duration
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
	if opts.MaxTransfer == 0 {
		opts.MaxTransfer = maxConnLifetime
	}
	if opts.MaxTransfer < time.Second || opts.MaxTransfer > 24*time.Hour {
		return nil, errors.New("server: transfer lifetime must be between one second and 24 hours")
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
		maxTransfer:  opts.MaxTransfer,
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
	hard := time.Now().Add(s.maxTransfer)
	readEnd := hard.Add(-min(replyReserve, s.maxTransfer/2))
	ctx, cancel := context.WithDeadline(ctx, readEnd)
	defer cancel()
	conn = &lifetimeConn{Conn: conn, hard: hard}

	if err := conn.SetReadDeadline(readEnd); err != nil {
		s.warnPeer("set deadline failed", "error", err)
		return
	}
	if err := conn.SetWriteDeadline(hard); err != nil {
		s.warnPeer("set deadline failed", "error", err)
		return
	}
	reader := bufio.NewReader(conn)

	structured, err := protocol.Sniff(reader)
	if err != nil {
		if isTimeout(err) {
			// lifetimeConn caps the reply at the same lifetime, so answering
			// hands the peer no extra time.
			s.warnPeer("connection timed out before a request arrived")
			s.respondError(conn, s.lifetimeExceeded())
			return
		}
		s.warnPeer("read failed", "error", err)
		return
	}

	if structured {
		s.handleStructured(ctx, conn, reader)
		return
	}
	// A raw stream is always work: it buffers a payload and forks pbcopy.
	if !s.acquire(ctx, conn) {
		return
	}
	defer func() { <-s.sem }()
	s.handleClipboard(ctx, conn, reader)
}

// acquire takes a work slot, or tells the peer why it could not.
func (s *Server) acquire(ctx context.Context, conn net.Conn) bool {
	select {
	case s.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.respondError(conn, s.lifetimeExceeded())
		} else {
			s.respondError(conn, "clipd is shutting down. Try again in a moment.")
		}
		return false
	}
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
			s.respondError(conn, s.lifetimeExceeded())
			return
		}
		s.warnPeer("payload read failed", "error", err)
		return
	}
	if n > s.maxPayload {
		s.warnPeer("payload rejected", "limit_bytes", s.maxPayload)
		s.respondError(conn, fmt.Sprintf("input is larger than the %s clipboard limit; send it as a file with clipd drop --name <file>, or raise max_payload_bytes%s",
			config.FormatSize(s.maxPayload), raiseLimit))
		drainRejected(conn, r)
		return
	}

	// Bound the clipboard write so a wedged helper cannot pin this handler.
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := s.clip.Write(writeCtx, buf.Bytes()); err != nil {
		s.warnPeer("clipboard write failed", "error", err)
		s.respondError(conn, "couldn't write to the clipboard; see clipd's log on the receiving computer")
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
func (s *Server) handleStructured(ctx context.Context, conn net.Conn, r *bufio.Reader) {
	req, err := protocol.ReadRequest(r)
	if err != nil {
		if isTimeout(err) {
			s.warnPeer("connection timed out mid-frame")
			s.respondError(conn, s.lifetimeExceeded())
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
	case protocol.TypeDrop, protocol.TypeStreamDrop:
		s.handleDrop(ctx, conn, r, req)
	default:
		s.warnPeer("unknown request type", "type", req.Type)
		s.respondError(conn, fmt.Sprintf("unknown request type %q", req.Type))
	}
}

// handleDrop extracts the tar stream following the envelope into the drop
// directory.
func (s *Server) handleDrop(ctx context.Context, conn net.Conn, r io.Reader, req protocol.Request) {
	if s.dropDir == "" {
		s.respondError(conn, "drop is not configured on this daemon")
		return
	}
	started := time.Now()
	opts := drop.Options{
		Dir:      s.dropDir,
		Context:  ctx,
		MaxBytes: s.maxDropBytes,
		MaxFiles: s.maxDropFiles,
	}
	if req.Progress {
		last := time.Time{}
		publishing := false
		opts.PublishProgress = func(done, total int) {
			if publishing && time.Since(last) < time.Second {
				return
			}
			publishing = true
			last = time.Now()
			s.respond(conn, fmt.Sprintf("clipd: progress: publishing %d / %d files; elapsed %s", done, total, time.Since(started).Round(time.Second)))
		}
		previousName := ""
		var previous, transferred int64
		opts.Progress = func(name string, received, total int64) {
			if name != previousName {
				previousName = name
				previous = 0
			}
			transferred += received - previous
			previous = received
			if time.Since(last) < time.Second {
				return
			}
			last = time.Now()
			percent := int64(100)
			if total > 0 {
				percent = received * 100 / total
			}
			elapsed := time.Since(started)
			rate := float64(transferred) / elapsed.Seconds()
			eta := time.Duration(0)
			if rate > 0 {
				eta = time.Duration(float64(total-received) / rate * float64(time.Second))
			}
			if total < 0 {
				s.respond(conn, fmt.Sprintf("clipd: progress: %s: %d bytes %.1f MiB/s elapsed %s", name, received, rate/(1<<20), elapsed.Round(time.Second)))
			} else {
				s.respond(conn, fmt.Sprintf("clipd: progress: %s: %d%% (%d / %d bytes) %.1f MiB/s elapsed %s ETA %s", name, percent, received, total, rate/(1<<20), elapsed.Round(time.Second), eta.Round(time.Second)))
			}
		}
	}
	// A named drop is one file's bytes, sent by a pipeline that had no file to
	// hand to tar. A streaming drop is an archive whose names come from inside
	// it. Exactly one of these may run: both read the same stream to its end.
	var (
		res drop.Result
		err error
	)
	switch {
	case req.Type == protocol.TypeDrop && req.Name != "":
		res, err = drop.Save(r, req.Name, opts)
	case req.Type == protocol.TypeStreamDrop && req.Name == "":
		res, err = drop.Extract(r, opts)
	case req.Type == protocol.TypeDrop:
		// An archive under the original drop type comes from a shell function
		// generated before streaming drops. Its format is no longer accepted,
		// so the sender is told how to update rather than having its archive
		// read as something else.
		s.warnPeer("drop rejected", "reason", "archive drop from an outdated clipd function")
		s.respondError(conn, "this host's clipd function is out of date; on the Mac, run clipd setup for this host")
		drainRejected(conn, r)
		return
	default:
		s.respondError(conn, "streaming named input is not supported")
		return
	}
	if err != nil {
		// Warn rather than Error: every one of these is caused by what a peer
		// sent, so they are rate-limited like the rest of the peer-driven
		// lines. Error names are validated relative paths or bounded quoted names;
		// payload contents never appear here.
		s.warnPeer("drop rejected", "error", err)
		s.respondError(conn, s.dropFailure(err))
		drainRejected(conn, r)
		return
	}
	// Routine success is acknowledged to the sender. Keeping it at Debug avoids
	// growing launchd's unrotated log by one line for every ordinary drop.
	s.log.Debug("files dropped", "count", len(res.Names), "bytes", res.Bytes, "dir", s.dropDir)
	reply := fmt.Sprintf("dropped %s (%s) into %s in %s",
		dropSummary(res.Names), config.FormatSize(res.Bytes), s.dropDir, time.Since(started).Round(time.Millisecond))
	switch {
	case res.Skipped == 1:
		reply += "; skipped 1 symlink or other special file"
	case res.Skipped > 1:
		reply += fmt.Sprintf("; skipped %d symlinks or other special files", res.Skipped)
	}
	s.respondOK(conn, reply)
}

// raiseLimit ends every reply about a configurable limit.
const raiseLimit = " in the Mac's clipd config, then run clipd restart"

// dropFailure names the setting to raise when a drop hits a configured limit.
func (s *Server) dropFailure(err error) string {
	switch {
	case errors.Is(err, drop.ErrTooLarge):
		return fmt.Sprintf("drop is larger than the %s limit; raise max_drop_bytes%s", config.FormatSize(s.maxDropBytes), raiseLimit)
	case errors.Is(err, drop.ErrTooManyFiles):
		return fmt.Sprintf("drop has more than %d files; raise max_drop_files%s", s.maxDropFiles, raiseLimit)
	case isTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return s.lifetimeExceeded()
	}
	return err.Error()
}

// lifetimeExceeded names the setting behind a connection's time limit.
func (s *Server) lifetimeExceeded() string {
	return fmt.Sprintf("transfer took longer than the %s limit; raise max_transfer_seconds%s", config.FormatDuration(s.maxTransfer), raiseLimit)
}

// dropSummary bounds replies independently of the configured file count.
func dropSummary(names []string) string {
	if len(names) <= 8 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:8], ", "), len(names)-8)
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
// Bounded in time as well as bytes, so a sender dripping bytes cannot hold the
// work slot for the rest of the connection's lifetime. lifetimeConn keeps the
// drain deadline from outliving the connection.
func drainRejected(conn net.Conn, r io.Reader) {
	_ = conn.SetReadDeadline(time.Now().Add(drainTimeout))
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
	if len(line) > protocol.MaxFrameBytes {
		line = line[:protocol.MaxFrameBytes-4]
		for !utf8.ValidString(line) {
			line = line[:len(line)-1]
		}
		line += "..."
	}
	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return
	}
	if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
		s.log.Debug("response write failed", "error", err)
	}
}

// lifetimeConn prevents any later deadline, for a reply, progress or a drain,
// from extending a connection's absolute lifetime.
type lifetimeConn struct {
	net.Conn
	hard time.Time
}

func (c *lifetimeConn) clamp(t time.Time) time.Time {
	if t.IsZero() || t.After(c.hard) {
		return c.hard
	}
	return t
}
func (c *lifetimeConn) SetDeadline(t time.Time) error     { return c.Conn.SetDeadline(c.clamp(t)) }
func (c *lifetimeConn) SetReadDeadline(t time.Time) error { return c.Conn.SetReadDeadline(c.clamp(t)) }
func (c *lifetimeConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(c.clamp(t))
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
