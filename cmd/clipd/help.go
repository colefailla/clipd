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

Run on the Mac. Probes a POSIX-shell remote and configures socket forwarding.
The destination may be an SSH alias, hostname, literal IPv4/IPv6 address, or
user@host; all use SSH UNIX sockets. Run setup separately for each destination.
Later setups preserve other destinations' forwarding blocks.
Only the destination spelling supplied to setup receives the generated block.

Files created or edited:
  Mac: ~/.ssh/config gets a managed Host/Match block and RemoteForward.
       config.clipd-backup preserves the first version before clipd's edits.
  Remote: ~/.bashrc, ~/.zshrc or ~/.profile gets the generated shell function.
          Its .clipd-backup preserves the first version before clipd's edits.
          ~/.clipd is created/restricted to 0700; SSH binds socket inside it.

RemoteForward connects the remote socket to the Mac's daemon socket.
StreamLocalBindMask requests a private socket; Host * resets config scope.
ControlMaster auto and ControlPersist yes keep the SSH forward alive after
shell logout. ControlPath hashes the resolved host, port, user and jump host.
Normal connections use ssh <host>; no extra flags are required.
The shared SSH connection runs in the background on the Mac. The forwarded
capability remains available until that connection ends.
No remote binary or service is installed. No daemon config file is created.
Remote rc writes preserve inode/symlink metadata but are not crash-atomic.
A backup is a recovery aid, not cross-machine rollback. Partial success is
reported; re-running setup replaces managed blocks and is safe to retry.
Setup clears a confirmed stale remote socket and preserves active listeners.
If recovery fails, it reports that setup files were updated but the socket
could not be repaired. Existing sockets require OpenBSD-compatible nc for
safe inspection. -print does not attempt socket recovery.

Options:
  -print    show generated shell and SSH settings without editing those files

Reconnect afterward. Remove managed blocks to undo setup, retaining unrelated
content. Backups are first versions, not current snapshots. For stale remote
sockets and competing sessions, see README Troubleshooting.`,

	"drop": `clipd drop [--no-progress] <file-or-directory>...
       <command> | clipd drop --name <filename>

Run on the configured remote host. Sends files beneath the Mac's drop directory
(default ~/Drop). Use clipd -h on the remote for a short command summary.

  clipd drop report.pdf
  clipd drop author/book
  clipd drop author/book/*
  journalctl -u nginx | clipd drop --name nginx.log

Directories retain their structure. Wildcards are expanded by your shell;
children land directly in Drop, normally excluding hidden files. Existing files
and directories get numbered alternatives, never overwrites or directory merges.
Conflicting paths within one request and special archive entries are rejected.

Archives stream without remote payload staging. The Mac publishes only after
validation, a successful-producer completion marker and EOF. A failed transfer
cleans up its private received files; originals are untouched. No retry or
resume. Multi-name publication is not crash-atomic; cleanup errors are reported.
A lost final reply can report failure after files were published; check Drop
before retrying. The exit status is the daemon's answer, so clipd drop x && rm x
remains safe.
Upstream status in producer | clipd drop --name x is your shell's responsibility.

Interactive progress shows percentage for the current file, bytes, speed,
elapsed time and ETA on stderr. Unknown-size stdin shows bytes and time.
--no-progress hides it. Only the final clipd: ok: confirms publication.

Default limits: 256 MiB total, 256 files, 30-minute lifetime, 30-second idle.
On the Mac, use clipd help config to adjust size/lifetime, then clipd restart.
Upgrade the Mac binary before rerunning setup to update the remote function.
Older functions remain supported with their original flattened/staged behavior.

On Debian, noninteractive .bashrc commonly returns before the generated block.
Use an interactive shell or put the block before that return.`,

	"config": `clipd config file

Location: $XDG_CONFIG_HOME/clipd/config.json, or ~/.config/clipd/config.json.
Override with -config <path> or CLIPD_CONFIG. Built-in defaults work without a
file. clipd install writes all supported settings, retaining configured values;
clipd setup does not create this file. Pre-v3 config files are not migrated.

Keys (sizes are numeric bytes, times are numeric seconds):
  address                ~/.clipd.sock; UNIX path or manual loopback TCP address
  drop_dir               ~/Drop; published files and directories
  max_payload_bytes      10485760 (10 MiB); clipboard, 1 byte to 1 GiB
  max_drop_bytes         268435456 (256 MiB); whole drop, maximum 1 TiB
  max_drop_files         256; regular files per drop, maximum 65536
  max_concurrent         8; work slots, maximum 64
  max_transfer_seconds   1800 (30 minutes); lifetime, 1 to 86400 (24 hours)

Omitted keys use defaults. Zero means default for numeric limits and lifetime,
not unlimited. Negative values and unknown keys are rejected.
Clipboard size times concurrency must fit a 2 GiB budget; buffer allocation may
transiently exceed it. Each concurrent drop can stage up to its own disk limit.
The fixed 30-second idle timeout remains active even with a longer lifetime.

10 GiB = 10737418240; 100 GiB = 107374182400; one hour = 3600 seconds.
After editing config.json, run clipd restart on the Mac. Settings are read only
at startup. clipd status shows limits and flags edits since startup.
Older binaries reject the new lifetime key; upgrade before adding it.`,

	"security": `clipd security model

clipd has no authentication and no encryption of its own, and that is the
design rather than a gap in it.

The default listener is a private UNIX socket. The manual TCP fallback binds
loopback only and is reachable by other local accounts. SSH supplies encrypted
transport for forwarded traffic; filesystem permissions authorize UNIX access.

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

  Streaming drops validate relative paths and retain directories. Files never
  overwrite, never arrive executable, and receive best-effort quarantine.
  Legacy archive drops retain basename flattening. Hard-link publication and
  quarantine use pathname APIs, so root-path confinement is not absolute.

The 'address' setting also accepts a loopback host:port, for hosts whose SSH
cannot forward a UNIX socket — OpenSSH gained that ability only in 6.7, and its
Windows build still lacks it. That is a tunnel endpoint like the socket, not a
network service: a reachable address is refused at startup rather than warned
about, because nothing here authenticates and no warning makes that safe.

What a port gives up against a socket is that any user on the machine can reach
loopback, where a 0600 socket admits only its owner. Prefer the socket wherever
SSH can forward one.`,

	"install": `clipd install

macOS only. Writes ~/Library/LaunchAgents/com.clipd.agent.plist and loads it
into your GUI session, so the daemon starts at login and restarts if it
crashes. No root privileges are required.

Options:
  -exec <path>   binary path to record in the plist (default: this binary)`,

	"reconnect": `clipd reconnect <ssh-host>

Run on the Mac after clipd setup. Checks the daemon and SSH configuration,
resets this destination's clipd-managed shared connection, safely clears a
stale remote socket, and restores the SSH forward in the background.
Returns to your Mac prompt. Normal logins use ssh <ssh-host>.

Resetting the shared connection disconnects shells using it; close them first.
It opens no login shell, rewrites no setup files, and does not restart the Mac
clipboard daemon. Use ssh -O exit <host> to close the background connection.

Existing sockets require nc. A protocol ping
must report an explicit refused connection before deletion. Other active
listeners, ambiguous errors, symlinks, changed files, and non-socket files are
preserved. Network failures or terminating the master can still require
reconnect; normal shell logout leaves the shared forward running.`,
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
security, install, restart, reconnect, uninstall, status, version.`,
}
