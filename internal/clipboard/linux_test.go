//go:build linux

package clipboard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHelpers puts executables with the given names on a PATH of their own and
// points PATH at it, so selection is decided by the test rather than by what
// the machine running it happens to have installed.
//
// Each one appends its stdin to a file named after itself, which is what lets a
// test check both that the right tool was chosen and that the bytes reached it
// unaltered.
func fakeHelpers(t *testing.T, names ...string) (dir string) {
	t.Helper()
	// Resolved before PATH is narrowed to dir, which hides cat from the fakes.
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatalf("find cat: %v", err)
	}
	dir = t.TempDir()
	for _, name := range names {
		script := "#!/bin/sh\n" + cat + " >> " + filepath.Join(dir, name+".out") + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

// TestNewRefusesWithoutADisplay is the headless case: a server has no clipboard
// for any backend to write to, and saying so at startup beats failing on the
// first copy with something about a missing package.
func TestNewRefusesWithoutADisplay(t *testing.T) {
	fakeHelpers(t, "wl-copy", "xclip", "xsel")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")

	if _, err := New(); !errors.Is(err, ErrNoDisplay) {
		t.Fatalf("New() = %v, want ErrNoDisplay", err)
	}
}

// TestNewPrefersWaylandOverX11 pins the ordering that matters.
//
// A Wayland session normally also sets DISPLAY so XWayland works, so a check
// that tested DISPLAY first would pick an X11 tool on a Wayland desktop — where
// it reaches only XWayland clients and silently misses everything native.
func TestNewPrefersWaylandOverX11(t *testing.T) {
	fakeHelpers(t, "wl-copy", "xclip", "xsel")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("DISPLAY", ":0")

	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(c.Name(), "wl-copy") {
		t.Errorf("chose %q on a Wayland session, want wl-copy", c.Name())
	}
}

func TestNewFallsBackToX11(t *testing.T) {
	fakeHelpers(t, "xclip", "xsel")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":0")

	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(c.Name(), "xclip") {
		t.Errorf("chose %q, want xclip", c.Name())
	}
	// The clipboard proper, not the primary selection: every one of these tools
	// defaults to the middle-click buffer, which is not what anyone means.
	if !strings.Contains(c.Name(), "-selection clipboard") {
		t.Errorf("xclip was chosen without selecting the clipboard: %q", c.Name())
	}
}

// TestNewNamesTheMissingPackage: an error that says what to install is the
// difference between a two-minute fix and a bug report.
func TestNewNamesTheMissingPackage(t *testing.T) {
	fakeHelpers(t) // a PATH with no clipboard tools on it at all
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":0")

	_, err := New()
	if err == nil {
		t.Fatal("New succeeded with no helper installed")
	}
	for _, want := range []string{"xclip", "xsel"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "wl-clipboard") {
		t.Errorf("err = %v, want it to skip the Wayland package on an X11 session", err)
	}
}

// TestWritePassesBytesThrough covers the exec path, including the WaitDelay
// that exists because X11 helpers fork a child to hold the selection.
func TestWritePassesBytesThrough(t *testing.T) {
	dir := fakeHelpers(t, "xclip")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":0")

	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Deliberately awkward: a trailing newline and an embedded NUL are exactly
	// the things a clipboard helper must not tidy up.
	payload := []byte("line one\nline two\n\x00tail")
	if err := c.Write(context.Background(), payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "xclip.out"))
	if err != nil {
		t.Fatalf("read what the helper received: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("helper received %q, want %q", got, payload)
	}
}
