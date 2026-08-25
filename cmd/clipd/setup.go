package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/colefailla/clipd/internal/server"
)

// Markers bracket the blocks clipd manages, in both the remote shell rc file
// and the local SSH config.
//
// They exist so setup is idempotent and reversible: re-running replaces the
// block rather than appending a second copy, and a user who wants clipd gone
// can delete from one marker to the other without having to work out which
// lines were theirs.
const (
	blockStart = "# >>> clipd >>>"
	blockEnd   = "# <<< clipd <<<"
)

// probeScript asks a remote host what it has, rather than assuming.
//
// This is the whole reason `setup` exists as a command instead of a paragraph
// in the README. The client is netcat, and netcat is not one program: the
// OpenBSD build speaks UNIX sockets with -U and half-closes with -N, the
// "traditional" build shipped by default on some Debian systems does neither,
// and the failure when you guess wrong is either "invalid option" or a copy
// that hangs forever with no output. Asking takes one round trip and removes
// the entire class of problem.
const probeScript = `
printf 'home=%s\n' "$HOME"
printf 'shell=%s\n' "${SHELL:-/bin/sh}"
if command -v tar >/dev/null 2>&1; then printf 'tar=yes\n'; fi
if command -v nc >/dev/null 2>&1; then
  printf 'nc=yes\n'
  if nc -h 2>&1 | grep -q -- '-U'; then printf 'nc_unix=yes\n'; fi
  if nc -h 2>&1 | grep -q -- '-N'; then printf 'nc_shutdown=yes\n'; fi
fi
if command -v socat >/dev/null 2>&1; then printf 'socat=yes\n'; fi
`

// remoteFacts is what the probe learned.
type remoteFacts struct {
	home       string
	shell      string
	hasTar     bool
	hasNC      bool
	ncUnix     bool
	ncShutdown bool
	hasSocat   bool
}

// cmdSetup configures a remote host to talk to this daemon.
func cmdSetup(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "setup",
		"Usage: clipd setup [options] <ssh-host>\n\n"+
			"Probes the host, installs a 'clipd' shell function there, and adds the\n"+
			"matching RemoteForward to your local SSH config.")
	printOnly := flags.Bool("print", false, "show what would be changed, without changing anything")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	if flags.NArg() != 1 {
		return failf(e, exitUsage, "setup needs exactly one ssh host, got %d", flags.NArg())
	}
	host := flags.Arg(0)

	cfg, _, err := loadConfig(g)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	localSocket, err := server.ExpandPath(cfg.Address)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	if !server.IsSocketPath(cfg.Address) {
		return failf(e, exitConfig,
			"setup forwards a UNIX socket, but this daemon is configured to listen on %q; set address to a path first",
			cfg.Address)
	}

	fmt.Fprintf(e.stdout, "Probing %s...\n", host)
	facts, err := probe(ctx, host)
	if err != nil {
		return fail(e, exitFailure, err)
	}

	client, err := clientCommand(facts)
	if err != nil {
		return fail(e, exitFailure, err)
	}
	remoteSocket := facts.home + "/.clipd.sock"
	rcFile := rcFileFor(facts)
	block := shellFunction(client, remoteSocket, facts.hasTar)

	fmt.Fprintf(e.stdout, "\n  remote shell   %s\n", facts.shell)
	fmt.Fprintf(e.stdout, "  remote client  %s\n", client)
	fmt.Fprintf(e.stdout, "  remote rc      %s\n", rcFile)
	fmt.Fprintf(e.stdout, "  remote socket  %s\n", remoteSocket)
	fmt.Fprintf(e.stdout, "  local socket   %s\n", localSocket)
	if !facts.hasTar {
		fmt.Fprintf(e.stdout, "\n  note: %s has no tar, so 'clipd drop' is unavailable there.\n", host)
	}

	forward := fmt.Sprintf("RemoteForward %s:%s", remoteSocket, localSocket)
	sshBlock := fmt.Sprintf("%s\nHost %s\n  %s\n%s\n", blockStart, host, forward, blockEnd)

	if *printOnly {
		fmt.Fprintf(e.stdout, "\n--- would append to %s on %s ---\n%s", rcFile, host, block)
		fmt.Fprintf(e.stdout, "\n--- would append to ~/.ssh/config ---\n%s", sshBlock)
		return exitOK
	}

	if err := installRemote(ctx, host, rcFile, block); err != nil {
		return fail(e, exitFailure, err)
	}
	fmt.Fprintf(e.stdout, "\nInstalled the clipd function in %s on %s.\n", rcFile, host)

	sshPath, changed, err := installSSHConfig(host, sshBlock)
	if err != nil {
		return fail(e, exitFailure, err)
	}
	if changed {
		fmt.Fprintf(e.stdout, "Added the socket forward to %s.\n", sshPath)
	} else {
		fmt.Fprintf(e.stdout, "The socket forward in %s was already current.\n", sshPath)
	}

	fmt.Fprintf(e.stdout, `
Reconnect for the forward to take effect:

  ssh -O exit %s 2>/dev/null; ssh %s

Then, on %s:

  ls -l | clipd
  clipd drop notes.txt

An existing SSH session will not have the forward. If you multiplex with
ControlMaster, the old master has to go first, which is what the -O exit above
is for.
`, host, host, host)
	return exitOK
}

