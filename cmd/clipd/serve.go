package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/server"
)

// cmdServe runs the clipboard daemon in the foreground.
//
// It never detaches. Under the LaunchAgent, launchd is responsible for
// backgrounding, log redirection and restarts; a process that also tried to
// daemonize itself would just be a second opinion for launchd to disagree
// with.
func cmdServe(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "serve", "Usage: clipd serve [options]")
	address := flags.String("address", "", "socket path or host:port to listen on (default from config)")
	dropDir := flags.String("drop-dir", "", "directory for dropped files (default from config)")
	if code, ok := flags.parse(args); !ok {
		return code
	}

	cfg, cfgPath, err := loadConfig(g)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	if *address != "" {
		cfg.Address = *address
	}
	if *dropDir != "" {
		cfg.DropDir = *dropDir
	}
	if err := cfg.Validate(); err != nil {
		return fail(e, exitConfig, err)
	}

	clip, err := clipboard.New()
	if err != nil {
		if errors.Is(err, clipboard.ErrUnsupported) {
			return failf(e, exitFailure,
				"the clipd daemon runs on macOS only; run it on the Mac whose clipboard you want to write")
		}
		return fail(e, exitFailure, err)
	}

	// Resolved once here rather than inside the server, so the path that gets
	// logged is the path files will actually appear at.
	resolvedDrop, err := server.ExpandPath(cfg.DropDir)
	if err != nil {
		return fail(e, exitConfig, err)
	}

	level := slog.LevelInfo
	if g.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: level}))

	srv, err := server.New(server.Options{
		Clipboard:     clip,
		DropDir:       resolvedDrop,
		MaxPayload:    cfg.MaxPayloadBytes,
		MaxDropBytes:  cfg.MaxDropBytes,
		MaxDropFiles:  cfg.MaxDropFiles,
		MaxConcurrent: cfg.MaxConcurrent,
		Logger:        logger,
	})
	if err != nil {
		return fail(e, exitConfig, err)
	}

	ln, err := server.Listen(cfg.Address)
	if err != nil {
		return fail(e, exitFailure, err)
	}

	logger.Info("clipd starting",
		"version", version,
		"listen", ln.Addr().String(),
		"config", cfgPath,
		"drop_dir", resolvedDrop,
		"max_payload_bytes", cfg.MaxPayloadBytes)
	if !server.IsSocketPath(cfg.Address) {
		// Worth saying every time. Nothing in this daemon authenticates, so a
		// TCP listener is only ever safe on the loopback interface, and the
		// user has configured their way off the supported path to get here.
		logger.Warn("listening on TCP; this daemon has no authentication, so anything that can reach this address can write to the clipboard",
			"address", cfg.Address)
	}

	// Signals are handled here rather than in main so that short-lived
	// commands keep the default Ctrl-C behaviour.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx, ln); err != nil {
		return fail(e, exitFailure, err)
	}
	return exitOK
}

// loadConfig resolves and loads the daemon's configuration.
func loadConfig(g *globalOptions) (config.Config, string, error) {
	path, err := config.ResolvePath(g.configPath)
	if err != nil {
		return config.Config{}, "", err
	}
	cfg, err := config.Load(path)
	return cfg, path, err
}
