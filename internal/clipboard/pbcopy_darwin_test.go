//go:build darwin

package clipboard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests here never invoke the real /usr/bin/pbcopy: clobbering the
// developer's clipboard as a side effect of `go test` would be rude, and it
// would make the assertions racy against anything else using the pasteboard.
// The backend takes its binary path as a field precisely so a stand-in can be
// substituted here.

func TestPbcopyWritesPayloadToStdin(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	output := filepath.Join(dir, "captured")
	clip := &pbcopy{path: writeScript(t, dir, fmt.Sprintf("#!/bin/sh\ncat > %q\n", output))}

	// Bytes chosen to break anything that assumes text: a NUL, invalid
	// UTF-8, CRLF, an escape sequence and no trailing newline.
	payload := []byte("first\r\nsecond\t\x1b[0m\x00\xff\xfelast")
	if err := clip.Write(context.Background(), payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read captured output: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("pbcopy received %q, want %q", got, payload)
	}
}

func TestPbcopyPreservesTrailingNewline(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	output := filepath.Join(dir, "captured")
	clip := &pbcopy{path: writeScript(t, dir, fmt.Sprintf("#!/bin/sh\ncat > %q\n", output))}

	payload := []byte("total 0\ndrwxr-xr-x  2 user  staff  64 Jan  1 00:00 .\n")
	if err := clip.Write(context.Background(), payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read captured output: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("trailing newline was not preserved: got %q", got)
	}
}

func TestPbcopyReportsFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	clip := &pbcopy{path: writeScript(t, dir, "#!/bin/sh\necho 'pasteboard unavailable' >&2\nexit 1\n")}

	err := clip.Write(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("Write succeeded despite a failing helper")
	}
	// The helper's stderr is the only clue to why a copy failed, so it has to
	// survive into the error.
	if !strings.Contains(err.Error(), "pasteboard unavailable") {
		t.Errorf("error = %v, want it to include the helper's stderr", err)
	}
}

// TestPbcopyRespectsContext covers the worst case: a helper that outlives the
// kill signal and leaves a child holding the pipes open.
//
// The trailing echo matters. Without it a shell will often exec into the last
// command, replacing itself, so killing the process kills the sleep too and
// the hard path never runs. With it, the shell forks, and the orphaned sleep
// keeps the stderr pipe open after the shell dies — which is exactly what made
// this test pass locally and hang for 30 seconds on CI. cmd.WaitDelay is what
// bounds it now.
func TestPbcopyRespectsContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	clip := &pbcopy{path: writeScript(t, dir, "#!/bin/sh\nsleep 30\necho unreachable\n")}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := clip.Write(ctx, []byte("x")); err == nil {
		t.Fatal("Write succeeded despite an expired context")
	}
	elapsed := time.Since(start)
	if elapsed > waitDelay+3*time.Second {
		t.Errorf("Write took %s; the context did not bound the helper", elapsed)
	}
	// It should not return before the delay it promised, either: returning
	// early would mean the pipes were abandoned rather than bounded.
	if elapsed < 100*time.Millisecond {
		t.Errorf("Write returned in %s, before the context even expired", elapsed)
	}
}

func TestNewReturnsPbcopy(t *testing.T) {
	t.Parallel()

	clip, err := New()
	if err != nil {
		t.Fatalf("New on macOS: %v", err)
	}
	if clip.Name() != pbcopyPath {
		t.Errorf("Name = %q, want %q", clip.Name(), pbcopyPath)
	}
}

// writeScript creates an executable stand-in for pbcopy.
func writeScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-pbcopy")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write stand-in: %v", err)
	}
	return path
}
