package server

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/drop"
	"github.com/colefailla/clipd/internal/protocol"
)

// shortTempDir returns a temp directory with a deliberately short path.
//
// t.TempDir() embeds the test's name, and macOS caps a UNIX socket path at
// around 104 bytes: a table-driven subtest name is enough to push a socket in
// t.TempDir() past the limit and fail with "invalid argument", which is not an
// obvious symptom.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "clipd")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// harness is a running daemon on a UNIX socket, with a fake clipboard.
type harness struct {
	t       *testing.T
	srv     *Server
	socket  string
	dropDir string
	clip    *clipboard.Fake
	done    chan struct{}
}

type harnessOption func(*Options)

func withMaxPayload(n int64) harnessOption { return func(o *Options) { o.MaxPayload = n } }
func withDropLimits(b int64, f int) harnessOption {
	return func(o *Options) { o.MaxDropBytes, o.MaxDropFiles = b, f }
}
func withoutDropDir() harnessOption { return func(o *Options) { o.DropDir = "" } }

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()

	dir := shortTempDir(t)
	h := &harness{
		t:       t,
		socket:  filepath.Join(dir, "s.sock"),
		dropDir: filepath.Join(dir, "drop"),
		clip:    &clipboard.Fake{},
		done:    make(chan struct{}),
	}

	options := Options{
		Clipboard:  h.clip,
		DropDir:    h.dropDir,
		MaxPayload: 1 << 20,
	}
	for _, opt := range opts {
		opt(&options)
	}

	srv, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.srv = srv

	ln, err := Listen(h.socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		defer close(h.done)
		_ = srv.Serve(context.Background(), ln)
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-h.done
	})
	return h
}

// send writes payload to the socket, half-closes, and returns the daemon's
// acknowledgement — the same exchange `nc -N -U` performs.
func (h *harness) send(payload []byte) string {
	h.t.Helper()
	conn, err := net.DialTimeout("unix", h.socket, 5*time.Second)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		h.t.Fatalf("deadline: %v", err)
	}
	// A write error is not fatal. When the daemon rejects a message it stops
	// reading, and a sender far enough past the limit will see the connection
	// go away mid-write — which is exactly what nc does, and nc goes on to read
	// whatever reply arrived. Failing the test here would test the harness
	// rather than the daemon.
	_, _ = conn.Write(payload)
	// Half-close so the daemon sees EOF, which is what tells it the message is
	// complete. This is exactly what nc's -N does.
	_ = conn.(*net.UnixConn).CloseWrite()
	reply, err := io.ReadAll(conn)
	if err != nil {
		h.t.Fatalf("read reply: %v", err)
	}
	return strings.TrimSpace(string(reply))
}

// tarOf builds an archive of name/body pairs.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

// dropRequest frames an archive as a streaming drop, the way the remote shell
// function does with printf, tar and the completion marker.
func dropRequest(archive []byte) []byte {
	request := append([]byte(protocol.Magic+`{"type":"`+protocol.TypeStreamDrop+`"}`+"\n"), archive...)
	return append(request, drop.Completion...)
}

// namedDropRequest frames raw bytes as a drop under one name, the way the
// shell function does when stdin is a pipe and there is no file to tar.
func namedDropRequest(name string, body []byte) []byte {
	head := protocol.Magic + `{"type":"drop","name":"` + name + `"}` + "\n"
	return append([]byte(head), body...)
}

func TestRawStreamReachesTheClipboard(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	reply := h.send([]byte("hello from the remote"))

	if got := string(h.clip.Data()); got != "hello from the remote" {
		t.Errorf("clipboard = %q, want the payload", got)
	}
	if !strings.Contains(reply, "copied 21 bytes") {
		t.Errorf("reply = %q, want a byte count", reply)
	}
}

// TestContentIsVerbatim is the promise the whole tool rests on: what you pipe
// in is what you paste, including the trailing newline and any binary bytes.
func TestContentIsVerbatim(t *testing.T) {
	t.Parallel()

	payload := []byte("line one\nline two\n\t tabbed \x00\xff\n")
	h := newHarness(t)
	h.send(payload)

	if got := h.clip.Data(); !bytes.Equal(got, payload) {
		t.Errorf("clipboard = %q, want %q", got, payload)
	}
}

