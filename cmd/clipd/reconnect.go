package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/colefailla/clipd/internal/server"
)

func cmdReconnect(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "reconnect", "Usage: clipd reconnect [options] <ssh-host>\n\nRestore the shared SSH forward, then return to your Mac prompt. Run setup first.")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	if flags.NArg() != 1 {
		return failf(e, exitUsage, "reconnect needs exactly one ssh host")
	}
	destination := flags.Arg(0)
	// Reuse setup's destination validation before passing it to ssh.
	if _, err := sshBlockFor(destination, "/remote/socket", "/local/socket"); err != nil {
		return fail(e, exitUsage, err)
	}
	cfg, _, err := loadConfig(g)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	if !server.IsSocketPath(cfg.Address) {
		return failf(e, exitConfig, "reconnect requires a UNIX-socket daemon")
	}
	local, err := server.ExpandPath(cfg.Address)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	if err := server.Ping("unix", local); err != nil {
		return failf(e, exitFailure, "Mac daemon is unavailable: %v; check clipd status", err)
	}
	control, cleanup, err := controlPath()
	if err != nil {
		return fail(e, exitFailure, err)
	}
	defer cleanup()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(closeCtx, "ssh", "-F", "/dev/null", "-S", control, "-O", "exit", destination)
		cmd.WaitDelay = time.Second
		_ = cmd.Run()
	}()
	fmt.Fprintf(e.stdout, "Checking %s (you may be asked for your password)...\n", destination)
	facts, err := probe(ctx, destination, control)
	if err != nil {
		return fail(e, exitFailure, err)
	}
	remote := facts.home + "/.clipd/socket"
	if !strings.HasPrefix(facts.home, "/") {
		return failf(e, exitFailure, "remote home is not an absolute path")
	}
	for _, path := range []string{remote, local} {
		if err := validSSHValue("socket path", path); err != nil {
			return fail(e, exitConfig, err)
		}
		if strings.ContainsAny(path, ":\t") {
			return failf(e, exitConfig, "reconnect cannot forward a socket path containing a colon or tab")
		}
	}
	shared, err := reconnectControlPath(ctx, destination, remote, local)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
	check := exec.CommandContext(checkCtx, "ssh", "-F", "/dev/null", "-S", shared, "-O", "check", destination)
	check.WaitDelay = time.Second
	check.Stdout, check.Stderr = &capWriter{max: maxProbeOutput}, &capWriter{max: maxProbeOutput}
	running := check.Run() == nil
	checkCancel()
	if running {
		fmt.Fprintln(e.stdout, "Resetting the shared SSH connection; shells using it will disconnect.")
		stopCtx, stopCancel := context.WithTimeout(ctx, 5*time.Second)
		stop := exec.CommandContext(stopCtx, "ssh", "-F", "/dev/null", "-S", shared, "-O", "exit", destination)
		stop.WaitDelay = time.Second
		stop.Stdout, stop.Stderr = &capWriter{max: maxProbeOutput}, &capWriter{max: maxProbeOutput}
		stopErr := stop.Run()
		stopCancel()
		if stopErr != nil {
			return failf(e, exitFailure, "could not stop the shared SSH connection: %v", stopErr)
		}
	}
	result, err := runRemote(ctx, destination, control, reconnectScript(remote))
	if err != nil {
		return failf(e, exitFailure, "remote socket recovery failed: %v", err)
	}
	switch strings.TrimSpace(result) {
	case "missing":
		fmt.Fprintln(e.stdout, "Remote socket is clear.")
	case "removed":
		fmt.Fprintln(e.stdout, "Removed stale remote socket.")
	case "active":
		return failf(e, exitFailure, "remote socket already accepts connections; existing listener was preserved")
	default:
		return failf(e, exitFailure, "unexpected remote recovery response")
	}
	startCtx, startCancel := context.WithTimeout(ctx, setupCommandTimeout)
	defer startCancel()
	start := exec.CommandContext(startCtx, "ssh", "-f", "-N", "-o", "ExitOnForwardFailure=yes", destination)
	start.Stdout, start.Stderr = e.stdout, e.stderr
	start.WaitDelay = time.Second
	if err := start.Run(); err != nil {
		return failf(e, exitFailure, "could not restore SSH forwarding: %v", err)
	}
	fmt.Fprintf(e.stdout, "Forward restored. Connect normally: ssh %s\n", destination)
	return exitOK
}

