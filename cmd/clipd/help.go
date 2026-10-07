package main

var helpTopics = map[string]string{
	"serve": `clipd serve

Runs the daemon in the foreground until SIGINT or SIGTERM. Requires pbcopy on
macOS, or a supported clipboard helper in a Linux graphical session.

It does not detach: under the LaunchAgent, launchd owns backgrounding,
restarts and log files.

Options:
  -address <path>     socket path to listen on (default ~/.clipd.sock)
  -drop-dir <path>    where dropped files land (default ~/Drop)`,

	"setup": `clipd setup [options] <ssh-host>

Run on the Mac. Connects to the host, checks which nc or socat it has, and
writes two things:

  Remote: a clipd shell function in ~/.bashrc, ~/.zshrc or ~/.profile, and a
          private ~/.clipd directory for the socket. No program is installed.
  Mac:    a block in ~/.ssh/config that forwards the remote socket to the
          daemon's socket.

Both are between clipd markers, so re-running setup replaces them, leaving the
rest of each file alone. The first run saves a .clipd-backup of each file.
The host may be an SSH alias, hostname, IP or user@host; the block matches it
exactly as typed, so set up each name you use.

Reconnect afterwards for the forward to take effect.

sshd leaves the remote socket behind when you log out, which makes the next
login's forward fail. clipd removes the leftover the first time you use it and
asks you to reconnect; re-running setup also clears it. On a server you run,
setup prints a one-line sshd change that stops it happening at all.

Options:
  -print    show what would be written, without changing anything

To undo, delete the blocks between the clipd markers. See the README for
details and troubleshooting.`,

	"drop": `clipd drop [--no-progress] <file-or-folder>...
<command> | clipd drop --name <filename>

Run on the remote host. Sends files and folders to the Mac's ~/Drop.

  clipd drop report.pdf                       ~/Drop/report.pdf
  clipd drop photos                           ~/Drop/photos/, structure kept
  clipd drop photos/*                         photos' contents, into ~/Drop
  journalctl -u nginx | clipd drop --name nginx.log

Nothing is overwritten: a second report.pdf arrives as report-1.pdf. A drop is
all or nothing, and the exit status is the Mac's answer, so
clipd drop x && rm x only removes x once it has arrived. Symlinks and other
special files are skipped, never followed, and the reply counts them.

Progress shows in a terminal; --no-progress hides it.

Default limits: 256 MiB and 256 files per drop, 30 minutes per transfer. An
error names the limit you hit; see clipd help config on the Mac to raise it.

After updating clipd on the Mac, rerun clipd setup to update this function.
On Debian, ssh host 'clipd ...' needs ssh -t: its .bashrc skips non-interactive
shells before reaching the function.`,

	"config": `clipd config file

~/.config/clipd/config.json ($XDG_CONFIG_HOME/clipd/config.json when set), or
the path given by -config or CLIPD_CONFIG. Optional: every setting has a
default. clipd install writes one with every setting filled in.

  address                ~/.clipd.sock   socket, or loopback host:port
  drop_dir               ~/Drop          where drops land
  max_payload_bytes      10485760        clipboard, 10 MiB; up to 1 GiB
  max_drop_bytes         268435456       per drop, 256 MiB; up to 1 TiB
  max_drop_files         256             per drop; up to 65536
  max_concurrent         8               at once; up to 64
  max_transfer_seconds   1800            per transfer, 30 min; up to 86400

Sizes are bytes, times are seconds: 10 GiB = 10737418240, one hour = 3600.
A missing setting or 0 means the default, never unlimited. Unknown settings
are rejected. max_payload_bytes times max_concurrent must fit in 2 GiB.

The daemon reads this file only at startup: run clipd restart after editing.
clipd status shows the limits in use and notices unapplied edits.
Config files from before v3 are not migrated.`,

	"security": `clipd security model

clipd has no authentication or encryption of its own, by design. The daemon
listens on a UNIX socket only you can use. It reaches a remote host only through
your SSH connection, which encrypts it, checks the host key and authenticates
you. On the remote, the socket sits in your private ~/.clipd directory.

So anything running as you on that host can write to your clipboard and send
you files. Other accounts there cannot. Letting a remote machine write to your
clipboard means trusting what runs there as you; anything able to abuse that
already has your files on that machine.

What limits the damage: bracketed paste in modern shells stops pasted text from
running before you press Enter; drops stay inside ~/Drop, never overwrite and
never arrive executable; and sizes, file counts, connections and transfer time
are all bounded.

The address setting also accepts a loopback host:port, for hosts whose SSH
cannot forward a socket. Every account on a machine can reach loopback, so it is
weaker than the socket; anything reachable from the network is refused.

SECURITY.md in the repository lists the exact guarantees and their exceptions.`,

	"install": `clipd install

macOS only. Writes ~/Library/LaunchAgents/com.clipd.agent.plist and loads it
into your GUI session, so the daemon starts at login and restarts if it
crashes. No root privileges are required.

Options:
  -exec <path>   binary path to record in the plist (default: this binary)`,

	"restart": `clipd restart

macOS only. Stops and starts the LaunchAgent.

The config file is read once, when the daemon starts, so editing it has no
effect until this is run. 'clipd status' says when the file has been edited
since the daemon last started.

Equivalent to:

  launchctl kickstart -k gui/$(id -u)/com.clipd.agent`,

	"uninstall": `clipd uninstall

macOS only. Unloads the LaunchAgent and removes its plist. The config file and
log directory are left in place, as are the clipd shell functions on any remote
hosts — delete the block between the clipd markers in their rc files.`,

	"status": `clipd status

Shows the resolved configuration and whether the daemon is actually listening.

It dials the socket rather than just checking that the file exists, because a
daemon killed with SIGKILL leaves the socket file behind. A stale socket and a
live one look identical in a directory listing.

The exit code carries the verdict — 0 when the daemon answers, 1 when it does
not — so 'clipd status && ...' works as a preflight.`,

	"version": `clipd version

Prints the version, commit and build date recorded at build time, plus the Go
toolchain and target platform.`,

	"help": `clipd help [topic]

With no topic, prints the command list. Topics: serve, setup, drop, config,
security, install, restart, uninstall, status, version.`,
}
