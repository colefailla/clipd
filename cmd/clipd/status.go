package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/launchagent"
	"github.com/colefailla/clipd/internal/server"
)

var agentStatus = launchagent.Status

func cmdStatus(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "status", "Usage: clipd status")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	cfg, path, err := loadConfig(g)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	socket, err := server.ExpandPath(cfg.Address)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	drops, err := server.ExpandPath(cfg.DropDir)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	out := e.stdout
	fmt.Fprintf(out, "clipd %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	code := exitOK
	if err := server.PingContext(ctx, "unix", socket); err != nil {
		fmt.Fprintf(out, "Receiver did not respond as clipd: %v\n", err)
		code = exitFailure
	} else {
		fmt.Fprintln(out, "Receiver is responding.")
	}
	var state launchagent.State
	var stateErr error
	var spec launchagent.Spec
	var specErr error
	serviceMatches := false
	if runtime.GOOS == "darwin" {
		state, stateErr = agentStatus(ctx)
		if stateErr == nil && state.PlistInstalled {
			spec, specErr = readInstalledAgent(ctx)
			if specErr == nil {
				serviceAddress, err := serviceSocket(spec)
				serviceMatches = err == nil && serviceAddress == socket
				if pinned := spec.EnvironmentVariables["CLIPD_CONFIG"]; pinned != "" {
					absolute, _ := filepath.Abs(path)
					serviceMatches = serviceMatches && pinned == absolute
				}
			}
		}
		if serviceMatches && state.Loaded && state.PID > 0 {
			if info, err := os.Stat(path); err == nil {
				if sock, err := os.Lstat(socket); err == nil && info.ModTime().After(sock.ModTime()) {
					fmt.Fprintln(out, "Config changed since startup. Run 'clipd restart' to apply it.")
				}
			}
		}
	}
	fmt.Fprintf(out, "\nListener:\n  socket       %s\n", socket)
	if info, err := os.Lstat(socket); err == nil {
		fmt.Fprintf(out, "  mode         %04o\n", info.Mode().Perm())
	} else if os.IsNotExist(err) {
		fmt.Fprintln(out, "  mode         unavailable (no socket)")
	} else {
		fmt.Fprintf(out, "  mode         unavailable (%v)\n", err)
	}
	if runtime.GOOS == "darwin" {
		fmt.Fprintln(out, "\nService:")
		switch {
		case stateErr != nil:
			fmt.Fprintf(out, "  state        unknown (%v)\n", stateErr)
		case !state.PlistInstalled:
			fmt.Fprintln(out, "  state        not installed (run 'clipd install')")
		case state.Loaded && state.PID > 0:
			fmt.Fprintf(out, "  state        running (pid %d)\n", state.PID)
		case state.Loaded:
			fmt.Fprintln(out, "  state        loaded but not running")
		default:
			fmt.Fprintln(out, "  state        not loaded (run 'clipd install')")
		}
		if state.PlistInstalled {
			if specErr != nil {
				fmt.Fprintf(out, "  binary       unknown (%v)\n", specErr)
			} else {
				fmt.Fprintf(out, "  binary       %s\n", spec.ProgramArguments[0])
				if pinned := spec.EnvironmentVariables["CLIPD_CONFIG"]; pinned != "" && pinned != path {
					fmt.Fprintf(out, "  config       %s (service configuration differs from status)\n", pinned)
				}
			}
		}
		if state.PlistPath != "" {
			fmt.Fprintf(out, "  plist        %s\n", state.PlistPath)
		}
		if state.LogPath != "" {
			fmt.Fprintf(out, "  log          %s\n", state.LogPath)
		}
		if state.LastExitStatus != 0 {
			fmt.Fprintf(out, "  last exit    %d (see log)\n", state.LastExitStatus)
		}
		if code != exitOK && state.LogPath != "" {
			fmt.Fprintf(out, "\nCheck the log: %s\n", state.LogPath)
		}
	}
	fmt.Fprintf(out, "\nConfiguration:\n  file         %s", path)
	if !config.Exists(path) {
		fmt.Fprint(out, " (not created; using defaults)")
	}
	fmt.Fprintf(out, "\n  drops        %s%s\n", drops, existsNote(drops))
	if old := config.LegacyPath(); old != "" {
		fmt.Fprintf(out, "  note         an unused config from an older clipd release remains at %s\n", old)
	}
	fmt.Fprintf(out, "\nConfigured limits:\n  clipboard    %s\n  drop         %s, %d files\n  concurrent   %d\n  transfer     %s\n\nLimits reflect the config file, not the running daemon.\n",
		config.FormatSize(cfg.MaxPayloadBytes), config.FormatSize(cfg.MaxDropBytes), cfg.MaxDropFiles, cfg.MaxConcurrent, config.FormatDuration(time.Duration(cfg.MaxTransferSeconds)*time.Second))
	return code
}

func existsNote(dir string) string {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return ""
	}
	return " (created on first drop)"
}
