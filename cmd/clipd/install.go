package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/launchagent"
	"github.com/colefailla/clipd/internal/server"
)

// installAgent is replaceable in command tests to avoid managing a live service.
var installAgent = launchagent.Install

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
	// launchd's working directory differs from the invoking shell's.
	for _, setting := range []struct{ name, path string }{
		{"address", cfg.Address}, {"drop_dir", cfg.DropDir},
	} {
		if !filepath.IsAbs(setting.path) && !strings.HasPrefix(setting.path, "~/") {
			return failf(e, exitConfig, "%s: installation requires an absolute path or a path beginning with ~/", setting.name)
		}
	}
	socket, err := server.ExpandPath(cfg.Address)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	dropDir, err := server.ExpandPath(cfg.DropDir)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	// Written out even when every value is a default, so that the file exists
	// to edit and status has something to point at.
	if err := cfg.Save(path); err != nil {
		return fail(e, exitConfig, err)
	}

	// Pin the config path because launchd does not inherit shell variables
	// such as XDG_CONFIG_HOME and uses a different working directory.
	opts := launchagent.Options{ExecutablePath: *execPath}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	opts.ConfigPath = path

	res, err := installAgent(ctx, opts)
	if err != nil {
		return fail(e, exitFailure, err)
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
