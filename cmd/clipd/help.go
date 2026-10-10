package main

var helpTopics = map[string]string{
	"serve": `clipd serve [options]

Run the daemon in this terminal. Press Ctrl-C to stop it.

On macOS, 'clipd install' runs it in the background and starts it at login.
On a Linux desktop, use this command in a graphical session with wl-copy,
xclip or xsel installed. macOS uses pbcopy.

Options:
  -address <path>     socket path (default: from config)
  -drop-dir <path>    folder for received files (default: from config)

Without a config file, the defaults are ~/.clipd.sock and ~/Drop.`,

	"setup": `clipd setup [options] <host>

Run on the receiving computer to configure an SSH host.
Setup connects to the host, checks for nc or socat, and writes:

  On the host:  a clipd function in ~/.bashrc, ~/.zshrc or ~/.profile,
                plus a private ~/.clipd directory for the socket.
                No binary or service is installed.
  Locally:      a block in ~/.ssh/config that forwards the host's socket
                to the local clipd socket.

Running setup again replaces the marked clipd blocks and leaves the rest
of each file alone. Existing files are backed up to .clipd-backup files
before the first change.

<host> can be an SSH alias, hostname, IP address or user@host. Use the same
name you use when connecting. If you connect by two names, set up both.

Log out and reconnect afterwards to use the forward. Run setup again
when you need to update the host's clipd function.

Options:
  -print    show the proposed changes without writing them

To remove the setup, delete the marked clipd blocks from your local SSH
config and the host's shell startup file.
See Troubleshooting in the README for connection and leftover-socket problems.`,

	"drop": `clipd drop [--no-progress] <file-or-folder> ...
command | clipd drop --name <filename>

Run on the remote host. Sends files and folders to the receiving computer's
configured drop folder (default: ~/Drop).

  clipd drop report.pdf               send report.pdf
  clipd drop photos                   send the folder and keep its structure
  clipd drop photos/*                 send the items matched by your shell
  journalctl -u nginx | clipd drop --name nginx.log

Existing files are never overwritten. A second report.pdf arrives as
report-1.pdf. clipd reports success after the receiving computer has saved
the drop, so you can use:

  clipd drop report.pdf && rm report.pdf

Symlinks and other special files are skipped, never followed.
The reply tells you how many were skipped.

Drops show progress in a terminal. Use --no-progress to hide it.

Default limits: 256 MiB and 256 files per drop, 30 minutes per transfer.
Errors identify the limit you hit. Run 'clipd help config' on the receiving
computer for settings and limits.

To run a drop over SSH without opening a shell first:

  ssh host 'bash -ic "clipd drop report.pdf"'

Use zsh -ic on Zsh hosts. ssh -t alone does not load the interactive shell
startup file. See the README for quoting and shell startup details.`,

	"config": `clipd help config

The config file is optional. Without it, clipd uses the defaults below.
'clipd install' writes the file and preserves existing settings.

Default path: ~/.config/clipd/config.json
With XDG_CONFIG_HOME set: $XDG_CONFIG_HOME/clipd/config.json
Use -config <path> or CLIPD_CONFIG to select a different file.

  setting                default        meaning
  address                ~/.clipd.sock  UNIX socket path
  drop_dir               ~/Drop         folder for received files
  max_payload_bytes      10485760       bytes per clipboard copy (10 MiB; max 1 GiB)
  max_drop_bytes         268435456      bytes per drop (256 MiB; max 1 TiB)
  max_drop_files         256            files per drop (max 65536)
  max_concurrent         8              copies and drops at once (max 64)
  max_transfer_seconds   1800           seconds per transfer (30 minutes; max 24 hours)

Sizes are bytes; times are seconds. For example, 10 GiB is 10737418240
bytes and one hour is 3600 seconds.

Omitted settings use their defaults. For numeric limits, 0 also uses the
default; it does not mean unlimited. Unknown settings are rejected.
max_payload_bytes multiplied by max_concurrent must not exceed 2 GiB.

Settings are read when the daemon starts. On macOS, run 'clipd restart'
after editing them. On Linux, stop and restart 'clipd serve'.

'clipd status' shows the resolved config and, for the macOS login service,
warns when the file has been edited since the daemon started.
Config files from older clipd releases are not migrated.`,

	"security": `clipd help security

SSH encrypts forwarded traffic, verifies the host key and authenticates
your account. clipd uses private UNIX sockets and adds no separate password
or encryption layer.

On the remote host, the socket is inside your private ~/.clipd directory.
Other ordinary accounts cannot use it, but anything running as your user
can write to the receiving computer's clipboard and send it files.
Only forward clipd to accounts whose programs you trust with that access.

Received files stay inside the configured drop folder, never overwrite
existing files, and are not given executable permissions. Symlinks and
other special files are skipped.

Sizes, file counts, connections and transfer time are bounded.
These limits do not cap the total size of files you keep over time.

SECURITY.md in the repository describes the guarantees and known exceptions.`,

	"install": `clipd install [options]

macOS only. Starts clipd now and at login, and restarts it if it crashes.
Installs ~/Library/LaunchAgents/com.clipd.agent.plist for your user account.
No sudo is needed.

It also writes the selected config file with all settings filled in,
preserving existing values. The default is ~/.config/clipd/config.json;
XDG_CONFIG_HOME, -config or CLIPD_CONFIG can select another path.

Options:
  -exec <path>   clipd binary to run (default: this binary)`,

	"restart": `clipd restart

macOS only. Stops and restarts the installed clipd login service.
Run this after editing the config so the daemon reads the new settings.

'clipd status' warns if the config file has been edited since the service
started.

Equivalent to:

  launchctl kickstart -k gui/$(id -u)/com.clipd.agent`,

	"uninstall": `clipd uninstall

macOS only. Stops the daemon and removes its login service. Everything else
clipd created stays where it is:

On this computer:
  ~/.config/clipd/config.json      settings
  ~/Drop/                          files you received
  ~/Library/Logs/clipd/            the daemon's log
  ~/.ssh/config                    one marked clipd block per host
  ~/.ssh/config.clipd-backup       your SSH config from before the first setup

On each host you set up:
  ~/.bashrc, ~/.zshrc or ~/.profile   the clipd function, in a marked block
  ...clipd-backup                     a copy from before the first setup
  ~/.clipd/                           a private folder for the socket

To remove them, delete the marked clipd blocks and these files. The clipd
binary stays wherever you installed it.`,

	"status": `clipd status

Shows the resolved config and checks whether the daemon answers.
On macOS, it also shows the login service's state and log path.

It talks to the daemon rather than checking only for a socket file,
which can remain after a crash.

The displayed settings come from the config file, not from the running
daemon. For the macOS login service, status warns about edits made since
the daemon started.

Exits 0 when the daemon answers and 1 when it does not.
Config and command errors have separate exit codes.
You can use 'clipd status && ...' in scripts.`,

	"version": `clipd version

Shows the version, commit and build date, plus the Go version and platform
it was built for.`,

	"help": `clipd help [topic]

With no topic, shows the overview.
Topics: serve, setup, drop, config, security, install, restart, uninstall,
status, version.`,
}
