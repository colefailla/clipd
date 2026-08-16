package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/colefailla/clipd/internal/auth"
	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/launchagent"
	"github.com/colefailla/clipd/internal/transport"
)

// cmdStatus reports the resolved configuration, the local daemon's state and
// whether the configured server answers.
func cmdStatus(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "status", "Usage: clipd status [options]")
	if code, ok := flags.parse(args); !ok {
		return code
	}

	cfg, path, err := loadConfig(e, g)
	if err != nil {
		return fail(e, exitConfig, err)
	}

	out := e.stdout
	fmt.Fprintf(out, "clipd %s (%s/%s)\n\n", version, runtime.GOOS, runtime.GOARCH)

	fmt.Fprintf(out, "configuration\n")
	if config.Exists(path) {
		fmt.Fprintf(out, "  file         %s\n", path)
		if warning := permissionWarning(path, "the token"); warning != "" {
			fmt.Fprintf(out, "  warning      %s\n", warning)
		}
	} else {
		fmt.Fprintf(out, "  file         %s (not created yet)\n", path)
	}
	fmt.Fprintf(out, "  token        %s\n", auth.Redact(cfg.Token))
	fmt.Fprintf(out, "  max payload  %s\n", config.FormatSize(cfg.MaxPayloadBytes))
	fmt.Fprintf(out, "  timeout      %s\n", cfg.Timeout())

	if runtime.GOOS == "darwin" {
		fmt.Fprintf(out, "\ndaemon (this machine)\n")
		fmt.Fprintf(out, "  listen       %s\n", cfg.ListenAddress())
		// Effective values: both fall back to defaults that depend on other
		// settings, so printing what is stored would mislead more than help.
		fmt.Fprintf(out, "  memory       %s across %d concurrent copies\n",
			config.FormatSize(cfg.MemoryBudget()), cfg.Concurrency())
		reportCertificate(out, cfg.CertPath(path))
		// The private key is the daemon's identity: anyone holding it can
		// impersonate this Mac to every client pinned to it, and unlike the
		// token there is no way to notice that from the client side.
		if warning := permissionWarning(cfg.KeyPath(path), "the daemon's private key"); warning != "" {
			fmt.Fprintf(out, "  warning      %s\n", warning)
		}
		reportAgent(ctx, out)
	}

	fmt.Fprintf(out, "\nclient\n")
	if cfg.ServerAddress == "" {
		fmt.Fprintf(out, "  server       (not configured — run 'clipd configure')\n")
		return exitOK
	}
	fmt.Fprintf(out, "  server       %s\n", cfg.DialAddress())
	if cfg.ServerFingerprint == "" {
		fmt.Fprintf(out, "  fingerprint  (not configured — run 'clipd configure -fingerprint')\n")
		fmt.Fprintf(out, "  reachable    %s\n", probeTCP(cfg.DialAddress(), cfg.Timeout()))
		return exitOK
	}
	fmt.Fprintf(out, "  fingerprint  %s\n", cfg.ServerFingerprint)
	reportProbe(out, cfg)
	return exitOK
}

// reportCertificate describes the local daemon's keypair.
func reportCertificate(out io.Writer, certPath string) {
	cert, err := transport.LoadCertificate(certPath)
	if err != nil {
		fmt.Fprintf(out, "  certificate  none yet (created on first start)\n")
		return
	}
	fmt.Fprintf(out, "  fingerprint  %s\n", transport.FormatFingerprint(transport.Fingerprint(cert)))

	remaining := time.Until(cert.NotAfter)
	switch {
	case remaining <= 0:
		fmt.Fprintf(out, "  certificate  EXPIRED on %s — run 'clipd setup -rotate-cert'\n",
			cert.NotAfter.Format("2006-01-02"))
	case remaining < transport.ExpiryWarning:
		fmt.Fprintf(out, "  certificate  expires %s (%s) — rotation will require reconfiguring clients\n",
			cert.NotAfter.Format("2006-01-02"), humanUntil(cert.NotAfter))
	default:
		fmt.Fprintf(out, "  certificate  valid until %s (%s)\n",
			cert.NotAfter.Format("2006-01-02"), humanUntil(cert.NotAfter))
	}
}