func TestEmptyPayloadIsAccepted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	reply := h.send(nil)

	if len(h.clip.Data()) != 0 {
		t.Errorf("clipboard = %q, want empty", h.clip.Data())
	}
	if !strings.Contains(reply, "copied 0 bytes") {
		t.Errorf("reply = %q, want a zero byte count", reply)
	}
}

func TestOversizedPayloadIsRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withMaxPayload(1024))
	reply := h.send(bytes.Repeat([]byte("x"), 4096))

	if h.clip.WriteCount() != 0 {
		t.Error("an oversized payload reached the clipboard")
	}
	if !strings.Contains(reply, "1 KiB") || !strings.Contains(reply, "max_payload_bytes") {
		t.Errorf("reply = %q, want it to name the limit and its setting", reply)
	}
}

func TestPayloadOfExactlyTheLimitIsAccepted(t *testing.T) {
	t.Parallel()

	const limit = 4096
	h := newHarness(t, withMaxPayload(limit))
	h.send(bytes.Repeat([]byte("x"), limit))

	if len(h.clip.Data()) != limit {
		t.Errorf("clipboard holds %d bytes, want %d", len(h.clip.Data()), limit)
	}
}

func TestDropWritesFiles(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	reply := h.send(dropRequest(tarOf(t, map[string]string{
		"notes.txt": "first",
		"other.md":  "second",
	})))

	if !strings.Contains(reply, "dropped") {
		t.Fatalf("reply = %q, want a drop confirmation", reply)
	}
	for name, want := range map[string]string{"notes.txt": "first", "other.md": "second"} {
		got, err := os.ReadFile(filepath.Join(h.dropDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if h.clip.WriteCount() != 0 {
		t.Error("a drop reached the clipboard")
	}
}

// TestDropCannotEscapeTheDropDirectory is the end-to-end version of the
// extraction tests: a hostile archive arriving over a real socket is refused,
// and nothing is written inside the drop directory or beside it.
func TestDropCannotEscapeTheDropDirectory(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	reply := h.send(dropRequest(tarOf(t, map[string]string{
		"../../escaped.txt": "no",
	})))
	if !strings.HasPrefix(reply, protocol.StatusError) {
		t.Fatalf("reply = %q, want the hostile drop refused", reply)
	}

	parent := filepath.Dir(h.dropDir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "escaped.txt" {
			t.Fatal("a drop wrote outside the drop directory")
		}
	}
	if _, err := os.Stat(filepath.Join(h.dropDir, "escaped.txt")); !os.IsNotExist(err) {
		t.Errorf("a refused drop was published: %v", err)
	}
}

// TestOutdatedArchiveDropIsRefused covers a remote whose shell function predates
// streaming drops: its archive must be refused with the fix, not extracted or
// mistaken for clipboard data.
func TestOutdatedArchiveDropIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	outdated := append([]byte(protocol.Magic+`{"type":"drop"}`+"\n"), tarOf(t, map[string]string{"notes.txt": "x"})...)
	reply := h.send(outdated)
	if !strings.HasPrefix(reply, protocol.StatusError) || !strings.Contains(reply, "run clipd setup") {
		t.Fatalf("reply = %q, want an error that says to rerun setup", reply)
	}
	if entries, _ := os.ReadDir(h.dropDir); len(entries) != 0 {
		t.Fatalf("an outdated drop published %d entries", len(entries))
	}
	if h.clip.WriteCount() != 0 {
		t.Fatal("an outdated drop reached the clipboard")
	}
}

func TestDropRespectsLimits(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withDropLimits(100, 2))
	reply := h.send(dropRequest(tarOf(t, map[string]string{
		"big.bin": strings.Repeat("x", 500),
	})))

	if !strings.Contains(reply, "100 bytes") || !strings.Contains(reply, "max_drop_bytes") {
		t.Errorf("reply = %q, want it to name the size limit and its setting", reply)
	}

	reply = h.send(dropRequest(tarOf(t, map[string]string{"a": "1", "b": "2", "c": "3"})))
	if !strings.Contains(reply, "more than 2 files") || !strings.Contains(reply, "max_drop_files") {
		t.Errorf("reply = %q, want it to name the file limit and its setting", reply)
	}
}

func TestDropWithoutADropDirectoryIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withoutDropDir())
	reply := h.send(dropRequest(tarOf(t, map[string]string{"x.txt": "y"})))

	if !strings.Contains(reply, "not configured") {
		t.Errorf("reply = %q, want a not-configured message", reply)
	}
}

func TestUnknownRequestTypeIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	reply := h.send([]byte(protocol.Magic + `{"type":"teleport"}` + "\n"))

	if !strings.Contains(reply, "unknown request type") {
		t.Errorf("reply = %q, want an unknown-type message", reply)
	}
	if h.clip.WriteCount() != 0 {
		t.Error("an unknown request reached the clipboard")
	}
}

// TestMalformedFrameDoesNotFallBackToTheClipboard is the reason the magic
// prefix commits the connection. A frame that fails to parse must be an error,
// never a stream of clipboard content — otherwise a peer could put arbitrary
// text on the clipboard by sending a frame it knew would fail.
func TestMalformedFrameDoesNotFallBackToTheClipboard(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	reply := h.send([]byte(protocol.Magic + "this is not json\n"))

	if h.clip.WriteCount() != 0 {
		t.Error("a malformed frame reached the clipboard")
	}
	if reply == "" {
		t.Error("a malformed frame got no reply")
	}
}

// TestInvalidUTF8FrameIsRejected exercises the protocol check through a real
// daemon connection. The JSON decoder must not repair a malformed named drop
// and publish it under a replacement-character filename.
func TestInvalidUTF8FrameIsRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	request := append([]byte(protocol.Magic+`{"type":"drop","name":"report`), 0xff)
	request = append(request, []byte(".txt\"}\ncontents")...)
	reply := h.send(request)

	if !strings.Contains(reply, "valid UTF-8") {
		t.Errorf("reply = %q, want an invalid UTF-8 explanation", reply)
	}
	if h.clip.WriteCount() != 0 {
		t.Error("an invalid structured frame reached the clipboard")
	}
	if entries, err := os.ReadDir(h.dropDir); err == nil {
		if len(entries) != 0 {
			t.Errorf("invalid structured frame published %d drop entries", len(entries))
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect drop directory: %v", err)
	}
}

func TestClipboardFailureIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.clip.Err = fmt.Errorf("pbcopy exploded")
	reply := h.send([]byte("data"))

	if !strings.Contains(reply, "clipboard write failed") {
		t.Errorf("reply = %q, want a clipboard failure message", reply)
	}
}

func TestConcurrentMessages(t *testing.T) {
	t.Parallel()

	const clients = 24
	h := newHarness(t)

	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.DialTimeout("unix", h.socket, 5*time.Second)
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
			if _, err := fmt.Fprintf(conn, "payload %d", i); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			_ = conn.(*net.UnixConn).CloseWrite()
			if _, err := io.ReadAll(conn); err != nil {
				t.Errorf("read: %v", err)
			}
		}()
	}
	wg.Wait()

	if h.clip.WriteCount() != clients {
		t.Errorf("clipboard writes = %d, want %d", h.clip.WriteCount(), clients)
	}
}

