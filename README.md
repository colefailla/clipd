# clipd

[![CI](https://github.com/colefailla/clipd/actions/workflows/ci.yml/badge.svg)](https://github.com/colefailla/clipd/actions/workflows/ci.yml)

Copy command output and send files from an SSH host to your Mac.

clipd works through SSH without relying on terminal clipboard support, and can
send files as well as text.

On a configured remote host:

```bash
command | clipd                         # copy command output
clipd notes.txt                         # copy a file's contents
clipd drop report.pdf                   # send a file
command | clipd drop --name output.txt  # save command output as a file
```

The receiving computer runs the daemon. Remote hosts use a shell function and
an SSH-forwarded UNIX socket; no remote binary or service is installed.

```text
Remote host                         Receiving computer

clipd function ──▶ ~/.clipd/socket ══ SSH ══▶ clipd daemon
                                                ├──▶ clipboard
                                                └──▶ ~/Drop
```

Remote hosts need Linux, macOS or a BSD, a POSIX shell, compatible `nc` or
`socat`, and OpenSSH 6.7 or newer. File and folder drops also need `tar`.
Windows, Dropbear and older OpenSSH versions are not supported.

## Install

Install on the receiving computer. On macOS, download the release binary
(use `clipd_darwin_amd64` instead for an Intel Mac):

```bash
curl -fsSL https://github.com/colefailla/clipd/releases/latest/download/clipd_darwin_arm64 -o clipd
sudo install -m 0755 clipd /usr/local/bin/clipd
rm clipd
clipd install
```

Or build from source with Go 1.24 or newer:

```bash
git clone https://github.com/colefailla/clipd
cd clipd
make install
clipd install
```

`clipd install` starts the macOS login service and writes the config file,
preserving existing settings. The Go module has no third-party dependencies.

A Linux desktop can receive copies and drops too. Use `clipd_linux_amd64` or
`clipd_linux_arm64` from the [releases](https://github.com/colefailla/clipd/releases),
or build from source. Run `clipd serve` in a graphical session with `wl-copy`,
`xclip` or `xsel` available.
The login service commands (`install`, `restart`, `uninstall`) are macOS only.

### Updating

For a source install on macOS:

```bash
git pull && make reinstall
```

This builds, installs, restarts the daemon and shows its status. Update the
receiving computer first, then run `clipd setup <host>` for each host to update
its function. On Linux, rebuild with `make install` and restart `clipd serve`.

Functions from before streaming drops cannot send file or folder archives to
the current daemon; the error tells you to rerun setup. Clipboard copies and
`clipd drop --name` still work with those functions.

## Set up an SSH host

Run on the receiving computer, using the same destination you pass to SSH:

```bash
clipd setup debian
```

Setup checks the host's tools, writes the clipd function into its `~/.bashrc`,
`~/.zshrc` or `~/.profile`, and creates a private `~/.clipd` directory. Locally,
it adds the socket forward to `~/.ssh/config`.

Both files use marked clipd blocks. Re-running setup replaces those blocks and
preserves the surrounding content. Existing files get a `.clipd-backup` before
the first change. Preview the changes with `clipd setup -print debian`; this
still connects to the host but does not write files.

Reconnect to use the forward:

```bash
ssh -O exit debian 2>/dev/null; ssh debian
```

If you use SSH connection sharing, `ssh -O exit` closes the shared connection
and any sessions using it.

Aliases, hostnames, IPv4 addresses and `user@host` are accepted.
For an IPv6 address, use an SSH alias with that address as its `HostName`.
Set up each spelling you use: `debian` and `debian.local` need separate blocks. Different
accounts, such as `alice@server` and `bob@server`, are configured separately.
Even without `user@`, setup scopes the forward to the account it connected to.
If you change the account an alias uses, rerun setup. Rerunning setup also
replaces older blocks that matched every account at that destination.

Setup selects the appropriate `nc` flags or uses `socat`. It does not install
missing packages. Fish, csh and tcsh cannot load the function; setup prints how
to use it from a POSIX shell.

## Usage

Run these on a host you have set up:

```bash
ls -l | clipd                            # copy stdout
clipd notes.txt                          # copy a file's contents
clipd notes.txt other.txt                # copy their contents together

clipd drop report.pdf                    # send a file
clipd drop src/*.go                      # send several files
clipd drop photos                        # send a folder and keep its structure
pg_dump mydb | clipd drop --name db.sql  # send command output as a file
```

Copies and drops print the receiver's reply:

```text
clipd: ok: copied 47 bytes
```

A successful drop exits 0 after the files have been saved, so
`clipd drop report.pdf && rm report.pdf` removes the source only after success.

`--name` names a file made from stdin. In a pipeline, clipd cannot detect a
failure in the preceding command; use `set -o pipefail` in Bash or Zsh if you
need that reflected in the pipeline's exit status. Slow or silent producers
are allowed within the transfer time limit.

### Files and folders

Drops go to `~/Drop` by default; `drop_dir` changes the destination.

- A file keeps its name. A second `report.pdf` becomes `report-1.pdf`.
- Folders keep their structure. A second `photos` becomes `photos-1`; existing
  folders are never merged.
- `clipd drop photos/*` sends the items your shell matches, usually excluding
  hidden files.
- Received files have mode `0600`, directories `0700`. Files are not made
  executable, and macOS quarantine is applied where possible.
- Symlinks, hard links, devices and other special files are skipped. The reply
  counts them.

Files are staged privately inside the drop directory and published after the
complete archive validates. Publication of multiple names is not crash-atomic.
If the connection fails after publication, the files can arrive without a
success reply; check the destination before resending.

Drops show progress when stderr is a terminal. Use `clipd drop --no-progress`
to hide it. Piped input has no known size, so its progress has no percentage or
ETA.

### Commands over SSH

A command sent through SSH may not load the startup file containing clipd.
On Debian, Bash's usual `.bashrc` guard skips the function; Zsh loads `.zshrc`
only for interactive shells. Ask for an interactive shell explicitly:

```bash
ssh debian 'bash -ic "clipd drop report.pdf"'
ssh zsh-host 'zsh -ic "clipd drop report.pdf"'
```

For a filename with spaces:

```bash
ssh debian "bash -ic 'clipd drop \"report final.pdf\"'"
```

This runs the whole interactive startup file, including any output or tmux
startup commands, and may write shell history. Without a terminal, Bash may
print a job-control warning. `ssh -t` supplies a terminal but does not make
the shell interactive by itself; avoid it for piped binary input.

The Zsh example assumes setup's `~/.zshrc` is the file Zsh reads; a custom
`ZDOTDIR` may point elsewhere. For Bash, moving the clipd block above the
non-interactive return is another way to make direct SSH commands work.

## Configuration

The config file is optional. Its default path is `~/.config/clipd/config.json`,
or `$XDG_CONFIG_HOME/clipd/config.json` when that variable is set. Use
`-config <path>` or `CLIPD_CONFIG` to select another file. `clipd install`
writes the selected file with all settings filled in.

| Setting | Default | Meaning |
|---|---|---|
| `address` | `~/.clipd.sock` | UNIX socket path; TCP addresses are rejected |
| `drop_dir` | `~/Drop` | folder for received files |
| `max_payload_bytes` | `"10 MiB"` | bytes per clipboard copy; max 1 GiB |
| `max_drop_bytes` | `"256 MiB"` | bytes per drop; max 1 TiB |
| `max_drop_files` | `256` | files per drop; max 65536 |
| `max_concurrent` | `8` | copies and drops at once; max 64 |
| `max_transfer_seconds` | `1800` (30 minutes) | seconds per transfer; max 86400 (24 hours) |

The two byte limits accept integer bytes (such as `5368709120`) or quoted
whole-number sizes (such as `"5 GiB"`). Separate the number and unit with a
space. Units are case-insensitive: B, KB, MB, GB, TB, KiB, MiB, GiB, TiB.
GB means 1,000,000,000 bytes; GiB means 1,073,741,824 bytes.
Fractions are not accepted; use `"1536 MiB"` for exactly 1.5 GiB.

`clipd install` writes exact whole-unit sizes where possible and integer bytes
otherwise. Older binaries only accept numeric byte values; convert size strings
back to numbers before downgrading. Existing numeric configs still work.

Times are seconds. Omitted settings use defaults. For numeric
limits, `0` also means the default, not unlimited. Unknown settings and invalid
values are rejected. Obsolete settings from pre-socket releases are rejected.

`max_payload_bytes` multiplied by `max_concurrent` must not exceed 2 GiB.
This bounds payload sizes, not total process memory; buffers and other state
add overhead. Installation rejects invalid combinations before changing
the service.
Concurrent drops can each use up to `max_drop_bytes` of disk space. Received
files accumulate until you remove them.

For `clipd install`, `address` and `drop_dir` must be absolute paths or begin
with `~/`. Relative paths are supported by foreground `clipd serve`, where
they resolve from its working directory.

Settings are read at startup. After editing, run `clipd restart` on macOS or
stop and restart `clipd serve` on Linux. `clipd status` displays the resolved
config, not the daemon's active settings; for the macOS login service, it also
warns about config edits made since the daemon started.

Install and restart wait up to five seconds for a clipd identification reply.
Restart checks the socket selected by the installed LaunchAgent, independent of
`-config` or `CLIPD_CONFIG` in the invoking shell. A failed check exits 1 and
prints the log path; it does not remove the installed service. This checks the
local receiver, without writing to the clipboard or sending a drop. It does not
verify a remote SSH forward.

Status shows the CLI version, socket permissions, service state and PID,
installed binary, config, drop, plist and log paths. Its limits describe the
selected config file, not settings queried from the running receiver.

### Big files

An error identifies the limit you hit. For example, to allow drops up to
20 GiB and transfers up to one hour, set these fields in the config:

```json
{
  "max_drop_bytes": "20 GiB",
  "max_transfer_seconds": 3600
}
```

Restart the daemon after editing. The receiver needs disk space for the whole
drop. For output larger than the clipboard limit, use
`command | clipd drop --name output.txt`.

## Security

SSH encrypts forwarded traffic, verifies the host key and authenticates your
account. The daemon listens only on a private UNIX socket, mode `0600`. The
remote socket is inside a `0700` directory. Its own permissions are controlled
by the remote SSH server, whose default bind mask is `0177` (socket mode `0600`).
Client-side bind-mask settings do not control a remote forward.

Any process running as your user on a forwarded host can write to your
clipboard and send files to your computer. Other ordinary accounts cannot
use the private socket. Forward clipd only to accounts whose programs you
trust with that access.

Drop paths are validated, existing files are not overwritten, and received
files are not made executable. Connections, concurrent work, payloads and
transfer time are bounded. See [SECURITY.md](SECURITY.md) for the exact
guarantees and known exceptions.

## Troubleshooting

Start with `clipd status` on the receiving computer. Exit 0 means the clipd
daemon answered; a socket file alone does not prove it is running. For the
macOS login service, logs are in `~/Library/Logs/clipd/clipd.log`. Foreground
`clipd serve` writes logs to its terminal.

### Leftover sockets

sshd normally leaves `~/.clipd/socket` behind when a session ends and refuses
to replace it on the next login. This can cause `remote port forwarding failed`.

When clipd detects a stale socket, it removes it and asks you to reconnect.
Re-running setup applies the same check. A listener that accepts connections
is preserved even if the receiving daemon is unavailable. On macOS remotes,
`nc`'s silent failure is a heuristic for refusal; see [SECURITY.md](SECURITY.md).

On a server you administer, you can set `StreamLocalBindUnlink yes` in **sshd's**
configuration so it replaces existing sockets. On a Debian-style system that
includes `/etc/ssh/sshd_config.d/*.conf`:

```bash
ssh -t debian "echo 'StreamLocalBindUnlink yes' | sudo tee /etc/ssh/sshd_config.d/clipd.conf && { sudo systemctl reload ssh || sudo systemctl reload sshd; }"
```

Otherwise, add the setting to the server's `/etc/ssh/sshd_config` and reload
sshd using that system's service manager. Check the effective value on the
server with `sudo sshd -T | grep -i streamlocalbindunlink`.

This is a server-wide setting. Leave it to the administrators on work or shared
servers. Setting it in your own `~/.ssh/config` does not affect remote socket
binding. If the server disables socket forwarding, clipd cannot use it.

### Connection errors

| Message | Next step |
|---|---|
| `clipd: removed the unused socket at ...` | Log out and reconnect so SSH can create the forward. |
| `clipd: no SSH forward at ...` | Connect using the destination you set up. Check for `ClearAllForwardings=yes` or an older shared connection. |
| `Warning: remote port forwarding failed` | Check for a leftover socket or another session holding the forward. That session's forward may still work while it is open. With server-side `StreamLocalBindUnlink yes`, the newest session takes over the socket path. |
| `clipd: could not confirm the copy.` / `clipd: could not confirm the drop.` | Check the receiver with `clipd status` and read its log. This can be a transport failure or an invalid or missing reply. For drops, check the destination before retrying; files may have arrived. |
| `clipd: error: drop exceeds max_drop_bytes (...)` | Adjust the setting identified in the error and restart the receiver. See [Big files](#big-files). |

For an older ControlMaster connection, close it with `ssh -O exit <host>` and
reconnect. This closes sessions sharing that connection.

### Missing tools or shell functions

Setup needs a UNIX-socket-capable `nc` or `socat`. On Linux, `netcat-openbsd`
supports `-U` and `-N`; `netcat-traditional` does not support `-U`. macOS `nc`
uses different flags, which setup handles. Install the package named in setup's
error, then rerun setup. Without `tar`, clipboard copies and named stdin drops
still work.

For `clipd: command not found` in an SSH command, see
[Commands over SSH](#commands-over-ssh). For Fish, csh or tcsh, follow setup's
instruction to open `sh` and source the installed function.

### Exit codes

```text
0   success
1   the daemon did not answer, or an operation failed
4   configuration error
64  usage error
```

## Uninstall

On macOS:

```bash
clipd uninstall
```

This stops the daemon and removes its LaunchAgent. The binary, config, logs,
received files and SSH setup remain. On Linux, stop `clipd serve`.

### Everything clipd touches

These are the default paths on macOS; custom install, config, socket and drop
paths may differ.

| Path | What it is |
|---|---|
| `/usr/local/bin/clipd` | binary, installed by you |
| `~/Library/LaunchAgents/com.clipd.agent.plist` | login service, removed by `clipd uninstall` |
| `~/.config/clipd/config.json` | settings, written by `clipd install` |
| `~/.clipd.sock` | daemon socket, removed on a clean shutdown |
| `~/Drop/` | files you received |
| `~/Drop/.clipd-stage-*` | private staging folders; completed ones are removed, stale ones cleaned on startup and periodically while running |
| `~/Library/Logs/clipd/` | daemon logs, written by launchd |
| `~/.ssh/config` | one marked forward block per destination |
| `~/.ssh/config.clipd-backup` | SSH config from before the first edit |

To remove the default installation's binary, config, logs and socket:

```bash
sudo rm /usr/local/bin/clipd
rm -rf ~/.config/clipd ~/Library/Logs/clipd ~/.clipd.sock
```

Delete the marked clipd blocks from `~/.ssh/config` by hand. Keep or remove
backups as you choose. Review the files in `~/Drop` before deleting them.

On each remote host:

| Path | What it is |
|---|---|
| `~/.bashrc`, `~/.zshrc` or `~/.profile` | function between `# >>> clipd >>>` and `# <<< clipd <<<` |
| matching `.clipd-backup` | startup file from before the first edit |
| `~/.clipd/` | private directory created by setup |
| `~/.clipd/socket` | socket created by sshd; may remain after logout |
| `/etc/ssh/sshd_config.d/clipd.conf` | only if you added the optional server setting |

Delete the marked function block and remove `~/.clipd` after closing sessions
using its forward. If you added the optional sshd setting, remove it and reload
sshd to undo that change.

## Development

Run `make check` for formatting, vet and race tests. CI requires sh, dash,
Bash, Zsh, OpenBSD nc (macOS nc on Macs), and socat, installing missing test
tools so both supported transports are exercised.

`CLIPD_TEST_SSHD=1 go test ./cmd/clipd -run TestSSHForwardLifecycle -v`
starts a temporary loopback-only sshd with temporary keys and configuration.
It tests transfer, stale-socket recovery, reconnect, and a live forward with
the receiver down. It opens no remote shell and does not install a service or
edit SSH configuration. Run it on a disposable runner with sshd available.

`go test ./internal/drop -run '^$' -bench BenchmarkExtract -benchtime=3x`
compares an 8 MiB file with 256 files totaling 8 MiB, including file sync and
macOS quarantine. `-bench BenchmarkQuarantine` measures the native quarantine
helper separately on macOS. All output files are temporary.

## Credits

Inspired by [wincent/clipper](https://github.com/wincent/clipper), with file drops
and automated SSH setup.

## License

MIT
