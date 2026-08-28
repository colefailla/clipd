package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/launchagent"
	"github.com/colefailla/clipd/internal/server"
)

// cmdStatus reports the daemon's configuration and whether it is actually
// listening.
//
// The exit code carries the verdict, so `clipd status && ...` is a real
// preflight: 0 when the daemon answers, 1 when it does not.
func cmdStatus(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "status", "Usage: clipd status")
	if code, ok := flags.parse(args); !ok {
		return code
	}

	cfg, path, err := loadConfig(g)
	if err != nil {
		return fail(e, exitConfig, err)
	}

	out := e.stdout
	fmt.Fprintf(out, "clipd %s (%s/%s)\n\n", version, runtime.GOOS, runtime.GOARCH)

	fmt.Fprintf(out, "configuration\n")
	switch {
	case !config.Exists(path):
		fmt.Fprintf(out, "  file         %s (not created; using defaults)\n", path)
	case staleConfig(ctx, path):
		// The values printed below come from the file, but the daemon read it
		// once at startup. Without this line status reports an edit as though
		// it had taken effect, which is worse than not reporting it at all.
		fmt.Fprintf(out, "  file         %s\n", path)
		fmt.Fprintf(out, "               EDITED since the daemon started — run 'clipd restart'\n")
	default:
		fmt.Fprintf(out, "  file         %s\n", path)
	}
	// Before v3 this lived under Application Support. Nothing reads it now, and
	// an upgrader looking for their settings should be told that rather than
	// left editing a file with no effect.
	if old := config.LegacyPath(); old != "" {
		fmt.Fprintf(out, "  note         an unused pre-v3 config remains at %s\n", old)
	}
	fmt.Fprintf(out, "  max payload  %s\n", config.FormatSize(cfg.MaxPayloadBytes))
	fmt.Fprintf(out, "  max drop     %s across %d files\n",
		config.FormatSize(cfg.MaxDropBytes), cfg.MaxDropFiles)

	dropDir, err := server.ExpandPath(cfg.DropDir)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	fmt.Fprintf(out, "  drop dir     %s%s\n", dropDir, existsNote(dropDir))

	fmt.Fprintf(out, "\nlistener\n")
	code := reportListener(out, cfg.Address)

	if runtime.GOOS == "darwin" {
		fmt.Fprintf(out, "\nLaunchAgent\n")
		reportAgent(ctx, out)
	}
	return code
}

// staleConfig reports whether the config has been modified since the daemon
// last started.
//
// The daemon's start time is taken from the socket, which it creates on the way
// up and removes on the way down. That makes the comparison exact for the
// question being asked — has the file changed since this daemon read it —
// without having to ask launchd anything or have the daemon report its own
// uptime.
func staleConfig(ctx context.Context, path string) bool {
	cfg, err := os.Stat(path)
	if err != nil {
		return false
	}
	state, err := launchagent.Status(ctx)
	if err != nil || !state.Loaded || state.PID == 0 {
		// Nothing running, so nothing to be out of date with.
		return false
	}
	started, err := daemonStart()
	if err != nil {
		return false
	}
	return cfg.ModTime().After(started)
}

// daemonStart returns when the listening socket was created, which is when the
// running daemon came up.
func daemonStart() (time.Time, error) {
	cfg, _, err := loadConfig(&globalOptions{})
	if err != nil {
		return time.Time{}, err
	}
	if !server.IsSocketPath(cfg.Address) {
		return time.Time{}, errors.New("not a socket")
	}
	sock, err := server.ExpandPath(cfg.Address)
	if err != nil {
		return time.Time{}, err
	}
	info, err := os.Lstat(sock)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// reportListener says whether something is actually accepting on the
// configured address, and returns the exit code that answer implies.
func reportListener(out io.Writer, address string) int {
	if !server.IsSocketPath(address) {
		fmt.Fprintf(out, "  address      %s (TCP — no authentication; loopback only)\n", address)
		conn, err := net.Dial("tcp", address)
		if err != nil {
			fmt.Fprintf(out, "  daemon       not answering (%v)\n", err)
			return exitFailure
		}
		conn.Close()
		fmt.Fprintf(out, "  daemon       answering\n")
		return exitOK
	}

	path, err := server.ExpandPath(address)
	if err != nil {
		fmt.Fprintf(out, "  address      %s (%v)\n", address, err)
		return exitConfig
	}
	fmt.Fprintf(out, "  socket       %s\n", path)

	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		fmt.Fprintf(out, "  daemon       not running (no socket)\n")
		return exitFailure
	}
	if err != nil {
		fmt.Fprintf(out, "  daemon       unknown (%v)\n", err)
		return exitFailure
	}
	fmt.Fprintf(out, "  mode         %04o\n", info.Mode().Perm())

	// Dialling is the only way to tell a live socket from one a killed daemon
	// left behind, and the difference matters: a stale file looks identical in
	// a directory listing but accepts nothing.
	conn, err := net.Dial("unix", path)
	if err != nil {
		fmt.Fprintf(out, "  daemon       not running (stale socket: %v)\n", err)
		return exitFailure
	}
	conn.Close()
	fmt.Fprintf(out, "  daemon       listening\n")
	return exitOK
}

// existsNote annotates a directory that has not been created yet, which is
// normal before the first drop.
func existsNote(dir string) string {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return ""
	}
	return " (created on first drop)"
}

// reportAgent prints the LaunchAgent's state.
func reportAgent(ctx context.Context, out io.Writer) {
	state, err := launchagent.Status(ctx)
	if err != nil {
		fmt.Fprintf(out, "  agent        unknown (%v)\n", err)
		return
	}
	if !state.PlistInstalled {
		fmt.Fprintf(out, "  agent        not installed (run 'clipd install')\n")
		return
	}
	fmt.Fprintf(out, "  plist        %s\n", state.PlistPath)
	switch {
	case state.Loaded && state.PID > 0:
		fmt.Fprintf(out, "  launchd      running (pid %d)\n", state.PID)
	case state.Loaded:
		fmt.Fprintf(out, "  launchd      loaded but not running\n")
	default:
		fmt.Fprintf(out, "  launchd      not loaded (run 'clipd install')\n")
	}
	// A non-zero previous exit is the one thing worth surfacing unprompted:
	// KeepAlive restarts the daemon after a crash, so without this the failure
	// leaves no trace anywhere the user would think to look.
	if state.LastExitStatus != 0 {
		fmt.Fprintf(out, "  last exit    %d (see %s)\n", state.LastExitStatus, state.LogPath)
	}
	if state.LogPath != "" {
		fmt.Fprintf(out, "  log          %s\n", state.LogPath)
	}
}