func TestShutdownWaitsForInFlightWork(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	conn, err := net.DialTimeout("unix", h.socket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("partial")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Give Serve time to accept and reach its first blocking read. A
	// successful dial only means the connection is in the kernel's backlog,
	// and Shutdown closes the listener — which discards anything still
	// queued there. Without this pause the test would sometimes be measuring
	// a connection that was never handled at all.
	time.Sleep(200 * time.Millisecond)

	// Shutdown while a handler is mid-message; it must not return until the
	// connection finishes rather than cutting it off.
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- h.srv.Shutdown(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	_ = conn.(*net.UnixConn).CloseWrite()
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("read reply: %v", err)
	}

	if err := <-shutdownDone; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if got := string(h.clip.Data()); got != "partial" {
		t.Errorf("clipboard = %q, want the in-flight payload", got)
	}
}

func TestServeAfterShutdownClosesTheListener(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	srv, err := New(Options{Clipboard: &clipboard.Fake{}, MaxPayload: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = srv.Shutdown(ctx)

	ln, err := Listen(filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := srv.Serve(context.Background(), ln); err == nil {
		t.Fatal("Serve on a shut-down server returned nil")
	}
	// The contract is that Serve closes the listener it was handed, even on
	// the refusal path, or the socket stays bound with nobody draining it.
	if _, err := ln.Accept(); err == nil {
		t.Error("the listener is still open after Serve refused it")
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	t.Parallel()

	tests := map[string]Options{
		"no clipboard":     {MaxPayload: 1024},
		"zero max payload": {Clipboard: &clipboard.Fake{}},
		"negative payload": {Clipboard: &clipboard.Fake{}, MaxPayload: -1},
	}
	for name, opts := range tests {
		if _, err := New(opts); err == nil {
			t.Errorf("New(%s) succeeded, want an error", name)
		}
	}
}

// TestNamedDropWritesOneFile is the pipeline case end to end: no archive, just
// bytes and a name, which is what `pg_dump db | clipd drop dump.sql` sends.
func TestNamedDropWritesOneFile(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	reply := h.send(namedDropRequest("dump.sql", []byte("rows and rows")))

	if !strings.Contains(reply, "dump.sql") {
		t.Fatalf("reply = %q, want it to name the file", reply)
	}
	got, err := os.ReadFile(filepath.Join(h.dropDir, "dump.sql"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "rows and rows" {
		t.Errorf("content = %q, want the streamed bytes", got)
	}
	if h.clip.WriteCount() != 0 {
		t.Error("a named drop reached the clipboard")
	}
}

// TestNamedDropCannotEscape: the name arrives over the wire from the same
// place an archive would, and is trusted exactly as little.
func TestNamedDropCannotEscape(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.send(namedDropRequest("../../escaped.txt", []byte("no")))

	parent := filepath.Dir(h.dropDir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "escaped.txt" {
			t.Fatal("a named drop wrote outside the drop directory")
		}
	}
	if _, err := os.Stat(filepath.Join(h.dropDir, "escaped.txt")); err != nil {
		t.Errorf("the file did not land in the drop directory: %v", err)
	}
}

// TestRejectionSurvivesAnInFlightPayload pins the drain.
//
// A payload small enough to fit in the socket buffers is fully sent before the
// daemon rejects it, so the reply is never at risk. This one is large enough
// that the sender is still writing when the rejection is issued — and closing
// a socket with data still arriving tears it down abruptly, taking the reply
// with it. In the field the same command reported the error six times in ten
// and said nothing the other four, the difference being how fast the source
// disk was.
func TestRejectionSurvivesAnInFlightPayload(t *testing.T) {
	t.Parallel()

	const limit = 64 << 10
	h := newHarness(t, withMaxPayload(limit))

	// Past the limit by less than drainCap. That is the range the drain can
	// actually cover: the daemon reads the remainder, closes cleanly, and the
	// reply survives. A sender megabytes past the limit is beyond what any
	// bounded drain can absorb, and may still lose the message — the drain
	// narrows this failure rather than eliminating it.
	reply := h.send(bytes.Repeat([]byte("x"), limit+(128<<10)))

	if reply == "" {
		t.Fatal("the rejection was lost")
	}
	if !strings.Contains(reply, "max_payload_bytes") {
		t.Errorf("reply = %q, want it to name the limit's setting", reply)
	}
	if h.clip.WriteCount() != 0 {
		t.Error("an oversized payload reached the clipboard")
	}
}

// TestDropRejectionSurvivesAnInFlightArchive is the same guarantee on the drop
// path, which has it for the same reason and lost it in the same way.
func TestDropRejectionSurvivesAnInFlightArchive(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withDropLimits(64<<10, 16))
	big := map[string]string{}
	for i := range 4 {
		big[string(rune('a'+i))+".bin"] = strings.Repeat("x", 48<<10)
	}

	reply := h.send(dropRequest(tarOf(t, big)))
	if reply == "" {
		t.Fatal("the drop rejection was lost")
	}
	if !strings.Contains(reply, "limit") {
		t.Errorf("reply = %q, want it to mention the limit", reply)
	}
}

func TestClipboardFailuresCannotFloodLogs(t *testing.T) {
	var logs bytes.Buffer
	h := newHarness(t, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	h.clip.Err = errors.New("helper unavailable")
	for i := 0; i < warnBudget+10; i++ {
		if reply := h.send([]byte("text")); !strings.HasPrefix(reply, protocol.StatusError) {
			t.Fatalf("reply %s", reply)
		}
	}
	if n := strings.Count(logs.String(), "clipboard write failed"); n != warnBudget {
		t.Fatalf("logged %d failures, want %d", n, warnBudget)
	}
}

func TestConfiguredLifetimeBoundsSilentConnections(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxTransfer = time.Second })
	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = io.ReadAll(conn)
	if err != nil {
		t.Fatalf("silent connection was not closed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("configured lifetime took %s", elapsed)
	}
}

func TestStreamingRequestProgressAndCompletion(t *testing.T) {
	h := newHarness(t)
	archive := tarOf(t, map[string]string{"book/file": "payload"})
	frame := []byte(protocol.Magic + `{"type":"drop-stream-v2","progress":true}` + "\n")
	reply := h.send(append(append(frame, archive...), []byte(drop.Completion)...))
	if !strings.Contains(reply, "clipd: progress: book/file:") || !strings.Contains(reply, protocol.StatusOK) {
		t.Fatalf("reply = %q", reply)
	}
	if h.clip.WriteCount() != 0 {
		t.Fatal("streaming request was reinterpreted as clipboard data")
	}
}

func TestLifetimeBoundsPeerThatDoesNotReadProgress(t *testing.T) {
	dir := t.TempDir()
	srv, err := New(Options{Clipboard: &clipboard.Fake{}, DropDir: dir, MaxPayload: 1024, MaxTransfer: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	receiver, sender := net.Pipe()
	defer sender.Close()
	done := make(chan struct{})
	go func() { srv.handle(context.Background(), receiver); close(done) }()
	payload := append([]byte(protocol.Magic+`{"type":"drop-stream-v2","progress":true}`+"\n"), tarOf(t, map[string]string{"file": "payload"})...)
	writer := make(chan struct{})
	go func() { _, _ = sender.Write(payload); close(writer) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("progress write extended lifetime")
	}
	<-writer
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("timed out request left entries: %v, %v", entries, err)
	}
}

// A producer such as sort or a build can be silent for longer than any idle
// window before its first byte, and again between bytes. Neither is a failure.
func TestSilentProducerIsNotCutOff(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the former 30-second idle cutoff")
	}
	t.Parallel()
	h := newHarness(t)
	conn, err := net.DialTimeout("unix", h.socket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	time.Sleep(31 * time.Second)
	if _, err := conn.Write([]byte("sorted output")); err != nil {
		t.Fatalf("write after silence: %v", err)
	}
	_ = conn.(*net.UnixConn).CloseWrite()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if !strings.HasPrefix(string(reply), protocol.StatusOK) {
		t.Fatalf("silent producer was cut off: %q", reply)
	}
	if got := string(h.clip.Data()); got != "sorted output" {
		t.Fatalf("clipboard = %q", got)
	}
}

func withMaxTransfer(d time.Duration) harnessOption { return func(o *Options) { o.MaxTransfer = d } }

// A transfer that runs out of time is told why, inside the same lifetime:
// reading stops replyReserve early rather than the reply extending it.
func TestLifetimeExceededIsExplainedWithinTheLifetime(t *testing.T) {
	t.Parallel()
	const lifetime = 2 * time.Second
	h := newHarness(t, withMaxTransfer(lifetime))
	for name, first := range map[string][]byte{
		"clipboard": []byte("partial clipboard"),
		"drop":      []byte("clipd:magic:v1\n{\"type\":\"drop-stream-v2\"}\n"),
		"silent":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.DialTimeout("unix", h.socket, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			start := time.Now()
			if _, err := conn.Write(first); err != nil {
				t.Fatal(err)
			}
			// Never half-close: the sender is still "producing".
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			reply, _ := io.ReadAll(conn)
			if elapsed := time.Since(start); elapsed > lifetime+time.Second {
				t.Fatalf("connection outlived its lifetime: %v", elapsed)
			}
			if !strings.Contains(string(reply), "took longer than the 2s limit") || !strings.Contains(string(reply), "max_transfer_seconds") {
				t.Fatalf("reply = %q", reply)
			}
		})
	}
}

func TestLifetimeExceededFormatsTheLimit(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "the 10s limit",
		90 * time.Second: "the 1m30s limit",
		30 * time.Minute: "the 30m limit",
		time.Hour:        "the 1h limit",
		90 * time.Minute: "the 1h30m limit",
	} {
		if got := (&Server{maxTransfer: d}).lifetimeExceeded(); !strings.Contains(got, want) {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
