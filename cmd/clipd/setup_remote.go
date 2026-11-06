package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxProbeOutput limits captured stdout and stderr from each remote command.
const maxProbeOutput = 64 << 10

// setupCommandTimeout allows time for an interactive SSH password prompt.
const setupCommandTimeout = 3 * time.Minute

// probeScript reports the account, home directory, shell and available tools.
const probeScript = `
printf 'home=%s\n' "$HOME"
printf 'user=%s\n' "$(id -un)"
printf 'os=%s\n' "$(uname -s)"
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
	user       string
	os         string
	shell      string
	hasTar     bool
	hasNC      bool
	ncUnix     bool
	ncShutdown bool
	hasSocat   bool
}

// unsupportedShell identifies login shells that cannot load the POSIX function.
func unsupportedShell(shell string) bool {
	switch filepath.Base(shell) {
	case "fish", "csh", "tcsh":
		return true
	}
	return false
}

// sshOptions uses a private shared connection for setup, with forwarding disabled.
// Password prompts remain available through SSH's controlling terminal.
func sshOptions(controlPath string) []string {
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlPath,
		"-o", "ControlPersist=30",
		"-o", "ClearAllForwardings=yes",
	}
}

// controlPath allocates a private directory for setup's SSH control socket.
func controlPath() (string, func(), error) {
	dir, err := os.MkdirTemp("", "clipd")
	if err != nil {
		return "", func() {}, fmt.Errorf("create control directory: %w", err)
	}
	return filepath.Join(dir, "c"), func() { os.RemoveAll(dir) }, nil
}

// capWriter keeps the first max bytes written to it and discards the rest.
//
// Every write is reported as fully accepted, so a remote command that keeps
// producing output is not killed with EPIPE partway through something else.
type capWriter struct {
	b   bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	written := len(p)
	if room := w.max - w.b.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		w.b.Write(p)
	}
	return written, nil
}

func (w *capWriter) String() string { return w.b.String() }

// runRemote bounds the duration and captured output of one remote script.
func runRemote(ctx context.Context, destination, control, script string) (stdout string, err error) {
	ctx, cancel := context.WithTimeout(ctx, setupCommandTimeout)
	defer cancel()

	args := append(sshOptions(control), destination, "sh -s")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = strings.NewReader(script)
	out := &capWriter{max: maxProbeOutput}
	errOut := &capWriter{max: maxProbeOutput}
	cmd.Stdout = out
	cmd.Stderr = errOut

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), errors.New(msg)
	}
	return out.String(), nil
}

// probe runs probeScript on the host and parses its key=value output.
func probe(ctx context.Context, destination, control string) (remoteFacts, error) {
	stdout, err := runRemote(ctx, destination, control, probeScript)
	if err != nil {
		return remoteFacts{}, fmt.Errorf("probe %s: %s", destination, err)
	}

	facts := remoteFacts{}
	for _, line := range strings.Split(stdout, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "home":
			facts.home = strings.TrimRight(value, "/")
		case "user":
			facts.user = value
		case "os":
			facts.os = value
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
	if err := validSSHUser(facts.user); err != nil {
		return facts, fmt.Errorf("probe %s: %w", destination, err)
	}
	if facts.home == "" {
		return facts, fmt.Errorf("probe %s: the host did not report a home directory", destination)
	}
	return facts, nil
}

// transport is how the generated function reaches the socket.
type transport struct {
	// send carries a transfer, and waits up to a day for the Mac's reply.
	send string

	// probe is the stale-socket check, which gives up after probeSeconds of
	// silence. That bounds a socket that accepts and never answers, such as
	// one an unreachable earlier session still holds; it is an idle timeout,
	// not an absolute deadline.
	probe string

	// silentRefusal marks macOS nc, which exits 1 without a message when a
	// connection is refused. It also does so for some other failures before
	// connecting, so for this client a silent exit 1 is a heuristic for a
	// stale socket rather than proof of one.
	silentRefusal bool
}

// probeSeconds is the stale-socket check's inactivity timeout (nc -w,
// socat -T and -t). It ends a connection that stays silent; it is not an
// absolute deadline for a connect that blocks or a peer that keeps sending.
const probeSeconds = 5

// clientCommand selects a UNIX-socket transport with compatible EOF handling.
func clientCommand(f remoteFacts) (transport, error) {
	switch {
	case f.os == "Darwin" && f.hasNC && f.ncUnix:
		// macOS ships its own netcat, where -N is not the half-close flag at
		// all — it takes a probe count for a write timeout, and passing it the
		// way OpenBSD's is passed fails with "invalid tcp adaptive write
		// timeout value". This netcat closes the socket on stdin EOF anyway,
		// so the flag is not needed. Checked ahead of the -N cases because the
		// flag is present here and means something else.
		return transport{
			send:          `nc -U -w 86400 "$_clipd_sock"`,
			probe:         fmt.Sprintf(`nc -U -w %d "$_clipd_sock"`, probeSeconds),
			silentRefusal: true,
		}, nil
	case f.hasNC && f.ncUnix && f.ncShutdown:
		return transport{
			send:  `nc -N -U -w 86400 "$_clipd_sock"`,
			probe: fmt.Sprintf(`nc -N -U -w %d "$_clipd_sock"`, probeSeconds),
		}, nil
	case f.hasSocat:
		return transport{
			send:  `socat -T 86400 -t 86400 - UNIX-CLIENT:"$_clipd_sock"`,
			probe: fmt.Sprintf(`socat -T %d -t %d - UNIX-CLIENT:"$_clipd_sock"`, probeSeconds, probeSeconds),
		}, nil
	case f.hasNC && f.ncUnix:
		// -U but no -N, on something that is not macOS: netcat-openbsd from
		// before the flag existed. Without a half-close the daemon never sees
		// the end of the message, so it never replies and the client waits out
		// its deadline. socat is preferred above; reaching here means there is
		// none.
		return transport{}, errors.New("the host's netcat cannot half-close a connection (no -N flag), so a copy would hang: install a newer netcat-openbsd, or socat")
	case f.hasNC:
		return transport{}, errors.New("the host's netcat has no -U flag, so it cannot use a UNIX socket: install netcat-openbsd (Debian/Ubuntu) or socat")
	default:
		return transport{}, errors.New("the host has neither nc nor socat: install netcat-openbsd or socat")
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
		// Use the login profile for other POSIX shells.
		return "$HOME/.profile"
	}
}

// installScript validates markers and stages the new rc contents before writing.
// The final cat preserves the rc inode, symlink and permissions but is not
// crash-atomic. The first backup is retained across subsequent setup runs.
const installScript = `set -e
rc="%s"
start='` + blockStart + `'
end='` + blockEnd + `'

