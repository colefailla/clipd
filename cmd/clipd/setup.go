package main

import (
	"context"
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

// sshMarkers keys local blocks by the destination spelling supplied to setup.
func sshMarkers(destination string) (start, end string) {
	return "# >>> clipd: " + destination + " >>>", "# <<< clipd: " + destination + " <<<"
}

// cmdSetup configures a remote host to talk to this daemon.
func cmdSetup(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "setup", "Usage: clipd setup [options] <host>")
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

	fmt.Fprintf(e.stdout, "Checking %s... SSH may ask for a password.\n", destination)
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

	fmt.Fprintf(e.stdout, "\n  account        %s\n", facts.user)
	fmt.Fprintf(e.stdout, "  shell          %s\n", facts.shell)
	fmt.Fprintf(e.stdout, "  transport      %s\n", strings.Fields(client.send)[0])
	fmt.Fprintf(e.stdout, "  remote rc      %s\n", strings.Replace(rcFile, "$HOME", facts.home, 1))
	fmt.Fprintf(e.stdout, "  remote socket  %s\n", remoteSocket)
	fmt.Fprintf(e.stdout, "  local socket   %s\n", localSocket)
	if home, err := os.UserHomeDir(); err == nil {
		fmt.Fprintf(e.stdout, "  SSH config     %s\n", filepath.Join(home, ".ssh", "config"))
	}
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
		fmt.Fprintf(e.stdout, "\n--- proposed block for %s on %s ---\n%s", rcFile, destination, block)
		fmt.Fprintf(e.stdout, "\n--- proposed block for ~/.ssh/config ---\n%s", sshBlock)
		return exitOK
	}

	removed, err := installRemote(ctx, destination, control, rcFile, client, block)
	if err != nil {
		return fail(e, exitFailure, err)
	}
	if removed {
		fmt.Fprintf(e.stdout, "\nRemoved the stale remote socket.\n")
	}

	_, changed, err := installSSHConfig(ctx, destination, sshBlock)
	if err != nil {
		// Setup cannot roll back the remote edit when the local update fails.
		return failf(e, exitFailure,
			"%v\n\nThe remote function was updated on %s.\nFix the local SSH config error, then rerun:\n  clipd setup %s",
			err, destination, destination)
	}
	if changed {
		fmt.Fprintf(e.stdout, "\nUpdated the remote function and local SSH config.\n")
	} else {
		fmt.Fprintf(e.stdout, "\nUpdated the remote function.\nLocal SSH config is already current.\n")
	}

	fmt.Fprintf(e.stdout, `
Reconnect:
  ssh %s

If you use connection sharing, first run:
  ssh -O exit %s

This closes the shared connection and its sessions.
`, destination, destination)
	return exitOK
}

// splitDestination separates an ssh destination into its user and host.
func splitDestination(destination string) (user, host string) {
	if u, h, ok := strings.Cut(destination, "@"); ok {
		return u, h
	}
	return "", destination
}