// Only an explicit refused connection proves that a socket is stale. A ping
// keeps a successful connection from being mistaken for empty clipboard data.
func reconnectScript(socket string) string {
	return "_clipd_sock=" + shellQuote(socket) + "\n" + `
export LC_ALL=C
_clipd_dir=${_clipd_sock%/*}
_clipd_fail() { printf '%s\n' "$1" >&2; exit 1; }
[ -d "$_clipd_dir" ] && [ ! -L "$_clipd_dir" ] || _clipd_fail 'Run clipd setup first; the remote private directory is missing or a symlink.'
case $(ls -ld "$_clipd_dir") in
  drwx------*) ;;
  *) _clipd_fail 'Remote .clipd directory must be private (0700).' ;;
esac
[ ! -L "$_clipd_sock" ] || _clipd_fail 'Refusing to remove a symlink at the socket path.'
if [ ! -e "$_clipd_sock" ]; then printf 'missing\n'; exit 0; fi
[ -S "$_clipd_sock" ] || _clipd_fail 'Refusing to remove a non-socket file.'
command -v nc >/dev/null 2>&1 || _clipd_fail 'Safe stale-socket recovery requires OpenBSD nc; socket was preserved.'
_clipd_identity=$(ls -di "$_clipd_sock") || exit 1
case $(uname -s) in
  Darwin) set -- nc -U -w 2 ;;
  *) set -- nc -U -N -w 2 ;;
esac
if _clipd_error=$(printf 'clipd:magic:v1\n{"type":"ping"}\n' | "$@" "$_clipd_sock" 2>&1 >/dev/null); then
  printf 'active\n'; exit 0
fi
case $_clipd_error in
  "nc: $_clipd_sock: Connection refused"|"nc: connectx to $_clipd_sock failed: Connection refused") ;;
  *) _clipd_fail 'Transport could not confirm a refused connection; socket was preserved.' ;;
esac
[ ! -L "$_clipd_sock" ] && [ -S "$_clipd_sock" ] && [ "$(ls -di "$_clipd_sock")" = "$_clipd_identity" ] || _clipd_fail 'Socket changed during inspection; socket was preserved.'
rm -- "$_clipd_sock" || exit 1
printf 'removed\n'
`
}

// ssh -G checks the destination as typed, including aliases and account scope.
// It cannot prove server policy; the next ordinary SSH connection binds the socket.
func verifyReconnectForward(ctx context.Context, destination, remote, local string) error {
	_, err := reconnectControlPath(ctx, destination, remote, local)
	return err
}

func reconnectControlPath(ctx context.Context, destination, remote, local string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, setupCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-G", destination)
	cmd.WaitDelay = time.Second
	out, errOut := &capWriter{max: maxProbeOutput}, &capWriter{max: maxProbeOutput}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cannot inspect SSH forwarding config: %s (%w)", strings.TrimSpace(errOut.String()), err)
	}
	expected := "remoteforward " + remote + " " + local
	found, control, master, persist := false, "", "", ""
	for _, line := range strings.Split(out.String(), "\n") {
		if line == expected {
			found = true
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "controlpath":
			control = value
		case "controlmaster":
			master = value
		case "controlpersist":
			persist = value
		}
	}
	if !found {
		return "", fmt.Errorf("SSH destination %q has no matching clipd forward; run clipd setup %s first", destination, destination)
	}
	name := filepath.Base(control)
	hash := strings.TrimPrefix(name, "clipd-")
	// OpenSSH appends a dot and sixteen characters while publishing the master.
	if master != "auto" || persist != "yes" || !filepath.IsAbs(control) || filepath.Base(filepath.Dir(control)) != ".ssh" || !strings.HasPrefix(name, "clipd-") || len(hash) != 40 || strings.Trim(hash, "0123456789abcdef") != "" || len(control)+17 >= 104 {
		return "", fmt.Errorf("SSH destination %q is not using clipd's persistent connection settings; run clipd setup %s and check for earlier SSH overrides", destination, destination)
	}
	return control, nil
}
