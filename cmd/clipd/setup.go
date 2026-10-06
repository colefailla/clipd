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

	"github.com/colefailla/clipd/internal/server"
)

// Markers bracket the blocks clipd manages.
//
// They exist so setup is idempotent and reversible: re-running replaces the
// block rather than appending a second copy, and a user who wants clipd gone
// can delete from one marker to the other without having to work out which
// lines were theirs.
//
// The remote rc file uses these plain markers, because a host has exactly one
// clipd function however many names you reach it by. The local SSH config
// cannot: setup is run once per destination and every destination needs its own
// block, so there the markers carry the destination. See sshMarkers.
const (
	blockStart = "# >>> clipd >>>"
	blockEnd   = "# <<< clipd <<<"
)

// sshMarkers returns the markers bracketing one destination's block in the SSH
// config.
//
// Naming the destination is what lets `clipd setup debian` and `clipd setup pi`
// coexist. With one shared marker the second run stripped the first run's block
// on its way to writing its own, so only the most recent host kept a forward —
// and the earlier ones failed later with nothing to explain why.
//
// The whole destination, not just the host: `alice@server` and `bob@server`
// forward different remote paths for different accounts, and keying on the host
// alone made setting up the second silently discard the first.
func sshMarkers(destination string) (start, end string) {
	return "# >>> clipd: " + destination + " >>>", "# <<< clipd: " + destination + " <<<"
}

// maxProbeOutput bounds what setup will buffer from a remote command.
//
// Nothing setup reads from a host needs more than this, and without a cap a
// hostile or badly misconfigured server can stream until the Mac runs out of
// memory.
const maxProbeOutput = 64 << 10