// probe runs probeScript on the host and parses its key=value output.
func probe(ctx context.Context, host string) (remoteFacts, error) {
	// BatchMode so a host needing a password fails with a message instead of
	// silently blocking on a prompt nothing is reading.
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", host, "sh -s")
	cmd.Stdin = strings.NewReader(probeScript)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return remoteFacts{}, fmt.Errorf("probe %s: %s", host, msg)
	}

	facts := remoteFacts{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "home":
			facts.home = strings.TrimRight(value, "/")
		case "shell":
			facts.shell = value
		case "tar":
			facts.hasTar = true
		case "nc":
			facts.hasNC = true
		case "nc_unix":
			facts.ncUnix = true
		case "nc_shutdown":
			facts.ncShutdown = true
		case "socat":
			facts.hasSocat = true
		}
	}
	if facts.home == "" {
		return facts, fmt.Errorf("probe %s: the host did not report a home directory", host)
	}
	return facts, nil
}

// clientCommand picks the command that writes to the socket, given what the
// host actually has.
//
// The ordering prefers netcat because it is more widely present, and falls
// back to socat because some netcat builds cannot speak UNIX sockets at all.
// When neither can, the error says exactly which package fixes it rather than
// leaving the user to decode "invalid option -- 'U'".
func clientCommand(f remoteFacts) (string, error) {
	switch {
	case f.hasNC && f.ncUnix && f.ncShutdown:
		return `nc -N -U "$sock"`, nil
	case f.hasNC && f.ncUnix:
		// Without -N, nc keeps the connection open after stdin ends and waits
		// for the daemon to hang up first. The daemon's acknowledgement is
		// what releases it, so this works — it just cannot half-close early.
		return `nc -U "$sock"`, nil
	case f.hasSocat:
		return `socat - UNIX-CLIENT:"$sock"`, nil
	case f.hasNC:
		return "", fmt.Errorf("the host's netcat has no -U flag, so it cannot use a UNIX socket: install netcat-openbsd (Debian/Ubuntu) or socat")
	default:
		return "", fmt.Errorf("the host has neither nc nor socat: install netcat-openbsd or socat")
	}
}

// rcFileFor picks the startup file for the remote user's shell.
func rcFileFor(f remoteFacts) string {
	switch filepath.Base(f.shell) {
	case "zsh":
		return "$HOME/.zshrc"
	case "bash":
		return "$HOME/.bashrc"
	default:
		// Every POSIX shell reads this one, so it is the safe fallback for a
		// shell whose own rc file we cannot name.
		return "$HOME/.profile"
	}
}

