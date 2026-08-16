package main

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/colefailla/clipd/internal/auth"
	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/transport"
)

// cmdConfigure stores the server address, port and token on a client machine.
//
// With flags it is scriptable; on a terminal with no flags it prompts. It
// refuses to prompt when stdin is not a terminal, so a misuse in a script
// fails instead of hanging on a read that will never be answered.
func cmdConfigure(_ context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "configure", "Usage: clipd configure [options]")
	server := flags.String("server", "", "Mac hostname or IP address")
	port := flags.Int("port", 0, "port the daemon listens on")
	token := flags.String("token", "", `authentication token, or "-" to read it from stdin`)
	fingerprint := flags.String("fingerprint", "", "server key fingerprint, as printed by `clipd setup`")
	maxPayload := flags.String("max-payload", "", "maximum payload to send, e.g. 10MB")
	if code, ok := flags.parse(args); !ok {
		return code
	}

	// loadFileConfig folds the global -timeout flag into cfg, so `clipd
	// configure -timeout 10s` stores the value it parsed — but skips the
	// CLIPD_* environment overrides, which exist for one-off runs and must
	// not be silently persisted by a configure that never mentioned them.
	cfg, path, err := loadFileConfig(e, g)
	if err != nil {
		return fail(e, exitConfig, err)
	}

	anyFlag := *server != "" || *port != 0 || *token != "" || *fingerprint != "" ||
		*maxPayload != "" || g.timeout != ""

	if *server != "" {
		cfg.ServerAddress = *server
	}
	if *fingerprint != "" {
		// Normalised on the way in so the stored form is canonical whatever
		// the user pasted.
		pin, err := transport.ParseFingerprint(*fingerprint)
		if err != nil {
			return failf(e, exitUsage, "-fingerprint: %v", err)
		}
		cfg.ServerFingerprint = transport.FormatFingerprint(pin)
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
	if *token != "" {
		value := *token
		if value == "-" {
			// On a terminal, say what is being waited for: a command that
			// silently blocks on a read looks like one that has hung.
			if !e.stdinIsPipe {
				fmt.Fprint(e.stdout, "Authentication token (from 'clipd setup' on the Mac): ")
			}
			line, err := readLine(e)
			if err != nil {
				return failf(e, exitConfig, "read token from stdin: %v", err)
			}
			value = line
		}
		cfg.Token = value
	}

	if !anyFlag {
		if e.stdinIsPipe {
			return failf(e, exitUsage,
				"configure needs flags when stdin is not a terminal: see 'clipd help configure'")
		}
		if code := promptForConfig(e, &cfg); code != exitOK {
			return code
		}
	}

	if err := auth.Validate(cfg.Token); err != nil {
		return fail(e, exitConfig, err)
	}
	if err := cfg.ValidateClient(); err != nil {
		return fail(e, exitConfig, err)
	}
	if err := cfg.Save(path); err != nil {
		return fail(e, exitConfig, err)
	}

	fmt.Fprintf(e.stdout, "Saved %s\n\n", path)
	fmt.Fprintf(e.stdout, "  server       %s\n", cfg.DialAddress())
	fmt.Fprintf(e.stdout, "  token        %s\n", auth.Redact(cfg.Token))
	fmt.Fprintf(e.stdout, "  fingerprint  %s\n", cfg.ServerFingerprint)
	fmt.Fprintf(e.stdout, "  max payload  %s\n", config.FormatSize(cfg.MaxPayloadBytes))
	fmt.Fprintf(e.stdout, "  timeout      %s\n", cfg.Timeout())
	fmt.Fprintf(e.stdout, "\nTest it:  echo hello | clipd -v\n")
	return exitOK
}

// promptForConfig asks for each value interactively, offering the current
// setting as the default.
//
// The token is echoed as it is typed. Hiding it would mean either a terminal
// dependency (golang.org/x/term) or raw-mode tricks, and it is typed once, on
// the user's own screen, from a value they just read off another screen.
func promptForConfig(e *env, cfg *config.Config) int {
	reader := bufio.NewReader(e.stdin)

	addr, err := prompt(e, reader, "Mac hostname or IP address", cfg.ServerAddress)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	if addr == "" {
		return failf(e, exitConfig, "a server address is required")
	}
	cfg.ServerAddress = addr

	portStr, err := prompt(e, reader, "Port", strconv.Itoa(cfg.Port))
	if err != nil {
		return fail(e, exitConfig, err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return failf(e, exitConfig, "%q is not a port number", portStr)
	}
	cfg.Port = p

	existing := ""
	if cfg.Token != "" {
		existing = auth.Redact(cfg.Token)
	}
	tok, err := prompt(e, reader, "Authentication token (from 'clipd setup' on the Mac)", existing)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	// An empty answer keeps the stored token, which is what the redacted
	// default shown above implies.
	if tok != "" && tok != existing {
		cfg.Token = tok
	}

	fp, err := prompt(e, reader, "Server fingerprint (from the same output)", cfg.ServerFingerprint)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	if fp == "" {
		return failf(e, exitConfig, "a server fingerprint is required")
	}
	pin, err := transport.ParseFingerprint(fp)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	cfg.ServerFingerprint = transport.FormatFingerprint(pin)

	fmt.Fprintln(e.stdout)
	return exitOK
}

func prompt(e *env, r *bufio.Reader, label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(e.stdout, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(e.stdout, "%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// readLine reads a single line from stdin, used for `-token -`.
func readLine(e *env) (string, error) {
	r := bufio.NewReader(e.stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
