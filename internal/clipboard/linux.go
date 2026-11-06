//go:build linux

package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// helper describes a clipboard writer and the display session it requires.
type helper struct {
	// name is the executable to look for on PATH.
	name string

	// args select the clipboard rather than the primary selection where needed.
	args []string

	// display is the environment variable that has to be set for this tool to
	// have a session to talk to.
	display string

	// pkg is what to install, named in the error when nothing is found.
	pkg string
}

// helpers are tried in order.
//
// wl-copy comes first because a Wayland session usually also sets DISPLAY for
// XWayland's benefit. Testing DISPLAY first would therefore pick an X11 tool on
// a Wayland desktop, where it can only talk to XWayland clients and would miss
// the clipboard of everything native.
var helpers = []helper{
	{name: "wl-copy", display: "WAYLAND_DISPLAY", pkg: "wl-clipboard"},
	{name: "xclip", args: []string{"-selection", "clipboard"}, display: "DISPLAY", pkg: "xclip"},
	{name: "xsel", args: []string{"--clipboard", "--input"}, display: "DISPLAY", pkg: "xsel"},
}

// ErrNoDisplay reports that the process has no graphical session, so there is
// no clipboard for any backend to write to.
var ErrNoDisplay = errors.New("clipboard: no graphical session (neither WAYLAND_DISPLAY nor DISPLAY is set)")

// New picks a clipboard backend from what this session actually has.
func New() (Clipboard, error) {
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") == "" {
		// Worth its own error: the most likely reason to hit it is running the
		// daemon on a headless server, where no amount of installing will help.
		// The second most likely is a user service that did not inherit the
		// graphical session's environment, which is a fixable configuration
		// problem and a different conversation from a missing package.
		return nil, ErrNoDisplay
	}

	var wanted []string
	for _, h := range helpers {
		if os.Getenv(h.display) == "" {
			continue
		}
		wanted = append(wanted, h.pkg)
		path, err := exec.LookPath(h.name)
		if err != nil {
			continue
		}
		return &helperClipboard{path: path, args: h.args}, nil
	}
	return nil, fmt.Errorf(
		"clipboard: this session needs one of these and has none installed: %s",
		strings.Join(wanted, ", "))
}

// helperClipboard writes to the clipboard by running an external tool.
type helperClipboard struct {
	path string
	args []string
}

func (h *helperClipboard) Name() string {
	if len(h.args) == 0 {
		return h.path
	}
	return h.path + " " + strings.Join(h.args, " ")
}

// waitDelay bounds waiting for pipes inherited by a clipboard owner's child.
// X11 and Wayland helpers may fork to keep serving the clipboard selection.
const waitDelay = 200 * time.Millisecond

func (h *helperClipboard) Write(ctx context.Context, data []byte) error {
	cmd := exec.CommandContext(ctx, h.path, h.args...)
	cmd.Stdin = bytes.NewReader(data)
	cmd.WaitDelay = waitDelay

	// Captured for the message on failure; stdout is discarded. Neither ever
	// contains clipboard content.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("clipboard: %s failed: %w: %s", h.path, err, msg)
		}
		return fmt.Errorf("clipboard: %s failed: %w", h.path, err)
	}
	return nil
}
