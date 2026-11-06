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

// sshMarkers keys local blocks by the destination spelling supplied to setup.
func sshMarkers(destination string) (start, end string) {
	return "# >>> clipd: " + destination + " >>>", "# <<< clipd: " + destination + " <<<"
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
	if err := validateDestination(destination); err != nil {
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

	fmt.Fprintf(e.stdout, "Checking %s (SSH may ask for a password)...\n", destination)
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

	sshBlock, err := sshBlockFor(destination, facts.user, remoteSocket, localSocket)
	if err != nil {
		return fail(e, exitConfig, err)
	}

	fmt.Fprintf(e.stdout, "\n  remote shell   %s\n", facts.shell)
	fmt.Fprintf(e.stdout, "  remote client  %s\n", client.send)
	fmt.Fprintf(e.stdout, "  remote rc      %s\n", rcFile)
	fmt.Fprintf(e.stdout, "  remote socket  %s\n", remoteSocket)
	fmt.Fprintf(e.stdout, "  local socket   %s\n", localSocket)
	if !facts.hasTar {
		fmt.Fprintf(e.stdout, "\n  note: %s has no tar. Clipboard copies still work.\n"+
			"        To send files and folders with 'clipd drop', install tar.\n"+
			"        You can also send command output with 'command | clipd drop --name filename'.\n", destination)
	}
	if unsupportedShell(facts.shell) {
		fmt.Fprintf(e.stdout, "\n  note: clipd's function doesn't work in %s, your login shell on %s.\n"+
			"        To use clipd there, run sh, then: . %s\n",
			filepath.Base(facts.shell), destination, rcFile)
	}

	if *printOnly {
		fmt.Fprintf(e.stdout, "\n--- would append to %s on %s ---\n%s", rcFile, destination, block)
		fmt.Fprintf(e.stdout, "\n--- would append to ~/.ssh/config ---\n%s", sshBlock)
		return exitOK
	}

	removed, err := installRemote(ctx, destination, control, rcFile, client, block)
	if err != nil {
		return fail(e, exitFailure, err)
	}
	fmt.Fprintf(e.stdout, "\nInstalled the clipd function in %s on %s.\n", rcFile, destination)
	if removed {
		fmt.Fprintf(e.stdout, "Removed a socket an earlier SSH session left behind on %s.\n", destination)
	}

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
Setup complete. Reconnect to use clipd:

  ssh -O exit %s 2>/dev/null; ssh %s

If you use SSH connection sharing, this also closes sessions using that connection.

Then try these on %s:

  ls -l | clipd
  clipd notes.txt
  clipd drop report.pdf

If clipd removes a leftover socket, log out and reconnect.
See "Leftover sockets" in the README for more details.
`, destination, destination, destination)
	return exitOK
}

// splitDestination separates an ssh destination into its user and host.
func splitDestination(destination string) (user, host string) {
	if u, h, ok := strings.Cut(destination, "@"); ok {
		return u, h
	}
	return "", destination
}