// shellFunction renders the client the remote host will run.
//
// It is a shell function rather than a binary because that is the whole point:
// nothing is installed on the remote side. The daemon's own name is what the
// user types, so the two halves stay in sync in their head even though only
// one of them is a program.
func shellFunction(client, socket string, hasTar bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", blockStart)
	fmt.Fprintf(&b, "# Sends to the clipd daemon on the Mac, through the socket SSH forwards.\n")
	fmt.Fprintf(&b, "# Managed by 'clipd setup'; edits between these markers are overwritten.\n")
	fmt.Fprintf(&b, "clipd() {\n")
	fmt.Fprintf(&b, "  sock=%q\n", socket)
	fmt.Fprintf(&b, "  if [ ! -S \"$sock\" ]; then\n")
	fmt.Fprintf(&b, "    printf 'clipd: %%s is missing; reconnect with the socket forward\\n' \"$sock\" >&2\n")
	fmt.Fprintf(&b, "    return 1\n")
	fmt.Fprintf(&b, "  fi\n")
	fmt.Fprintf(&b, "  if [ \"$1\" = \"drop\" ]; then\n")
	fmt.Fprintf(&b, "    shift\n")
	if hasTar {
		fmt.Fprintf(&b, "    if [ $# -eq 0 ]; then printf 'clipd drop: no files given\\n' >&2; return 64; fi\n")
		// The magic line and the envelope, then the archive, all on one
		// connection. printf rather than echo because echo's handling of
		// backslashes varies between shells.
		// COPYFILE_DISABLE stops macOS tar from emitting an AppleDouble "._"
		// companion for every file, which would otherwise double the count and
		// litter the drop directory with metadata nobody asked for. It is an
		// unknown variable on Linux, and ignored there.
		fmt.Fprintf(&b, "    { printf 'clipd:magic:v1\\n{\"type\":\"drop\"}\\n'; COPYFILE_DISABLE=1 tar cf - \"$@\"; } | %s\n", client)
	} else {
		fmt.Fprintf(&b, "    printf 'clipd drop: this host has no tar\\n' >&2; return 1\n")
	}
	fmt.Fprintf(&b, "  else\n")
	fmt.Fprintf(&b, "    %s\n", client)
	fmt.Fprintf(&b, "  fi\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "%s\n", blockEnd)
	return b.String()
}

// installScript rewrites the managed block in a remote rc file.
//
// The awk pass drops any previous block before the new one is appended, so
// running setup twice leaves one function rather than two. Writing through
// `cat >` rather than `mv` keeps the rc file's existing inode and permissions:
// a temp file created by mktemp is 0600, and silently tightening someone's
// .bashrc is not setup's business.
const installScript = `set -e
rc="%s"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
if [ -f "$rc" ]; then
  awk '/^# >>> clipd >>>$/{skip=1} skip==0{print} /^# <<< clipd <<<$/{skip=0}' "$rc" > "$tmp"
else
  : > "$tmp"
fi
cat >> "$tmp" <<'CLIPD_EOF'
%s
CLIPD_EOF
touch "$rc"
cat "$tmp" > "$rc"
`

// installRemote writes the shell function into the host's rc file.
func installRemote(ctx context.Context, host, rcFile, block string) error {
	script := fmt.Sprintf(installScript, rcFile, strings.TrimRight(block, "\n"))
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", host, "sh -s")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("install the clipd function on %s: %s", host, msg)
	}
	return nil
}

// installSSHConfig adds the socket forward to the user's SSH config, replacing
// any block a previous run left.
//
// The block is a Host stanza of its own rather than an edit to one the user
// already wrote. SSH accumulates RemoteForward across matching blocks, so this
// adds the forward without touching, reordering, or having to parse whatever
// else is in the file.
func installSSHConfig(host, block string) (path string, changed bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("locate home directory: %w", err)
	}
	dir := filepath.Join(home, ".ssh")
	path = filepath.Join(dir, "config")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return path, false, fmt.Errorf("create %s: %w", dir, err)
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return path, false, fmt.Errorf("read %s: %w", path, err)
	}

	stripped := stripBlock(string(existing))
	updated := stripped
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	if updated != "" {
		updated += "\n"
	}
	updated += block

	if string(existing) == updated {
		return path, false, nil
	}
	// A backup before the first edit, because this file is often hand-tuned
	// and losing it is a bad afternoon.
	if len(existing) > 0 {
		if err := os.WriteFile(path+".clipd-backup", existing, 0o600); err != nil {
			return path, false, fmt.Errorf("back up %s: %w", path, err)
		}
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return path, false, fmt.Errorf("write %s: %w", path, err)
	}
	return path, true, nil
}

// stripBlock removes a previously managed block, markers included.
func stripBlock(s string) string {
	var out []string
	skip := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == blockStart {
			skip = true
			continue
		}
		if trimmed == blockEnd {
			skip = false
			continue
		}
		if !skip {
			out = append(out, line)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}
