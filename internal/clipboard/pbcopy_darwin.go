//go:build darwin

package clipboard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// pbcopyPath is macOS's built-in clipboard writer. Shelling out to it avoids
// cgo and an NSPasteboard binding for v1; the process cost is irrelevant for
// an operation a human triggers by hand. Moving to NSPasteboard later is a
// drop-in replacement behind the Clipboard interface.
const pbcopyPath = "/usr/bin/pbcopy"

// New returns the macOS clipboard backend.
func New() (Clipboard, error) {
	if _, err := os.Stat(pbcopyPath); err != nil {
		return nil, fmt.Errorf("clipboard: %s is unavailable: %w", pbcopyPath, err)
	}
	return &pbcopy{path: pbcopyPath}, nil
}

// pbcopy writes to the macOS pasteboard via /usr/bin/pbcopy.
//
// The binary path is a field rather than a constant reference so tests can
// point it at a stand-in and verify the exact bytes handed over without
// clobbering the developer's real clipboard.
type pbcopy struct {
	path string
}

func (p *pbcopy) Name() string { return p.path }

// waitDelay bounds how long Run may linger after the context is cancelled.
//
// Cancelling the context kills the helper, but Run also waits for the stdin
// and stderr pipes to close, and a killed process can leave a child holding
// them open. Without this bound, Run blocks until that child exits on its own,
// which defeats the deadline the caller set. Real pbcopy spawns nothing, so
// this never fires in practice — it exists so that a hung or unusual helper
// cannot outlive its context.
const waitDelay = 2 * time.Second

func (p *pbcopy) Write(ctx context.Context, data []byte) error {
	cmd := exec.CommandContext(ctx, p.path)
	cmd.Stdin = bytes.NewReader(data)
	cmd.WaitDelay = waitDelay

	// pbcopy is silent on success. Capturing stderr gives a useful message on
	// failure; stdout is discarded. Neither ever contains clipboard content.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("clipboard: %s failed: %w: %s", p.path, err, msg)
		}
		// A pasteboard error here usually means the process has no access to
		// the user's GUI session, which is exactly what happens when clipd is
		// run as a LaunchDaemon instead of a per-user LaunchAgent.
		return fmt.Errorf("clipboard: %s failed: %w", p.path, err)
	}
	return nil
}