// reportProbe performs a full TLS handshake against the configured daemon and
// stops there — no token, no payload.
//
// This makes status a genuine preflight: it answers "is my pin correct"
// without a copy having to fail to find out.
func reportProbe(out io.Writer, cfg config.Config) {
	address := cfg.DialAddress()
	timeout := cfg.Timeout()

	pin, err := transport.ParseFingerprint(cfg.ServerFingerprint)
	if err != nil {
		fmt.Fprintf(out, "  reachable    unknown (%v)\n", err)
		return
	}
	tlsConfig, err := transport.ClientConfig(pin)
	if err != nil {
		fmt.Fprintf(out, "  reachable    unknown (%v)\n", err)
		return
	}

	start := time.Now()
	rawConn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		fmt.Fprintf(out, "  reachable    no (%v)\n", err)
		return
	}
	defer rawConn.Close()
	fmt.Fprintf(out, "  reachable    yes (%dms)\n", time.Since(start).Milliseconds())

	_ = rawConn.SetDeadline(time.Now().Add(timeout))
	conn := tls.Client(rawConn, tlsConfig)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := conn.HandshakeContext(ctx); err != nil {
		var mismatch *transport.PinMismatchError
		if errors.As(err, &mismatch) {
			fmt.Fprintf(out, "  pin          MISMATCH\n")
			fmt.Fprintf(out, "               expected %s\n", transport.FormatFingerprint(mismatch.Want))
			fmt.Fprintf(out, "               server   %s\n", transport.FormatFingerprint(mismatch.Got))
			return
		}
		fmt.Fprintf(out, "  pin          cannot verify (%v)\n", err)
		return
	}
	defer conn.Close()

	state := conn.ConnectionState()
	if len(state.PeerCertificates) > 0 {
		expiry := state.PeerCertificates[0].NotAfter
		fmt.Fprintf(out, "  pin          matches (server certificate valid until %s)\n",
			expiry.Format("2006-01-02"))
		return
	}
	fmt.Fprintf(out, "  pin          matches\n")
}

// reportAgent prints the LaunchAgent's state.
func reportAgent(ctx context.Context, out io.Writer) {
	state, err := launchagent.Status(ctx)
	if err != nil {
		fmt.Fprintf(out, "  LaunchAgent  unknown (%v)\n", err)
		return
	}
	if !state.PlistInstalled {
		fmt.Fprintf(out, "  LaunchAgent  not installed (run 'clipd install')\n")
		return
	}
	fmt.Fprintf(out, "  LaunchAgent  %s\n", state.PlistPath)
	switch {
	case state.Loaded && state.PID > 0:
		fmt.Fprintf(out, "  launchd      running (pid %d)\n", state.PID)
	case state.Loaded:
		fmt.Fprintf(out, "  launchd      loaded but not running\n")
	default:
		fmt.Fprintf(out, "  launchd      not loaded (run 'clipd install')\n")
	}
	// A non-zero previous exit is the one thing worth surfacing unprompted:
	// KeepAlive restarts the daemon after a crash, so without this the
	// failure leaves no trace anywhere the user would think to look.
	if state.LastExitStatus != 0 {
		fmt.Fprintf(out, "  last exit    %d (see %s)\n", state.LastExitStatus, state.LogPath)
	}
	if state.LogPath != "" {
		fmt.Fprintf(out, "  log          %s\n", state.LogPath)
	}
}

// probeTCP opens a TCP connection and closes it immediately, for the case
// where no fingerprint is configured and a handshake is therefore impossible.
//
// It sends no token: reachability is a network question, and answering it
// should not put a secret on the wire.
func probeTCP(address string, timeout time.Duration) string {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return fmt.Sprintf("no (%v)", err)
	}
	_ = conn.Close()
	return fmt.Sprintf("yes (%dms)", time.Since(start).Milliseconds())
}

// permissionWarning flags a file readable by anyone but its owner.
//
// Both files it is used on are secrets that clipd creates 0600, so a loose
// mode means something outside clipd changed it: a restore from backup, a cp
// that did not preserve modes, an editor writing through a new inode. what
// names the secret, because "permissions are 0644" is only alarming if the
// reader knows what is in the file.
func permissionWarning(path, what string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Sprintf("permissions are %04o; %s is readable by other users (chmod 600 %s)",
			mode, what, path)
	}
	return ""
}
