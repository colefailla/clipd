package main

import (
	"context"
	"fmt"
	"runtime"

	"github.com/colefailla/clipd/internal/auth"
	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/launchagent"
	"github.com/colefailla/clipd/internal/transport"
)

// cmdInstall sets up the macOS LaunchAgent, generating the config and token
// first if they do not exist yet.
//
// Nothing here needs root: the plist lives in the user's home directory and
// is bootstrapped into their own gui/<uid> domain, which is also what gives
// the daemon access to their pasteboard.
func cmdInstall(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "install", "Usage: clipd install [options]")
	execPath := flags.String("exec", "", "binary path to record in the plist (default: this binary)")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	if runtime.GOOS != "darwin" {
		return fail(e, exitFailure, launchagent.ErrUnsupported)
	}

	cfg, path, err := loadConfig(e, g)
	if err != nil {
		return fail(e, exitConfig, err)
	}

	generated := false
	if cfg.Token == "" {
		token, err := auth.GenerateToken()
		if err != nil {
			return fail(e, exitFailure, err)
		}
		cfg.Token = token
		generated = true
	}
	if err := cfg.ValidateServer(); err != nil {
		return fail(e, exitConfig, err)
	}
	if err := cfg.Save(path); err != nil {
		return fail(e, exitConfig, err)
	}

	// Generated here as well as in setup, so `clipd install` on a fresh
	// machine is genuinely the only command needed.
	certPath, keyPath := cfg.CertPath(path), cfg.KeyPath(path)
	if _, err := transport.EnsureCert(certPath, keyPath, transport.DefaultValidity); err != nil {
		return fail(e, exitTLS, err)
	}
	cert, err := transport.LoadCertificate(certPath)
	if err != nil {
		return fail(e, exitTLS, err)
	}
	fingerprint := transport.FormatFingerprint(transport.Fingerprint(cert))

	// The agent inherits no shell environment, so a non-default config path
	// has to be pinned into the plist or the daemon would read the default.
	opts := launchagent.Options{ExecutablePath: *execPath}
	if g.configPath != "" || e.getenv("CLIPD_CONFIG") != "" {
		opts.ConfigPath = path
	}

	res, err := launchagent.Install(ctx, opts)
	if err != nil {
		return fail(e, exitFailure, err)
	}

	out := e.stdout
	fmt.Fprintf(out, "Installed the clipd LaunchAgent.\n\n")
	fmt.Fprintf(out, "  binary       %s\n", res.ExecutablePath)
	fmt.Fprintf(out, "  plist        %s\n", res.PlistPath)
	fmt.Fprintf(out, "  log          %s\n", res.LogPath)
	fmt.Fprintf(out, "  listen       %s\n", cfg.ListenAddress())
	fmt.Fprintf(out, "  config       %s\n", path)
	fmt.Fprintf(out, "  certificate  %s\n", certPath)

	if generated {
		fmt.Fprintf(out, "\nAuthentication token (treat it like a password):\n\n  %s\n", cfg.Token)
	} else {
		fmt.Fprintf(out, "  token        %s\n", auth.Redact(cfg.Token))
		fmt.Fprintf(out, "\nRun 'clipd setup' to display the token again.\n")
	}
	fmt.Fprintf(out, "\nServer fingerprint (not secret — this is what clients verify):\n\n  %s\n", fingerprint)

	fmt.Fprintf(out, "\nOn the client machine:\n\n")
	fmt.Fprintf(out, "  clipd configure -server %s -port %d \\\n", suggestedServerAddress(), cfg.Port)
	fmt.Fprintf(out, "    -fingerprint %s \\\n", fingerprint)
	fmt.Fprintf(out, "    -token -\n\n")
	fmt.Fprintf(out, "Paste the token when it asks, rather than passing -token <value>:\n")
	fmt.Fprintf(out, "other local users can read a command line from ps, and the shell\n")
	fmt.Fprintf(out, "records it in history.\n\n")
	fmt.Fprintf(out, "The daemon starts at login and restarts if it exits. Check it with\n")
	fmt.Fprintf(out, "'clipd status'.\n")
	return exitOK
}

// cmdUninstall unloads and removes the LaunchAgent.
//
// The config file is left alone: uninstalling the daemon should not silently
// discard a token the user may still want, and removing it is one rm away.
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

	// The config is untouched, so say where it is: the token is still on disk
	// and the user may or may not want it there.
	if path, err := config.ResolvePath(g.configPath); err == nil && config.Exists(path) {
		fmt.Fprintf(e.stdout, "The config file still holds the token: %s\n", path)
	}
	return exitOK
}
