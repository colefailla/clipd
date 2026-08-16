package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/server"
	"github.com/colefailla/clipd/internal/transport"
)

// cmdServe runs the clipboard daemon in the foreground.
//
// It never detaches. Under the LaunchAgent, launchd is responsible for
// backgrounding, log redirection and restarts; a process that also tried to
// daemonize itself would just be a second opinion for launchd to disagree
// with.
func cmdServe(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "serve", "Usage: clipd serve [options]")
	bind := flags.String("bind", "", "listen address (default from config)")
	port := flags.Int("port", 0, "listen port (default from config)")
	if code, ok := flags.parse(args); !ok {
		return code
	}

	cfg, cfgPath, err := loadConfig(e, g)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	if *bind != "" {
		cfg.BindAddress = *bind
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if err := cfg.ValidateServer(); err != nil {
		return fail(e, exitConfig, err)
	}

	// The daemon generates a keypair rather than refusing to start without
	// one. Refusing would interact badly with the LaunchAgent's KeepAlive,
	// producing a restart loop every ten seconds; generating means the Mac
	// side always comes up and the problem surfaces client-side as a missing
	// fingerprint, which is one command to fix. Same precedent as sshd
	// creating host keys on first boot.
	certPath, keyPath := cfg.CertPath(cfgPath), cfg.KeyPath(cfgPath)
	created, err := transport.EnsureCert(certPath, keyPath, transport.DefaultValidity)
	if err != nil {
		return fail(e, exitTLS, err)
	}
	tlsConfig, err := transport.ServerConfig(certPath, keyPath)
	if err != nil {
		return fail(e, exitTLS, err)
	}

	clip, err := clipboard.New()
	if err != nil {
		if errors.Is(err, clipboard.ErrUnsupported) {
			return failf(e, exitFailure,
				"the clipd daemon runs on macOS only; on this machine use clipd as a client (see 'clipd help configure')")
		}
		return fail(e, exitFailure, err)
	}

	level := slog.LevelInfo
	if g.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: level}))

	srv, err := server.New(server.Options{
		Token:         cfg.Token,
		TLS:           tlsConfig,
		Clipboard:     clip,
		MaxPayload:    cfg.MaxPayloadBytes,
		MaxMemory:     cfg.MemoryBudget(),
		MaxConcurrent: cfg.Concurrency(),
		Timeout:       cfg.Timeout(),
		Logger:        logger,
	})
	if err != nil {
		return fail(e, exitConfig, err)
	}

	if created {
		logger.Warn("generated a new TLS keypair; clients must be configured with this fingerprint",
			"fingerprint", fingerprintOf(certPath), "cert", certPath)
	}

	ln, err := server.Listen(cfg.ListenAddress())
	if err != nil {
		return fail(e, exitFailure, err)
	}

	// Report the address actually bound, not the one requested: with port 0
	// or a hostname bind these differ, and the user needs the real one to
	// point a client at it.
	logger.Info("clipd starting",
		"version", version,
		"listen", ln.Addr().String(),
		"config", cfgPath,
		"fingerprint", fingerprintOf(certPath),
		// The resolved limits, not the configured ones: both have defaults
		// that depend on other fields, so the effective value is the only one
		// worth recording when diagnosing a busy daemon.
		"max_payload_bytes", cfg.MaxPayloadBytes,
		"memory_budget_bytes", cfg.MemoryBudget(),
		"max_concurrent", cfg.Concurrency())
	if cfg.BindAddress == "0.0.0.0" || cfg.BindAddress == "::" {
		logger.Info("listening on all interfaces; clients connect to this Mac's LAN address",
			"port", cfg.Port)
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

// fingerprintOf reads a certificate's pin for logging and status output. It
// returns a placeholder rather than an error because a daemon that is already
// serving should not be derailed by a cosmetic lookup.
func fingerprintOf(certPath string) string {
	cert, err := transport.LoadCertificate(certPath)
	if err != nil {
		return "(unavailable)"
	}
	return transport.FormatFingerprint(transport.Fingerprint(cert))
}
