# clipd

[![CI](https://github.com/colefailla/clipd/actions/workflows/ci.yml/badge.svg)](https://github.com/colefailla/clipd/actions/workflows/ci.yml)

Copy text and send files from an SSH session to your computer.

Run these on the remote host after setup:

```sh
command | clipd                   # copy output to your clipboard
clipd notes.txt                    # copy a file's contents
clipd drop report.pdf photos       # save files and folders to ~/Drop
```

Install clipd on your Mac, then run `clipd setup <host>` for each SSH host you
use. The remote side uses a shell function, `nc` or `socat`, and `tar` for file
drops. No remote binary or service is installed, and no terminal clipboard
integration is needed.

Linux desktops can also receive copies and files using `clipd serve`.

## Install

Install on the computer that will receive the text and files.

### macOS

For an Apple Silicon Mac:

```sh
curl -fsSL https://github.com/colefailla/clipd/releases/latest/download/clipd_darwin_arm64 -o clipd
sudo install -m 0755 clipd /usr/local/bin/clipd
rm clipd
/usr/local/bin/clipd install
```

For an Intel Mac, use `clipd_darwin_amd64` in the download URL instead.
All release binaries are on the [releases page](https://github.com/colefailla/clipd/releases).

`clipd install` starts the receiver now and at login. It also writes the config
file, preserving existing settings. A successful install reports that clipd is
responding.

The examples below assume `/usr/local/bin` is on your `PATH`. Otherwise, use
`/usr/local/bin/clipd` for local commands.

### Build from source

With Go 1.24 or newer and `make` installed:

```sh
git clone https://github.com/colefailla/clipd
cd clipd
make install
```

`make install` builds and installs the executable into `/usr/local/bin` by
default. It may ask for sudo access to that directory. On macOS, then run:

```sh
/usr/local/bin/clipd install
```

This second command installs and starts the login service.

### Linux desktop

Download `clipd_linux_amd64` or `clipd_linux_arm64` from the releases page, or
build from source. Install `wl-copy`, `xclip` or `xsel`, then run the receiver
in a graphical session:

```sh
clipd serve
```

Keep it running while using clipd remotely. The login service commands
(`install`, `restart`, `uninstall`) are macOS only.

## Set up a remote host

Replace `<host>` in these examples with the destination you normally pass to
SSH: an alias, hostname, or `user@hostname`. Do not type the angle brackets.
Use the same destination for setup and subsequent connections.

**On your Mac or receiving Linux desktop:**

```sh
clipd setup <host>
```

Setup adds a shell function to the remote startup file and a socket forward to
your local `~/.ssh/config`. It creates a private `~/.clipd` directory on the
remote host. It backs up existing files before their first change and preserves
content outside its marked blocks.

Exit any old remote shell and open a new connection:

```sh
ssh <host>
```

If you use SSH connection sharing, first run `ssh -O exit <host>` locally.
This closes the shared connection and its sessions.

**In the new remote shell:**

```sh
printf 'hello from remote\n' | clipd
```

Paste on your receiving computer. You should see `hello from remote`.

Repeat setup for each host you use. See [SSH setup details](#ssh-setup-details)
for startup files, aliases, account matching, and a preview option.

## Usage

Run these commands on a configured remote host.

### Copy text

```sh
command | clipd
clipd notes.txt
clipd notes.txt other.txt
```

File arguments copy their contents to the receiving computer's clipboard.
Multiple files are concatenated. A successful copy prints a reply such as:

```text
clipd: ok: copied 47 bytes
```

### Send files and folders

```sh
clipd drop report.pdf
clipd drop file1.txt file2.txt
clipd drop photos
clipd drop *.pdf
command | clipd drop --name output.txt
```

Drops go to `~/Drop` on the receiving computer by default.

- Files keep their names. If `report.pdf` exists, the new file becomes `report-1.pdf`.
- Folders keep their structure. A second `photos` becomes `photos-1`; existing
  folders are never merged.
- Wildcards are expanded by your shell. `clipd drop photos/*` usually excludes
  hidden files; `clipd drop photos` sends the folder itself, including hidden files.
- Symlinks, hard links, devices and other special files are skipped. The reply
  reports how many were skipped.

Drops show progress in a terminal. Use `clipd drop --no-progress <file>` to hide
it. Piped input has no known size, so its progress has no percentage or ETA.

A successful drop exits 0 after the files have been saved. You can use:

```sh
clipd drop report.pdf && rm report.pdf
```

If a transfer is interrupted or its result cannot be confirmed, check the
destination before retrying. Files may have arrived, and a crash during
publication can leave only some of the files in place.

`--name` saves stdin as one file. In a pipeline, clipd cannot detect failure in
the preceding command; use `set -o pipefail` in Bash or Zsh if you need that
reflected in the pipeline's exit status. Slow or silent producers are allowed
within the transfer time limit.

## Configuration

Settings are in `~/.config/clipd/config.json` by default. `clipd install` writes
this file and preserves existing values. Without a config file, the receiver
uses built-in defaults:

```json
{
  "address": "~/.clipd.sock",
  "drop_dir": "~/Drop",
  "max_payload_bytes": "10 MiB",
  "max_drop_bytes": "256 MiB",
  "max_drop_files": 256,
  "max_concurrent": 8,
  "max_transfer_seconds": 1800
}
```

For example, to allow up to 5 GiB per drop, change `max_drop_bytes` to
`"5 GiB"`, then restart the receiver. The drop size and file-count limits apply
to **all files in one invocation combined**.

| Setting | Meaning | Maximum |
|---|---|---|
| `address` | local UNIX socket path | — |
| `drop_dir` | destination for received files | — |
| `max_payload_bytes` | size of one clipboard copy | 1 GiB |
| `max_drop_bytes` | total file bytes in one drop | 1 TiB |
| `max_drop_files` | files in one drop | 65536 |
| `max_concurrent` | simultaneous copies and drops | 64 |
| `max_transfer_seconds` | time allowed for one transfer | 86400 seconds (24 hours) |

The two byte limits accept integer bytes or quoted whole-number sizes:

- `5368709120` and `"5 GiB"` mean the same size.
- `"5 GB"` means 5,000,000,000 bytes; GiB uses powers of 1024.
- Separate the number and unit with a space. Units are case-insensitive:
  B, KB, MB, GB, TB, KiB, MiB, GiB, TiB.
- Fractions are not accepted. Use `"1536 MiB"` for exactly 1.5 GiB.

Times are integer seconds. Omitted settings use defaults; numeric `0` also
means the default, not unlimited. Unknown settings and invalid values are rejected.

`max_payload_bytes` multiplied by `max_concurrent` must not exceed 2 GiB.
This bounds buffered clipboard payloads; allocation and other process memory
add overhead. Each concurrent drop can use up to `max_drop_bytes` of disk space.
Successfully received files remain until you remove them.

### Config paths and changes

With `XDG_CONFIG_HOME` set, the default file is
`$XDG_CONFIG_HOME/clipd/config.json`. Use `-config <path>` or `CLIPD_CONFIG` to
select another file. Setup and foreground operation do not create a config file.

For service installation, `address` and `drop_dir` must be absolute paths or
begin with `~/`. Relative paths in `clipd serve` resolve from its working directory.

Settings are read at startup. After editing, run `clipd restart` on macOS.
On Linux, stop and restart `clipd serve`.

## Managing clipd

Run these on the receiving computer:

```sh
clipd status
clipd restart          # macOS login service
clipd help
clipd help config
```

`clipd status` checks whether the receiver answers and shows the CLI version,
socket permissions, configured limits, and config and drop paths. On macOS,
it also shows the login service's state, PID, binary, plist and log paths.
Limits come from the selected config file, not from the running daemon.
For the macOS service, status warns when that file has changed since startup.

Install and restart wait up to five seconds for the receiver to answer.
A failed check exits 1 and prints the log path; it leaves the service installed.
The check does not modify the clipboard or send files, and does not verify a
remote SSH forward.

Restart uses the installed service's configuration, regardless of `-config` or
`CLIPD_CONFIG` in your shell.

## Updating

Update the receiving computer first, then refresh each remote function.

### Release download

Replace the executable using the [installation instructions](#macos), then run:

```sh
/usr/local/bin/clipd install
```

This reinstalls and starts the macOS login service, preserves configuration
values, and checks that the receiver responds.

### Source checkout

From your existing clipd source checkout:

```sh
git pull --ff-only
make install
/usr/local/bin/clipd install
```

On Linux, replace the binary or run `make install`, then stop and restart
`clipd serve` instead of running `clipd install`.

### Remote functions

On the receiving computer, run this for each host:

```sh
clipd setup <host>
```

Then open a new SSH connection as described in [setup](#set-up-a-remote-host).
Installing a local binary does not update functions already installed remotely.
Older functions that cannot send streaming archives will ask you to rerun setup.

Existing numeric size settings still work. Installation writes exact readable
sizes where possible. Before downgrading to a binary older than v1.1.0, convert
size strings back to numeric bytes in the config.

## Troubleshooting

Start on the receiving computer:

```sh
clipd status
```

For the macOS login service, check `~/Library/Logs/clipd/clipd.log`.
Foreground `clipd serve` writes logs to its terminal.

| Symptom | What to check |
|---|---|
| `clipd: command not found` remotely | Run setup and open a new shell. For direct SSH commands, see [Commands over SSH](#commands-over-ssh). |
| `clipd: no SSH forward at ...` | Connect using the destination passed to setup. Check for `ClearAllForwardings=yes` or an old shared connection. |
| Connection refused or `remote port forwarding failed` | See [Leftover sockets](#leftover-sockets). |
| `clipd: removed the unused socket at ...` | Log out and reconnect so SSH can create the forward. |
| Drop exceeds a size or file-count limit | Change the setting named in the error on the receiving computer, then restart it. |
| `clipd: could not confirm the copy.` or `clipd: could not confirm the drop.` | Check receiver status and logs. For drops, check the destination before retrying; files may have arrived. |

If output is too large for the clipboard limit, save it as a file with
`command | clipd drop --name output.txt`.

### Leftover sockets

sshd normally leaves `~/.clipd/socket` behind when a session ends and refuses
to replace it on the next login. A live session may also already hold that path.

When clipd detects a stale socket, it removes it and asks you to reconnect.
Re-running setup applies the same check. A listener that accepts connections
is preserved even if the receiving daemon is unavailable. On macOS remotes,
`nc`'s silent failure is a heuristic for refusal; see [SECURITY.md](SECURITY.md).

If you use connection sharing, close the old connection with
`ssh -O exit <host>` before reconnecting. This closes its shared sessions too.

### Optional server setting

On servers you administer, setting `StreamLocalBindUnlink yes` in the SSH
**server's** configuration lets sshd replace existing sockets when creating
forwards. This is server-wide and is not required for clipd. A new forward can
take over the socket path from an existing session.

Add this to the server's `/etc/ssh/sshd_config` or a configuration file it includes:

```text
StreamLocalBindUnlink yes
```

Validate the configuration on the server with `sudo sshd -t`, then reload the
SSH service using your operating system's service manager. Check the effective
setting with `sudo sshd -T | grep -i streamlocalbindunlink`.

On shared or work servers, ask the administrator. Setting this in your local
`~/.ssh/config` does not affect remote socket binding. The server must allow
UNIX socket forwarding for clipd to work.

### Missing tools

Setup needs a UNIX-socket-capable `nc` or `socat`. On Linux, `netcat-openbsd`
supports `-U` and `-N`; `netcat-traditional` does not support `-U`. macOS `nc`
uses different flags, which setup handles. Setup reports missing tools but does
not install packages. Without `tar`, clipboard copies and named stdin drops
still work.

Fish, csh and tcsh cannot load the function. Follow setup's instructions to
open `sh` and source the installed function.

## How it works

The receiving computer runs the daemon. A generated shell function on the
remote host sends data through an SSH-forwarded UNIX socket:

```text
Remote host                         Receiving computer

clipd function ──▶ ~/.clipd/socket ══ SSH ══▶ clipd daemon
                                                ├──▶ clipboard
                                                └──▶ ~/Drop
```

Text is passed to the local clipboard tool. File and folder drops stream a tar
archive; named stdin drops send one file's bytes. Files are staged privately
inside the drop directory and published after the request validates.
Publication of multiple names is not crash-atomic.

The Go module has no third-party dependencies. Remote hosts need Linux, macOS
or a BSD, a POSIX shell, compatible `nc` or `socat`, and OpenSSH 6.7 or newer.
File and folder drops also need `tar`. Windows, Dropbear and older OpenSSH
versions are not supported.

### SSH setup details

Setup writes a marked function block into `~/.bashrc`, `~/.zshrc` or `~/.profile`
on the remote host and a marked forward block into your local `~/.ssh/config`.
Running setup again replaces those blocks while preserving surrounding content.
The first backup of each existing file is kept with a `.clipd-backup` suffix.
It is a recovery copy from before the first change, not a current snapshot.

Preview the blocks with:

```sh
clipd setup -print <host>
```

This connects to inspect the host but does not write the proposed changes.

Aliases, hostnames, IPv4 addresses and `user@hostname` are accepted. For IPv6,
use an SSH alias with the address as its `HostName`. Set up each destination
spelling you use: an alias and its full hostname need separate blocks.

Setup scopes each forward to the account it connected to. Different accounts
on the same server are configured separately. If an alias changes accounts,
rerun setup.

### Commands over SSH

A command sent directly through SSH may not load the startup file containing
clipd. For Bash or Zsh, request an interactive shell explicitly:

```sh
ssh <host> 'bash -ic "clipd drop report.pdf"'
ssh <host> 'zsh -ic "clipd drop report.pdf"'
```

Use the command for the remote shell you configured. For a filename with spaces:

```sh
ssh <host> "bash -ic 'clipd drop \"report final.pdf\"'"
```

This loads the entire interactive startup file, including any output or tmux
startup commands, and may write shell history. Without a terminal, Bash may
print a job-control warning. `ssh -t` supplies a terminal but does not make
the shell interactive by itself; avoid it for piped binary input.

On Debian, Bash's usual `.bashrc` guard skips the function in non-interactive
shells. Moving the clipd block above that return is another option. Zsh loads
`.zshrc` for interactive shells; a custom `ZDOTDIR` may point somewhere other
than the `~/.zshrc` file setup writes.

### Exit codes

```text
0   success
1   the receiver did not answer, or an operation failed
4   configuration error
64  usage error
```

## Security

SSH encrypts forwarded traffic, verifies the host key and authenticates your
account. Programs running as your user on a forwarded host can use that forward
to write to your clipboard or send files to your computer. Only forward clipd
to accounts whose programs you trust with that access.

The daemon listens on a UNIX socket with mode `0600`. The remote socket is in
a `0700` directory; its permissions are controlled by sshd, whose default bind
mask is `0177` (socket mode `0600`). Client-side bind-mask settings do not control
a remote forward.

Received files have mode `0600`, directories `0700`. Files are not made
executable, and macOS quarantine is applied where possible. Drop paths are
validated and existing files are not overwritten. Connections, concurrent work,
payloads and transfer time are bounded.

See [SECURITY.md](SECURITY.md) for the security boundary and known limitations.

## Uninstall

On macOS:

```sh
clipd uninstall
```

This stops the receiver and removes its login service. On Linux, stop
`clipd serve`. The executable, config, logs, received files and SSH setup remain.

### Files and locations

Default local paths on macOS:

| Path | Purpose |
|---|---|
| `/usr/local/bin/clipd` | executable |
| `~/Library/LaunchAgents/com.clipd.agent.plist` | login service, removed by `clipd uninstall` |
| `~/.config/clipd/config.json` | configuration, written by `clipd install` |
| `~/.clipd.sock` | daemon socket, removed on clean shutdown |
| `~/Drop/` | received files |
| `~/Drop/.clipd-stage-*` | private staging directories; removed after completion, or cleaned when stale |
| `~/Library/Logs/clipd/` | service logs |
| `~/.ssh/config` | managed forward blocks |
| `~/.ssh/config.clipd-backup` | SSH config from before the first change |

Custom install, config, socket and drop paths may differ. After uninstalling
the service, remove the default executable, config, logs and socket with:

```sh
sudo rm /usr/local/bin/clipd
rm -rf ~/.config/clipd ~/Library/Logs/clipd ~/.clipd.sock
```

Delete the marked clipd blocks from `~/.ssh/config` by hand. Keep or remove
backups as you choose. Review the files in `~/Drop` before deleting them.

On each remote host:

| Path | Purpose |
|---|---|
| `~/.bashrc`, `~/.zshrc` or `~/.profile` | function between `# >>> clipd >>>` and `# <<< clipd <<<` |
| matching `.clipd-backup` | startup file from before the first change |
| `~/.clipd/` | private socket directory |
| `~/.clipd/socket` | socket created by sshd; may remain after logout |

Delete the marked function block. After closing sessions using its forward,
remove `~/.clipd`. If you added the optional server setting, remove it from the
server configuration and reload sshd to undo that change.

## Development

Run `make check` for formatting, vet and race tests. CI requires sh, dash,
Bash, Zsh, OpenBSD nc (macOS nc on Macs), and socat, installing missing test
tools so both supported transports are exercised.

For local macOS development, `make reinstall` builds and installs the executable,
requests a service restart, and prints version and status information.

`CLIPD_TEST_SSHD=1 go test ./cmd/clipd -run TestSSHForwardLifecycle -v`
starts a temporary loopback-only sshd with temporary keys and configuration.
It tests transfer, stale-socket recovery, reconnect, and a live forward with
the receiver down. It opens no remote shell and does not install a service or
edit SSH configuration. Run it on a disposable runner with sshd available.

`go test ./internal/drop -run '^$' -bench BenchmarkExtract -benchtime=3x`
compares an 8 MiB file with 256 files totaling 8 MiB, including file sync and
macOS quarantine. `-bench BenchmarkQuarantine` measures the native quarantine
helper separately on macOS. All output files are temporary.

## License

[MIT](LICENSE).

Inspired by [wincent/clipper](https://github.com/wincent/clipper), with file drops
and SSH setup.