# The socket gets a directory of its own, mode 0700. OpenSSH creates the socket
# itself 0600 by default, but it documents that not every operating system
# honours a socket's mode — and every one of them honours a directory's. On a
# shared host this is what stops another account reaching the forward.
mkdir -p "$HOME/.clipd"
chmod 700 "$HOME/.clipd"
_clipd_sock="$HOME/.clipd/socket"
%s
[ "$_clipd_stale" = 0 ] || printf 'clipd-removed-stale-socket\n'

# Made by hand rather than with mktemp, which is not in POSIX: clipd asks a host
# for tar, a shell and one of nc or socat, and nothing else. This file is inside
# clipd's private directory, and noclobber refuses the vanishingly rare
# stale-file/PID-reuse collision rather than following or overwriting it.
tmp="$HOME/.clipd/setup.$$"
if ! (umask 077; set -C; : > "$tmp") 2>/dev/null; then
  printf 'clipd: could not create private setup file %%s\n' "$tmp" >&2
  exit 1
fi
trap 'rm -f "$tmp"' 0
if [ -f "$rc" ]; then
  if ! awk -v s="$start" -v e="$end" '
    {
      line=$0
      sub(/^[[:space:]]+/, "", line)
      sub(/[[:space:]]+$/, "", line)
      if (line == s) {
        if (open || seen) bad=1
        open=1
        next
      }
      if (line == e) {
        if (!open) bad=1
        open=0
        seen=1
        next
      }
      if (!open) print
    }
    END { if (open || bad) exit 1 }
  ' "$rc" > "$tmp"; then
    printf 'clipd: %%s has malformed clipd markers; expected at most one start followed by one end\n' "$rc" >&2
    exit 1
  fi
  # The first backup is the one worth keeping: it is the file before clipd
  # touched it. Later runs would replace it with a copy that already has a
  # clipd block in it.
  [ -f "$rc.clipd-backup" ] || cp "$rc" "$rc.clipd-backup"
else
  : > "$tmp"
fi
cat >> "$tmp" <<'CLIPD_EOF'
%s
CLIPD_EOF
touch "$rc"
cat "$tmp" > "$rc"
`

// installRemote writes the shell function into the host's rc file, and reports
// whether it removed a socket an earlier session left behind.
func installRemote(ctx context.Context, destination, control, rcFile string, client transport, block string) (bool, error) {
	script := fmt.Sprintf(installScript, rcFile, staleSocketScript(client), strings.TrimRight(block, "\n"))
	out, err := runRemote(ctx, destination, control, script)
	if err != nil {
		return false, fmt.Errorf("install the clipd function on %s: %s", destination, err)
	}
	return strings.Contains(out, "clipd-removed-stale-socket"), nil
}
