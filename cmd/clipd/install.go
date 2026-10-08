package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/launchagent"
	"github.com/colefailla/clipd/internal/server"
)

// cmdInstall sets up the macOS LaunchAgent.
//
// Nothing here needs root: the plist lives in the user's home directory and is
// bootstrapped into their own gui/<uid> domain, which is also what gives the
// daemon access to their pasteboard.
func cmdInstall(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "install", "Usage: clipd install [options]")
	execPath := flags.String("exec", "", "binary path to record in the plist (default: this binary)")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	if runtime.GOOS != "darwin" {
		return fail(e, exitFailure, launchagent.ErrUnsupported)
	}

	cfg, path, err := loadConfig(g)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	// Written out even when every value is a default, so that the file exists
	// to edit and status has something to point at.
	if err := cfg.Save(path); err != nil {
		return fail(e, exitConfig, err)
	}

	// The agent inherits no shell environment, so the config path is pinned
	// into the plist rather than left to be resolved again at startup.
	//
	// Pinned unconditionally, not only for an explicit -config. XDG_CONFIG_HOME
	// selects the default path too, and launchd does not pass it on: the daemon
	// read ~/.config/clipd/config.json while this command reported — and wrote
	// — the file the variable pointed at.
	//
	// Absolute, because launchd starts agents with / as the working directory,
	// so a relative path that worked for this command would leave the daemon
	// reading a file that does not exist and crash-looping under KeepAlive.
	opts := launchagent.Options{ExecutablePath: *execPath}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	opts.ConfigPath = path

	res, err := launchagent.Install(ctx, opts)
	if err != nil {
		return fail(e, exitFailure, err)
	}

	socket, err := server.ExpandPath(cfg.Address)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	dropDir, err := server.ExpandPath(cfg.DropDir)
	if err != nil {
		return fail(e, exitConfig, err)
	}

	out := e.stdout
	fmt.Fprintf(out, "Installed the clipd LaunchAgent.\n\n")
	fmt.Fprintf(out, "  binary       %s\n", res.ExecutablePath)
	fmt.Fprintf(out, "  plist        %s\n", res.PlistPath)
	fmt.Fprintf(out, "  log          %s\n", res.LogPath)
	fmt.Fprintf(out, "  config       %s\n", path)
	fmt.Fprintf(out, "  socket       %s\n", socket)
	fmt.Fprintf(out, "  drop dir     %s\n", dropDir)

	fmt.Fprintf(out, "\nclipd listens on a private socket. Remote hosts reach it through SSH.\n")
	fmt.Fprintf(out, "\nTo configure a host, run:\n\n  clipd setup <host>\n")
	return exitOK
}

// cmdUninstall unloads and removes the LaunchAgent.
func cmdUninstall(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "uninstall", "Usage: clipd uninstall")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	if runtime.GOOS != "darwin" {
		return fail(e, exitFailure, launchagent.ErrUnsupported)
	}

	plistPath, err := launchagent.Uninstall(ctx)
	if err != nil {
		return fail(e, exitFailure, err)
	}
	fmt.Fprintf(e.stdout, "Removed the clipd LaunchAgent (%s).\n", plistPath)

	if path, err := config.ResolvePath(g.configPath); err == nil && config.Exists(path) {
		fmt.Fprintf(e.stdout, "The config file remains: %s\n", path)
	}
	if logPath, err := launchagent.LogPath(); err == nil {
		if dir := filepath.Dir(logPath); dirExists(dir) {
			fmt.Fprintf(e.stdout, "Log files remain in %s\n", dir)
		}
	}
	fmt.Fprintf(e.stdout, "\nThe 'clipd' shell functions on remote hosts are left in place;\n")
	fmt.Fprintf(e.stdout, "delete the block between the clipd markers in their rc files.\n")
	return exitOK
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
