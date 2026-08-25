package main

var helpTopics = map[string]string{
	"serve": `clipd serve

Runs the daemon in the foreground until SIGINT or SIGTERM. macOS only, since
writing the clipboard is what the daemon does.

It does not detach: under the LaunchAgent, launchd owns backgrounding,
restarts and log files.

Options:
  -address <path>     socket path to listen on (default ~/.clipd.sock)
  -drop-dir <path>    where dropped files land (default ~/Drop)`,

	"setup": `clipd setup <ssh-host>

Configures a remote host to talk to this daemon. Run it on the Mac.

It connects to the host, works out what that host actually has, installs a
'clipd' shell function there, and adds the matching RemoteForward to your
~/.ssh/config.

Nothing is installed on the remote machine. The function is a few lines of
shell that pipe into netcat (or socat), and the socket it writes to is created
by SSH when you connect.

The probe exists because netcat is not one program. The OpenBSD build speaks
UNIX sockets with -U and half-closes with -N; the "traditional" build on some
Debian systems does neither, and guessing wrong gives you either "invalid
option" or a copy that hangs with no output. Asking the host removes the
guess.

Options:
  -print    show what would change, without changing anything

Both edits are bracketed by clipd markers, so running setup again replaces the
block rather than adding a second one, and removing clipd means deleting from
one marker to the other. Your SSH config is backed up to config.clipd-backup
before the first edit.`,

	"drop": `clipd drop <file>...

Run on the remote host, not the Mac. Sends files to the Mac's drop directory
(default ~/Drop).

  clipd drop report.pdf
  clipd drop src/*.go

The files are packed with tar, so names, multiple files and whole directories
survive the trip. What arrives is deliberately flattened: every file lands
directly in the drop directory under its own basename, with no subdirectories
recreated.

That flattening is a security property, not a limitation. An archive can name
a file "../../.ssh/authorized_keys"; reducing every entry to its basename
makes that impossible to act on. Symlinks, hard links and device nodes in the
archive are skipped for the same reason, the executable bit is never
preserved, and on macOS each file is marked with the quarantine attribute so
Gatekeeper treats it like a download.

Files are never overwritten. A second report.pdf arrives as report-1.pdf.`,

	"config": `clipd config file

Location:

  macOS   ~/Library/Application Support/clipd/config.json
  Linux   $XDG_CONFIG_HOME/clipd/config.json, or ~/.config/clipd/config.json

Override with -config <path> or CLIPD_CONFIG. Every value has a working
default, so a daemon with no config file at all is a working daemon.

Keys:

  address             socket path to listen on (default ~/.clipd.sock).
                      A leading /, ~ or . means a UNIX socket. Anything else
                      is host:port, which nothing authenticates — see
                      'clipd help security'.
  drop_dir            where dropped files land (default ~/Drop)
  max_payload_bytes   largest clipboard message (default 10485760)
  max_drop_bytes      largest drop, in total (default 268435456)
  max_drop_files      most files in one drop (default 256)
  max_concurrent      messages handled at once (default 8). Together with
                      max_payload_bytes this is the daemon's memory ceiling,
                      since a clipboard message is buffered whole.

Unknown keys are rejected rather than ignored, so a typo fails loudly instead
of silently leaving a default in place.

CLIPD_CONFIG is the only environment variable clipd reads.`,

	"security": `clipd security model

clipd has no authentication and no encryption of its own, and that is the
design rather than a gap in it.

The daemon listens on a UNIX domain socket in your home directory. Nothing is
bound to the network: there is no port to scan and nothing on your local
network can reach it, on a coffee shop wifi or anywhere else.

The socket reaches another machine only when you forward it over SSH. By the
time bytes arrive, SSH has encrypted the channel, verified the host key
against known_hosts, and authenticated you. The socket's 0600 permissions then
decide who on that machine may write to it.

Adding a token here would duplicate a decision SSH has already made, and worse:
a token stored on the remote machine is a stealable secret that works from
anywhere until you rotate it, whereas the socket is an ephemeral capability
that dies with the SSH session and cannot be copied off the box.

What this does mean:

  Anything running as you on the remote host can write to your clipboard and
  send you files. Other user accounts there cannot, because of the socket's
  permissions, but your own processes can — including a build script or a
  package install.

That is inherent to letting a remote machine write to your clipboard at all.
It also matters less than it sounds: anything positioned to abuse it already
has your files, your history and your keystrokes on that machine.

Two things reduce what it can do to you:

  Bracketed paste, on by default in modern shells, means pasted text ending in
  a newline is not executed until you press Enter.

  Dropped files are flattened to basenames, never overwrite, are never made
  executable, and carry macOS's quarantine attribute.

Setting 'address' to a host:port turns all of this off. There is no
authentication behind it, so a TCP listener is only ever appropriate on the
loopback interface.`,

	"install": `clipd install

macOS only. Writes ~/Library/LaunchAgents/com.clipd.agent.plist and loads it
into your GUI session, so the daemon starts at login and restarts if it
crashes. No root privileges are required.

Options:
  -exec <path>   binary path to record in the plist (default: this binary)`,

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
security, install, uninstall, status, version.`,
}