// setupCommandTimeout bounds one ssh invocation.
//
// Generous, because the first one may be waiting for a person: ssh reads a
// password from /dev/tty, so the prompt is answered by the user rather than by
// anything this program controls.
const setupCommandTimeout = 3 * time.Minute

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
	os         string
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
			"matching RemoteForward to your local SSH config.\n\n"+
			"Edits ~/.ssh/config and the remote ~/.bashrc, ~/.zshrc or ~/.profile.\n"+
			"Creates first-version .clipd-backup files and private remote ~/.clipd.\n"+
			"No remote binary/service or daemon config file is created.\n"+
			"Run clipd help setup for settings, recovery and removal details.")
	printOnly := flags.Bool("print", false, "show what would be changed, without changing anything")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	if flags.NArg() != 1 {
		return failf(e, exitUsage, "setup needs exactly one ssh host, got %d", flags.NArg())
	}
	destination := flags.Arg(0)
	// Validate before the probe: ssh would read "-oProxyCommand=..." as an option.
	if _, err := sshBlockFor(destination, "/remote/socket", "/local/socket"); err != nil {
		return fail(e, exitUsage, err)
	}

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
			"setup forwards a UNIX socket, but this daemon is configured to listen on %q.\n"+
				"       Set address to a socket path to use setup, or configure the host by hand:\n"+
				"       see \"Hosts that cannot forward a socket\" in the README.",
			cfg.Address)
	}

	control, cleanup, err := controlPath()
	if err != nil {
		return fail(e, exitFailure, err)
	}
	defer cleanup()
	// The master outlives this process by ControlPersist seconds; closing it
	// here keeps a stray authenticated connection from lingering.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(closeCtx, "ssh", "-o", "ControlPath="+control, "-O", "exit", destination)
		cmd.WaitDelay = time.Second
		_ = cmd.Run()
	}()

	fmt.Fprintf(e.stdout, "Probing %s (you may be asked for your password)...\n", destination)
	facts, err := probe(ctx, destination, control)
	if err != nil {
		return fail(e, exitFailure, err)
	}

	client, err := clientCommand(facts)
	if err != nil {
		return fail(e, exitFailure, err)
	}
	// A directory of its own, not a socket loose in $HOME: the mode on the
	// directory is what protects the socket on a host that ignores a socket's
	// own permissions. installScript creates it 0700.
	remoteDir := facts.home + "/.clipd"
	remoteSocket := remoteDir + "/socket"
	rcFile := rcFileFor(facts)
	block := shellFunction(client, remoteSocket, facts.hasTar)

	sshBlock, err := sshBlockFor(destination, remoteSocket, localSocket)
	if err != nil {
		return fail(e, exitConfig, err)
	}

	fmt.Fprintf(e.stdout, "\n  remote shell   %s\n", facts.shell)
	fmt.Fprintf(e.stdout, "  remote client  %s\n", client)
	fmt.Fprintf(e.stdout, "  remote rc      %s\n", rcFile)
	fmt.Fprintf(e.stdout, "  remote socket  %s\n", remoteSocket)
	fmt.Fprintf(e.stdout, "  local socket   %s\n", localSocket)
	if !facts.hasTar {
		fmt.Fprintf(e.stdout, "\n  note: %s has no tar; named stdin drops work, but file/directory drops require tar.\n", destination)
	}
	if unsupportedShell(facts.shell) {
		fmt.Fprintf(e.stdout, "\n  note: %s does not read %s. The function is POSIX shell, so add\n"+
			"        it to that shell's own startup file by hand, or run a POSIX shell.\n",
			filepath.Base(facts.shell), rcFile)
	}

	if *printOnly {
		fmt.Fprintf(e.stdout, "\n--- would append to %s on %s ---\n%s", rcFile, destination, block)
		fmt.Fprintf(e.stdout, "\n--- would append to ~/.ssh/config ---\n%s", sshBlock)
		return exitOK
	}

	if err := installRemote(ctx, destination, control, rcFile, block); err != nil {
		return fail(e, exitFailure, err)
	}
	fmt.Fprintf(e.stdout, "\nInstalled the clipd function in %s on %s.\n", rcFile, destination)

	sshPath, changed, err := installSSHConfig(ctx, destination, sshBlock)
	if err != nil {
		// The remote edit has already happened. Saying so is the difference
		// between a user who knows the two halves disagree and one who finds
		// out later from a forward that never appears.
		return failf(e, exitFailure,
			"%v\n       The clipd function was already installed on %s; re-run setup once this is fixed.",
			err, destination)
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
  pg_dump db | clipd drop --name dump.sql

An existing SSH session will not have the forward. If you multiplex with
ControlMaster, the old master has to go first, which is what the -O exit above
is for.

One-time step on %s, if you have not done it already (needs sudo):

  echo 'StreamLocalBindUnlink yes' | sudo tee /etc/ssh/sshd_config.d/clipd.conf
  sudo systemctl reload ssh    # the service is "sshd" on some distributions

A connection that drops uncleanly (sleep, network change) leaves the remote
socket behind. Without this setting, sshd refuses every later forward with
"remote port forwarding failed" until the socket is removed by hand.
`, destination, destination, destination, destination)
	return exitOK
}

// splitDestination separates an ssh destination into its user and host.
func splitDestination(destination string) (user, host string) {
	if u, h, ok := strings.Cut(destination, "@"); ok {
		return u, h
	}
	return "", destination
}

// unsupportedShell reports whether the remote login shell will not read the
// file setup writes to.
//
// The generated function is POSIX shell, and .profile is where every POSIX
// shell looks. fish and the csh family are neither, so they read none of it —
// and reporting a successful install for a function that never loads is the
// worst of the available outcomes.
func unsupportedShell(shell string) bool {
	switch filepath.Base(shell) {
	case "fish", "csh", "tcsh":
		return true
	}
	return false
}

// sshOptions are the options every connection setup makes shares.
//
// BatchMode is deliberately absent. It was here to stop a host that wants a
// password from blocking on a prompt nothing reads, but ssh reads passwords
// from /dev/tty rather than stdin, so the prompt reaches the user perfectly
// well even though this captures stdout and stderr — and with BatchMode set,
// a host without key authentication could never be set up at all.
//
// The rest exist so setup is not three password prompts and a pile of
// warnings. One multiplexed connection carries every command, and forwarding
// is cleared because setup only runs commands: without that, an unrelated
// stale socket left by a previous session prints a "remote port forwarding
// failed" warning over setup's own output.
func sshOptions(controlPath string) []string {
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlPath,
		"-o", "ControlPersist=30",
		"-o", "ClearAllForwardings=yes",
	}
}

// controlPath returns a short, private path for setup's multiplexing socket.
//
// Short because a UNIX socket path is capped near 104 bytes, and a control
// path built from a long temp directory silently exceeds it.
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

// runRemote runs one script on the host over the shared control connection.
//
// Both streams are capped and the whole command is bounded in time, because
// everything here is what a remote host chose to send: setup has authenticated
// to it, but that is not a reason to let it decide how much memory this process
// uses or how long it runs.
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
	if facts.home == "" {
		return facts, fmt.Errorf("probe %s: the host did not report a home directory", destination)
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
	case f.os == "Darwin" && f.hasNC && f.ncUnix:
		// macOS ships its own netcat, where -N is not the half-close flag at
		// all — it takes a probe count for a write timeout, and passing it the
		// way OpenBSD's is passed fails with "invalid tcp adaptive write
		// timeout value". This netcat closes the socket on stdin EOF anyway,
		// so the flag is not needed. Checked ahead of the -N cases because the
		// flag is present here and means something else.
		return `nc -U -w 86400 "$_clipd_sock"`, nil
	case f.hasNC && f.ncUnix && f.ncShutdown:
		return `nc -N -U -w 86400 "$_clipd_sock"`, nil
	case f.hasSocat:
		return `socat -T 86400 -t 86400 - UNIX-CLIENT:"$_clipd_sock"`, nil
	case f.hasNC && f.ncUnix:
		// -U but no -N, on something that is not macOS: netcat-openbsd from
		// before the flag existed. Without a half-close the daemon never sees
		// the end of the message, so it never replies and the client waits out
		// its deadline. socat is preferred above; reaching here means there is
		// none.
		return "", errors.New("the host's netcat cannot half-close a connection (no -N flag), so a copy would hang: install a newer netcat-openbsd, or socat")
	case f.hasNC:
		return "", errors.New("the host's netcat has no -U flag, so it cannot use a UNIX socket: install netcat-openbsd (Debian/Ubuntu) or socat")
	default:
		return "", errors.New("the host has neither nc nor socat: install netcat-openbsd or socat")
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
//
// The body is a subshell so none of its scratch variables or signal traps can
// leak into the user's interactive shell. Names remain prefixed as well, which
// keeps the generated block easy to audit when read on its own.
// Progress belongs on stderr; only final status lines are captured for exit status.
const shellReplyReader = `(
  _clipd_final=
  _clipd_invalid=0
  while IFS= read -r _clipd_line; do
    case $_clipd_line in
      ('clipd: progress: '*)
        if [ -n "$_clipd_final" ]; then _clipd_invalid=1; fi
        [ "$_clipd_progress" != true ] || command printf '\r%s\033[K' "$_clipd_line" >&2 ;;
      (*)
        if [ -n "$_clipd_final" ]; then _clipd_invalid=1; fi
        _clipd_final=$_clipd_line ;;
    esac
  done
  [ ! -t 2 ] || command printf '\r\033[K' >&2
  if [ "$_clipd_invalid" = 0 ] && [ -n "$_clipd_final" ]; then
    command printf '%s\n' "$_clipd_final"
  else
    command printf 'clipd: error: invalid or missing daemon reply\n'
    exit 1
  fi
)`

func shellFunction(client, socket string, hasTar bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", blockStart)
	fmt.Fprintf(&b, "# Sends to the clipd daemon on the Mac, through the socket SSH forwards.\n")
	fmt.Fprintf(&b, "# Managed by 'clipd setup'; edits between these markers are overwritten.\n")
	// A shell expands aliases while it parses a function definition. If the
	// user already has an alias named clipd, `clipd() (` is parsed as the alias
	// expansion followed by `()`, which is a syntax error in Bash, Dash and Zsh
	// — and a syntax error here breaks every new shell on the host.
	//
	// The backslash is what makes this work everywhere. `command unalias` looks
	// right, and passes in sh and bash, but zsh resolves `command` to an
	// external program and there is no external unalias: the guard silently
	// does nothing and the definition below dies. Escaping any character of the
	// word suppresses alias expansion on `unalias` itself while still finding
	// the builtin, so it also survives a user who has aliased that too.
	//
	// `|| :` keeps this from aborting an rc file whose owner enabled errexit.
	fmt.Fprintf(&b, "\\unalias clipd 2>/dev/null || :\n")
	fmt.Fprintf(&b, "clipd() (\n")
	fmt.Fprintf(&b, "  _clipd_sock=%s\n", shellQuote(socket))
	fmt.Fprintf(&b, "  _clipd_reply=\n")
	fmt.Fprintf(&b, "  _clipd_client=0\n")
	b.WriteString(`  case ${1:-} in (-h|--help|help)
    command printf '%s\n' 'Usage: command | clipd' '       clipd drop [--no-progress] file-or-directory ...' '       command | clipd drop --name filename' '' 'Drops go to the Mac configured by clipd setup. Directories retain their structure.' 'Interactive drops show progress; --no-progress hides it.' 'On the Mac: clipd status shows limits; clipd help config explains settings.' 'After editing the Mac config, run clipd restart.'
    return 0 ;;
  esac
  _clipd_progress=false
  [ ! -t 2 ] || _clipd_progress=true
`)

	fmt.Fprintf(&b, "  if [ ! -S \"$_clipd_sock\" ]; then\n")
	fmt.Fprintf(&b, "    command printf 'clipd: %%s is missing; reconnect with the socket forward\\n' \"$_clipd_sock\" >&2\n")
	fmt.Fprintf(&b, "    return 1\n")
	fmt.Fprintf(&b, "  fi\n")
	// ${1:-} rather than $1, so that a shell running under `set -u` does not
	// abort on a plain `printf x | clipd`, which passes no arguments at all.
	fmt.Fprintf(&b, "  if [ \"${1:-}\" = \"drop\" ]; then\n")
	fmt.Fprintf(&b, "    shift\n")
	b.WriteString(`    if [ "${1:-}" = "--no-progress" ]; then _clipd_progress=false; shift; fi
`)
	{
		// Two ways to drop, told apart by an explicit flag.
		//
		// They used to be told apart by whether stdin was a terminal, which
		// made `ssh host 'clipd drop report.pdf'` — and any drop from a script,
		// a cron job or a non-interactive shell — send a header and no body,
		// creating an empty file under the right name and reporting success.
		// Reading the file is now what `clipd drop file` always means.
		fmt.Fprintf(&b, "    if [ \"${1:-}\" = \"--name\" ]; then\n")
		fmt.Fprintf(&b, "      shift\n")
		fmt.Fprintf(&b, "      if [ \"$#\" -ne 1 ] || [ -z \"${1:-}\" ]; then\n")
		fmt.Fprintf(&b, "        command printf 'clipd drop: --name needs exactly one filename\\n' >&2\n")
		fmt.Fprintf(&b, "        return 64\n")
		fmt.Fprintf(&b, "      fi\n")
		fmt.Fprintf(&b, "      _clipd_name=$1\n")
		// JSON does not permit literal control characters in a string, and the
		// daemon refuses them in filenames anyway. Reject them before framing so a
		// newline cannot turn one request envelope into several lines.
		fmt.Fprintf(&b, "      case $_clipd_name in *'\n'*)\n")
		fmt.Fprintf(&b, "        command printf 'clipd drop: the filename contains a control character\\n' >&2; return 64 ;;\n")
		fmt.Fprintf(&b, "      esac\n")
		fmt.Fprintf(&b, "      if command printf '%%s' \"$_clipd_name\" | LC_ALL=C command grep '[[:cntrl:]]' >/dev/null 2>&1; then\n")
		fmt.Fprintf(&b, "        command printf 'clipd drop: the filename contains a control character\\n' >&2\n")
		fmt.Fprintf(&b, "        return 64\n")
		fmt.Fprintf(&b, "      fi\n")
		// The name lands inside a JSON string, so the two characters JSON
		// escapes have to be escaped here. Everything else the daemon rejects.
		fmt.Fprintf(&b, "      _clipd_esc=$(command printf '%%s' \"$_clipd_name\" | command sed 's/\\\\/\\\\\\\\/g; s/\"/\\\\\"/g') || return 1\n")
		fmt.Fprintf(&b, "      if _clipd_reply=$( { command printf 'clipd:magic:v1\\n{\"type\":\"drop\",\"name\":\"%%s\",\"progress\":%%s}\\n' \"$_clipd_esc\" \"$_clipd_progress\"; command cat; } | command %s | ", client)
		b.WriteString(shellReplyReader)
		b.WriteString(" ); then :; else _clipd_client=$?; fi\n")
		fmt.Fprintf(&b, "    else\n")
		fmt.Fprintf(&b, "      if [ $# -eq 0 ]; then\n")
		fmt.Fprintf(&b, "        command printf 'clipd drop: no files given (use --name to send stdin)\\n' >&2\n")
		fmt.Fprintf(&b, "        return 64\n")
		fmt.Fprintf(&b, "      fi\n")
		if hasTar {
			// Each operand is reduced to an absolute -C directory and a ./basename.
			// Prefixing operands prevents option injection while allowing multiple
			// tar -C directives (a global -- would turn those into filenames).
			b.WriteString(`      _clipd_reply=$(
        _clipd_first=1
        for _clipd_path in "$@"; do
          case $_clipd_path in (*'
'*) command printf 'clipd drop: path contains a control character\n' >&2; exit 3 ;; esac
          if command printf '%s' "$_clipd_path" | LC_ALL=C command grep '[[:cntrl:]]' >/dev/null 2>&1; then
            command printf 'clipd drop: path contains a control character\n' >&2; exit 3
          fi
          while [ "${_clipd_path%/}" != "$_clipd_path" ]; do _clipd_path=${_clipd_path%/}; done
          _clipd_base=${_clipd_path##*/}
          case $_clipd_base in (''|.|..) command printf 'clipd drop: give a file or directory name, not a filesystem root\n' >&2; exit 3 ;; esac
          case $_clipd_path in (*/*) _clipd_parent=${_clipd_path%/*}; [ -n "$_clipd_parent" ] || _clipd_parent=/ ;; (*) _clipd_parent=. ;; esac
          case $_clipd_parent in (/*|./*|../*|.) ;; (*) _clipd_parent=./$_clipd_parent ;; esac
          _clipd_parent=$(command -p sh -c 'CDPATH=; command cd "$1" && command pwd -P' sh "$_clipd_parent") || exit 3
          if [ "$_clipd_first" = 1 ]; then set --; _clipd_first=0; fi
          set -- "$@" -C "$_clipd_parent" "./$_clipd_base"
        done
        {
          command printf 'clipd:magic:v1\n{"type":"drop-stream-v2","progress":%s}\n' "$_clipd_progress"
          if COPYFILE_DISABLE=1 command tar cf - "$@"; then
            command printf 'clipd:complete:v2\n'
          else
            command printf 'clipd drop: tar failed; the drop was not completed\n' >&2
          fi
        } | `)
			fmt.Fprintf(&b, "command %s | ", client)
			b.WriteString(shellReplyReader)
			b.WriteString("\n      ) || _clipd_client=$?\n")
		} else {
			fmt.Fprintf(&b, "      command printf 'clipd drop: this host has no tar\\n' >&2; return 1\n")
		}
		fmt.Fprintf(&b, "    fi\n")
	}
	fmt.Fprintf(&b, "  else\n")
	fmt.Fprintf(&b, "    if _clipd_reply=$(command %s | ", client)
	b.WriteString(shellReplyReader)
	b.WriteString(" ); then :; else _clipd_client=$?; fi\n")
	fmt.Fprintf(&b, "  fi\n")
	// The daemon's reply is the whole user interface for the result, so it is
	// printed whatever it says; the status prefix on it is what turns the
	// result into an exit code.
	fmt.Fprintf(&b, "  if [ -n \"$_clipd_reply\" ]; then command printf '%%s\\n' \"$_clipd_reply\"; fi\n")
	// Failure to answer is not proof that a socket is stale. Do not probe with
	// an unbounded shell transport or recommend deleting an unidentified listener.
	fmt.Fprintf(&b, "  if [ \"$_clipd_client\" -ne 0 ] || [ -z \"$_clipd_reply\" ]; then\n")
	fmt.Fprintf(&b, "    if [ \"$_clipd_client\" -ne 3 ]; then\n")
	fmt.Fprintf(&b, "      command printf 'clipd: transfer failed or no reply arrived; check clipd status on the Mac and the SSH forwarding warning.\\n' >&2\n")
	fmt.Fprintf(&b, "    fi\n")
	fmt.Fprintf(&b, "    return 1\n  fi\n")
	fmt.Fprintf(&b, "  case $_clipd_reply in\n")
	fmt.Fprintf(&b, "    'clipd: ok: '*) return 0 ;;\n")
	fmt.Fprintf(&b, "    *) return 1 ;;\n")
	fmt.Fprintf(&b, "  esac\n")
	fmt.Fprintf(&b, ")\n")
	fmt.Fprintf(&b, "%s\n", blockEnd)
	return b.String()
}

// shellQuote renders one literal POSIX shell word.
//
// Double quotes are insufficient here: a remote home directory containing a
// dollar sign or backtick would still perform expansion when the generated rc
// file is sourced. A single quote is represented by ending the quoted word,
// writing a quoted quote, and starting it again.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'"
}

// installScript rewrites the managed block in a remote rc file.
//
// The awk pass drops any previous block before the new one is appended, so
// running setup twice leaves one function rather than two. Writing through
// `cat >` rather than `mv` keeps the rc file's existing inode and permissions:
// the staging file is created 0600, and silently tightening someone's .bashrc
// is not setup's business.
//
// The awk pass validates marker order while it writes only to the temporary
// file. A malformed rc file is therefore rejected before its contents or its
// backup are changed.
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
# A socket directly in $HOME is clipd's own leftover, from before the directory.
if [ -S "$HOME/.clipd.sock" ]; then rm -f "$HOME/.clipd.sock"; fi

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

// installRemote writes the shell function into the host's rc file.
func installRemote(ctx context.Context, destination, control, rcFile, block string) error {
	script := fmt.Sprintf(installScript, rcFile, strings.TrimRight(block, "\n"))
	if _, err := runRemote(ctx, destination, control, script); err != nil {
		return fmt.Errorf("install the clipd function on %s: %s", destination, err)
	}
	return nil
}

// sshQuote renders a value as one quoted ssh_config argument.
//
// Quoted because a socket path may contain a space, and an unquoted one splits
// into the wrong number of arguments — a RemoteForward that ssh then rejects.
// The doubled %% is a separate problem: ssh expands %h, %u and the rest inside
// these values whatever the quoting, so a literal one has to be escaped or a
// home directory containing it is silently rewritten.
func sshQuote(v string) string {
	return `"` + strings.ReplaceAll(v, "%", "%%") + `"`
}

// validSSHValue rejects a value that cannot be encoded as an ssh_config
// argument at all.
//
// Quoting handles spaces. Nothing handles an embedded quote or a newline: the
// first ends the argument early and the second starts a new directive, so a
// path containing either could turn into configuration the user never wrote.
func validSSHValue(what, v string) error {
	if v == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if i := strings.IndexAny(v, "\"\\\n\r"); i >= 0 {
		return fmt.Errorf("%s %q contains %q, which an ssh config cannot carry", what, v, string(v[i]))
	}
	return nil
}

// validHostPattern rejects a destination that would widen the block beyond the
// host it was written for.
//
// `clipd setup '*'` is the case that matters: it writes a stanza matching every
// host, so the forward is attempted on every connection the user ever makes.
func validHostPattern(host string) error {
	if host == "" {
		return errors.New("the ssh destination has no host")
	}
	if strings.HasPrefix(host, "-") {
		return errors.New("the ssh host cannot start with a dash")
	}
	if i := strings.IndexAny(host, "*?!/: \t"); i >= 0 {
		return fmt.Errorf("ssh destination %q contains %q; setup writes a block for one named host, so give it one",
			host, string(host[i]))
	}
	return validSSHValue("ssh destination", host)
}

// sshBlockFor renders the stanza carrying one destination's forward.
//
// A destination that names a user gets a Match block rather than a Host block.
// ssh strips the user before matching Host patterns, so `Host server` applies to
// every account on that machine: `setup bob@server` after `setup alice@server`
// pointed alice's connections at bob's home directory, where the forward fails
// with a permission error and nothing to explain it.
func sshBlockFor(destination, remoteSocket, localSocket string) (string, error) {
	if strings.HasPrefix(destination, "-") {
		return "", errors.New("the ssh destination cannot start with a dash")
	}
	user, host := splitDestination(destination)
	if err := validHostPattern(host); err != nil {
		return "", err
	}
	if user != "" {
		if err := validSSHValue("ssh user", user); err != nil {
			return "", err
		}
	}
	if err := validSSHValue("remote socket path", remoteSocket); err != nil {
		return "", err
	}
	if err := validSSHValue("local socket path", localSocket); err != nil {
		return "", err
	}

	start, end := sshMarkers(destination)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", start)
	if user != "" {
		// originalhost, not host: `Match host` is evaluated against the name
		// after HostName substitution, so for an alias like
		//
		//	Host myserver
		//	  HostName real.example.com
		//
		// `Match host "myserver"` never fires — ssh is by then looking at
		// real.example.com. originalhost matches the name as typed, which is
		// the one setup was given. The failure was silent: a block that reads
		// correctly, matches nothing, and leaves the forward simply not
		// happening. Verified with `ssh -G`.
		fmt.Fprintf(&b, "Match originalhost %s user %s\n", sshQuote(host), sshQuote(user))
	} else {
		// A Host pattern already matches the name as typed, so the bare
		// destination needs no equivalent.
		fmt.Fprintf(&b, "Host %s\n", sshQuote(host))
	}
	fmt.Fprintf(&b, "  RemoteForward %s %s\n", sshQuote(remoteSocket), sshQuote(localSocket))
	// OpenSSH's default mask already yields a 0600 socket, but a
	// StreamLocalBindMask the user set elsewhere in this file would apply here
	// too, and 0000 there publishes the socket to every account on the host.
	fmt.Fprintf(&b, "  StreamLocalBindMask 0177\n")
	// StreamLocalBindUnlink is deliberately absent, and should not be added back.
	// It reads like the fix for a socket left behind by a session that died
	// uncleanly, and it is — for a socket the ssh client binds, which is local
	// forwarding. A remote forward is bound by the remote's sshd, and the
	// streamlocal-forward request carries only a path, so the client has no way
	// to ask for an unlink. Measured against a real host: the option present and
	// active in `ssh -G`, a stale socket in place, and the forward still failed.
	// The setting that governs this lives in the remote's sshd_config.
	// Host and Match sections continue until the next Host or Match directive;
	// the end marker is only a comment. Reset to an all-host section so a user
	// who later appends a global option does not silently scope it to clipd's
	// destination (or, for Match, to one destination/account pair).
	fmt.Fprintf(&b, "Host *\n")
	fmt.Fprintf(&b, "%s\n", end)
	return b.String(), nil
}

// installSSHConfig adds the socket forward to the user's SSH config, replacing
// any block a previous run left.
//
// The block is a stanza of its own rather than an edit to one the user already
// wrote. SSH accumulates RemoteForward across matching blocks, so this adds the
// forward without touching, reordering, or having to parse whatever else is in
// the file.
func installSSHConfig(ctx context.Context, destination, block string) (path string, changed bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("locate home directory: %w", err)
	}
	dir := filepath.Join(home, ".ssh")
	path = filepath.Join(dir, "config")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return path, false, fmt.Errorf("create %s: %w", dir, err)
	}
	writePath, err := resolvedWritePath(path)
	if err != nil {
		return path, false, err
	}

	existing, err := os.ReadFile(writePath)
	if err != nil && !os.IsNotExist(err) {
		return path, false, fmt.Errorf("read %s: %w", path, err)
	}

	start, end := sshMarkers(destination)
	stripped, err := stripBlock(string(existing), start, end)
	if err != nil {
		return path, false, fmt.Errorf("%s: %w", path, err)
	}
	// Also drop a block from before the markers carried a destination. There
	// can only be one, it names some host this run may not be for, and leaving
	// it would forward the same socket twice — which fails at connect time with
	// "remote port forwarding failed" and no clue as to the cause.
	stripped, err = stripBlock(stripped, blockStart, blockEnd)
	if err != nil {
		return path, false, fmt.Errorf("%s: %w", path, err)
	}

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
	if err := verifySSHConfig(ctx, updated, destination); err != nil {
		return path, false, err
	}
	// A backup before the first edit, because this file is often hand-tuned
	// and losing it is a bad afternoon. Only the first: later runs would
	// overwrite it with a copy that already contains a clipd block, which is
	// not the file the user wants back.
	backup := path + ".clipd-backup"
	if len(existing) > 0 {
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(backup, existing, 0o600); err != nil {
				return path, false, fmt.Errorf("back up %s: %w", path, err)
			}
		}
	}
	if err := writeFileAtomic(writePath, []byte(updated), 0o600); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// resolvedWritePath preserves a config symlink while retaining atomic writes.
//
// Replacing ~/.ssh/config itself would replace the symlink, which is a common
// way to manage dotfiles. Resolve an existing link once and atomically replace
// its regular-file target instead. A dangling link is refused: guessing where
// to create its target could write outside the user's intended location.
func resolvedWritePath(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("refusing to replace %s: it is not a regular file", path)
		}
		return path, nil
	}

	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("inspect target of %s: %w", path, err)
	}
	if !targetInfo.Mode().IsRegular() {
		return "", fmt.Errorf("refusing to replace %s: its target %s is not a regular file", path, target)
	}
	return target, nil
}

// verifySSHConfig asks ssh to parse a candidate file before it replaces the
// real one.
//
// `ssh -G` resolves a destination against a config and exits non-zero if it
// cannot read it. Checking here turns a quoting mistake into a refusal to
// write, instead of an ~/.ssh/config that ssh rejects wholesale — which would
// break every host the user has, not only the one being set up.
//
// A check that cannot be run is not a failure. If ssh is missing or a temporary
// file cannot be made, the install proceeds unverified rather than blocking on
// the absence of a second opinion.
func verifySSHConfig(ctx context.Context, content, destination string) error {
	f, err := os.CreateTemp("", "clipd-sshcheck-*")
	if err != nil {
		return nil
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return nil
	}
	if err := f.Close(); err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-G", "-F", f.Name(), destination)
	stderr := &capWriter{max: maxProbeOutput}
	cmd.Stderr = stderr
	cmd.Stdout = &capWriter{max: maxProbeOutput}
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("the generated SSH configuration would not parse: %s", msg)
	}
	return nil
}

// writeFileAtomic replaces a file's contents in one step.
//
// A plain write truncates first, so an interruption between the truncate and
// the write leaves an empty ~/.ssh/config — not a file to be casual with. The
// temporary is made in the same directory, so the rename cannot cross a
// filesystem boundary and fall back to a copy.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".clipd-*")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmp := f.Name()
	// A no-op once the rename below has succeeded.
	defer os.Remove(tmp)

	if err := f.Chmod(perm); err != nil {
		f.Close()
		return fmt.Errorf("restrict %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// stripBlock removes a previously managed block, markers included.
//
// Exactly zero or one complete block is accepted. Reversed, nested, duplicate,
// or unmatched markers mean the file was edited by hand or corrupted; refusing
// it is safer than guessing which surrounding lines belong to the user.
func stripBlock(s, start, end string) (string, error) {
	var out []string
	inside := false
	seen := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == start {
			if inside || seen {
				return "", fmt.Errorf("found duplicate or nested %q; fix the clipd block by hand", start)
			}
			inside = true
			continue
		}
		if trimmed == end {
			if !inside {
				return "", fmt.Errorf("found %q before a matching %q; fix the clipd block by hand", end, start)
			}
			inside = false
			seen = true
			continue
		}
		if !inside {
			out = append(out, line)
		}
	}
	if inside {
		return "", fmt.Errorf("found %q with no matching %q; fix the clipd block by hand", start, end)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n"), nil
}
