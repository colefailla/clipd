package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/colefailla/clipd/internal/auth"
	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/transport"
)

// cmdSetup initialises the Mac's configuration and token.
//
// There is no pairing protocol. These are two machines with one owner;
// establishing trust between them is a matter of the owner copying a token
// once, and any handshake built to automate that would be more machinery than
// the problem deserves.
func cmdSetup(_ context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "setup", "Usage: clipd setup [options]")
	bind := flags.String("bind", "", "listen address to store (default 0.0.0.0)")
	port := flags.Int("port", 0, "listen port to store")
	maxPayload := flags.String("max-payload", "", "maximum accepted payload, e.g. 10MB")
	rotate := flags.Bool("rotate", false, "generate a new token, invalidating the current one")
	rotateCert := flags.Bool("rotate-cert", false, "generate a new TLS keypair; every client must be reconfigured")
	if code, ok := flags.parse(args); !ok {
		return code
	}

	// File and flags only: a CLIPD_* override active during setup must not
	// be baked into the stored config — with CLIPD_TOKEN set it would even
	// be persisted as the daemon's permanent secret in place of a generated
	// one.
	cfg, path, err := loadFileConfig(e, g)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	if *bind != "" {
		cfg.BindAddress = *bind
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if *maxPayload != "" {
		size, err := config.ParseSize(*maxPayload)
		if err != nil {
			return failf(e, exitUsage, "-max-payload: %v", err)
		}
		cfg.MaxPayloadBytes = size
	}

	generated := false
	if cfg.Token == "" || *rotate {
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

	certPath, keyPath := cfg.CertPath(path), cfg.KeyPath(path)
	certCreated := false
	switch {
	case *rotateCert:
		if err := transport.WriteCert(certPath, keyPath, transport.DefaultValidity); err != nil {
			return fail(e, exitTLS, err)
		}
		certCreated = true
	default:
		created, err := transport.EnsureCert(certPath, keyPath, transport.DefaultValidity)
		if err != nil {
			return fail(e, exitTLS, err)
		}
		certCreated = created
	}

	cert, err := transport.LoadCertificate(certPath)
	if err != nil {
		return fail(e, exitTLS, err)
	}
	fingerprint := transport.FormatFingerprint(transport.Fingerprint(cert))

	out := e.stdout
	if generated {
		if *rotate {
			fmt.Fprintf(out, "Generated a new token. Every client must be reconfigured.\n\n")
		} else {
			fmt.Fprintf(out, "Generated an authentication token.\n\n")
		}
	} else {
		fmt.Fprintf(out, "Using the existing token. Run 'clipd setup -rotate' to replace it.\n\n")
	}

	if *rotateCert {
		fmt.Fprintf(out, "Generated a new TLS keypair. Every client must be reconfigured.\n\n")
	}

	fmt.Fprintf(out, "  config       %s\n", path)
	fmt.Fprintf(out, "  listen       %s\n", cfg.ListenAddress())
	fmt.Fprintf(out, "  certificate  %s\n", certPath)
	fmt.Fprintf(out, "  expires      %s (%s)\n",
		cert.NotAfter.Format("2006-01-02"), humanUntil(cert.NotAfter))
	fmt.Fprintf(out, "  max payload  %s\n", config.FormatSize(cfg.MaxPayloadBytes))

	// The token is printed here, and only here, because handing it to the
	// user is the entire point of setup. It is never written to a log.
	fmt.Fprintf(out, "\nAuthentication token (treat it like a password):\n\n  %s\n", cfg.Token)
	// The fingerprint is not a secret — it is the server's identity, and
	// publishing it is what makes impersonation detectable.
	fmt.Fprintf(out, "\nServer fingerprint (not secret — this is what clients verify):\n\n  %s\n", fingerprint)

	fmt.Fprintf(out, "\nNext:\n")
	fmt.Fprintf(out, "  1. On this Mac:  clipd install\n")
	fmt.Fprintf(out, "  2. On the client machine:\n\n")
	fmt.Fprintf(out, "       clipd configure -server %s -port %d \\\n", suggestedServerAddress(), cfg.Port)
	fmt.Fprintf(out, "         -fingerprint %s \\\n", fingerprint)
	fmt.Fprintf(out, "         -token -\n\n")
	// The token is deliberately absent from the suggested command. A command
	// line is not private: on Linux any local user can read another process's
	// argv out of /proc, and the shell writes it to history besides. Reading
	// the secret from stdin costs one paste and keeps it out of both.
	fmt.Fprintf(out, "     and paste the token when it asks. Passing it as -token <value>\n")
	fmt.Fprintf(out, "     instead would expose it: other local users can read a command\n")
	fmt.Fprintf(out, "     line from ps, and the shell records it in history.\n\n")
	fmt.Fprintf(out, "  3. Test it:  echo hello | clipd\n")

	if certCreated && !*rotateCert {
		fmt.Fprintf(out, "\nA TLS keypair was generated for this daemon.\n")
	}
	if cfg.BindAddress == "0.0.0.0" || cfg.BindAddress == "::" {
		fmt.Fprintf(out, "\nThe daemon accepts connections from any interface. Traffic is encrypted\n")
		fmt.Fprintf(out, "with TLS 1.3 and clients verify the fingerprint above.\n")
	}
	return exitOK
}

// humanUntil renders a remaining lifetime, so an expiry date is legible
// without arithmetic.
func humanUntil(t time.Time) string {
	remaining := time.Until(t)
	if remaining <= 0 {
		return "EXPIRED"
	}
	days := int(remaining.Hours() / 24)
	switch {
	case days > 365:
		return fmt.Sprintf("%.1f years", float64(days)/365)
	case days > 1:
		return fmt.Sprintf("%d days", days)
	default:
		return "under a day"
	}
}

// suggestedServerAddress offers a starting point for the client's -server
// value.
//
// This is a hint, not discovery: it is one stdlib call, and the user is told
// to substitute their LAN address if the hostname does not resolve. Probing
// interfaces or announcing over mDNS would be a networking subsystem in
// service of saving one lookup.
func suggestedServerAddress() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "<mac-address-or-hostname>"
	}
	return host
}
