# clipd

[![CI](https://github.com/colefailla/clipd/actions/workflows/ci.yml/badge.svg)](https://github.com/colefailla/clipd/actions/workflows/ci.yml)

Send command output and files from a remote machine to your Mac, over the SSH
connection you already have.

```bash
ssh debian
tail -50 error.log | clipd        # now in the Mac's clipboard
clipd drop report.pdf                    # now in the Mac's ~/Drop
pg_dump mydb | clipd drop --name db.sql  # output that never touched disk
```

**No program is installed on the remote machine.** `clipd` there is a shell
function in your `~/.bashrc` (or `~/.zshrc`, `~/.profile`) that pipes into
`nc`, which every Unix box already has. The daemon runs only on the Mac.

## What it supports

| | |
|---|---|
| **Receiving machine** | macOS. A Linux desktop also works, with `wl-copy`, `xclip` or `xsel` for the clipboard. |
| **Remote machines** | Linux, macOS and the BSDs, with a POSIX shell, `nc` or `socat`, and `tar` for sending files and folders. |
| **SSH** | OpenSSH 6.7 or newer on the remote (2014 onward), which can forward a socket. |
| **Not supported** | Windows, which has no POSIX shell for the remote function. Dropbear, and OpenSSH older than 6.7, which cannot forward a socket. |

On a Linux desktop, run `clipd serve` to start the daemon. `clipd install`,
which starts it at login, writes a macOS LaunchAgent and only works on a Mac.

## How it works

```text
Linux                                      macOS

  tail -50 error.log
     │ stdout
     ▼
  clipd ──▶ ~/.clipd/socket ══ ssh -R ══▶ ~/.clipd.sock ──▶ clipd serve
  (a shell function)                                              │
                                                                  ▼
                                                          pbcopy ──▶ clipboard
                                                          tar    ──▶ ~/Drop
```

SSH's `RemoteForward` makes the Mac's socket appear on the remote host. Anything
that can write bytes to it — `nc`, `socat`, an editor, a shell function — can
reach the daemon.

[OSC 52](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html) does the
clipboard half through a terminal escape sequence and is simpler when it works.
clipd is for the cases where it doesn't: Terminal.app has no support, several
terminals silently truncate large payloads, and no escape sequence sends files.

## Prior art

[wincent/clipper](https://github.com/wincent/clipper) does the clipboard half,
has done for years, and is excellent. If you only need text on your clipboard,
use it — it is smaller, more mature, and packaged everywhere.

clipd differs in three ways:

- **`clipd drop`** sends files. Clipper is text only.
- **`clipd setup <host>`** probes the remote host and writes the shell function
  and SSH config for you. Clipper leaves you to hand-wire the alias, which is
  where most of the sharp edges are.
- **The daemon replies.** A copy prints `clipd: ok: copied 47 bytes` instead of
  succeeding silently or hanging with no output.

The structured-frame design, the stale-socket recovery and the umask handling
are all taken from clipper. It solved these problems first.

## Install

On the **Mac only** — there is nothing to install on the machines you copy from.

From a release binary (Apple Silicon; use `clipd_darwin_amd64` on Intel):

```bash
curl -fsSL https://github.com/colefailla/clipd/releases/latest/download/clipd_darwin_arm64 -o clipd
sudo install -m 0755 clipd /usr/local/bin/clipd
rm clipd
```

Or from source, which needs only a Go toolchain — clipd has no dependencies:

```bash
git clone https://github.com/colefailla/clipd && cd clipd && make install
```

Then start it at login:

```bash
clipd install
```

There is no Homebrew formula yet.

To update a source install later:

```bash
git pull && make reinstall
```

`make reinstall` builds, installs, restarts the daemon and shows its status in
one step. Then rerun `clipd setup <host>` for each host, so its shell function
matches the new daemon.

## Setup a remote host

```bash
clipd setup debian
```

That connects to `debian`, works out what it has, installs a `clipd` shell
function in the right rc file, and adds the socket forward to your
`~/.ssh/config`. Use `-print` to see what it would do without doing it.

Reconnect for the forward to take effect:

```bash
ssh -O exit debian 2>/dev/null; ssh debian
```

Run it once per host you use. Both halves — the remote function and the local
`Host` block — are written together, so hosts you have not re-run it on keep
working as they were; they just do not get the newer behaviour until you do.

Setting up two accounts on the same machine works: `alice@server` and
`bob@server` get separate blocks, scoped with `Match ... user`, rather than the
second replacing the first.

### Leftover sockets

sshd creates `~/.clipd/socket` on the remote when you log in, but does not
delete it when you log out. By default it also refuses to replace one that is
already there, so the next login prints `remote port forwarding failed` and has
no clipboard. Clipper has the same problem.

clipd handles this without any server changes. The first time you use `clipd`
in a session like that, it checks that nothing is listening on the old socket,
removes it, and tells you to log out and back in. Re-running `clipd setup` from
the Mac clears it too.

**On a server you run**, one sshd setting makes the problem go away: sshd
replaces the leftover itself. Run this once from the Mac; it asks for your sudo
password on the server:

```bash
ssh -t debian "echo 'StreamLocalBindUnlink yes' | sudo tee /etc/ssh/sshd_config.d/clipd.conf && { sudo systemctl reload ssh || sudo systemctl reload sshd; }"
```

`tee` is there because the file belongs to root: `sudo echo … > file` would
fail, since the `>` runs as you. If the server's `sshd_config` has no `Include`
for `sshd_config.d`, add the line to `/etc/ssh/sshd_config` itself. To check it
took, run `sudo sshd -T | grep -i streamlocalbindunlink` on the server.

**On a server someone else runs**, such as a work machine, leave sshd alone. It
is a server-wide setting and their call. clipd still works; you just reconnect
once in a while. If the administrators have turned socket forwarding off
(`AllowStreamLocalForwarding no`), clipd cannot work there, by their choice.

### Why it probes

`nc` is not one program. Several unrelated implementations share the name, they
disagree about which flags exist, and — worse — they disagree about what the
same flag *means*. Guessing wrong gives you `invalid option`, or a copy that
hangs with no output, or a flag error on every single use.

So setup asks the host what it has, and writes a client to match:

| Remote | What setup finds | What it writes |
|---|---|---|
| Linux, `netcat-openbsd` | `-U` and `-N` | `nc -N -U "$sock"` |
| macOS | `-U`, and a `-N` that means something else | `nc -U "$sock"` |
| any, with `socat` | no usable `nc` | `socat - UNIX-CLIENT:"$sock"` |
| `netcat-traditional` only | no `-U` | refuses, names the package to install |

The two flags that matter:

- **`-U`** talks to a UNIX socket instead of a network address. `netcat-traditional`
  does not have it, which is why that host needs `socat` or a different netcat.
- **`-N`** closes the sending half after stdin ends. Without it the daemon never
  sees the end of your message, so it never replies, and the copy hangs until it
  times out.

macOS is the case worth spelling out. Its netcat **has** a `-N`, but there it
takes a probe count for a write timeout — passing it the OpenBSD way fails with
`invalid tcp adaptive write timeout value`. It also closes on stdin EOF without
being asked, so it needs no flag. A probe that checked only whether `-N` existed
would produce a broken client for every Mac; setup checks `uname -s` too.

Nothing is ever installed for you. When the host genuinely cannot do it, setup
refuses and names the package rather than writing a client that fails later.

Both edits are bracketed by clipd markers, so re-running replaces the block
rather than adding another, and uninstalling means deleting between them. The
SSH config markers name the host — `# >>> clipd: debian >>>` — so setting up
several hosts leaves each one's forward intact. Your SSH config is backed up
before the first edit.

## Usage

On the remote host:

```bash
ls -l | clipd                       # copy stdout
clipd notes.txt                     # copy a file's contents
cat ~/.ssh/id_ed25519.pub | clipd   # grab a public key

clipd drop report.pdf                        # send a file to ~/Drop
clipd drop src/*.go                          # send several
clipd drop photos                            # send a folder, structure kept
pg_dump mydb | clipd drop --name db.sql      # send output as a file
journalctl -u nginx | clipd drop --name nginx.log
```

Every command prints the Mac's answer, `clipd: ok: …` or `clipd: error: …`, and
exits non-zero on anything but `ok`. So `clipd drop x && rm x` deletes `x` only
if it actually arrived.

`--name` matters when the thing you want has no file on disk. With arguments,
`tar` carries the names; in a pipeline there is nothing to name, so you supply
one. The flag is required rather than guessed from whether stdin is a terminal,
so a drop with a pipe or a redirect on stdin sends the file rather than an empty
one.

In a pipeline, `clipd` cannot see whether the command before it failed. Use
`set -o pipefail` in bash or zsh if the pipeline's exit status should reflect
that.

Slow commands are fine. `clipd` waits as long as the command takes to produce
its output, up to the transfer time limit (30 minutes by default). A bare
`clipd` with nothing piped in waits for you to type; Ctrl-C ends it.

### What lands in ~/Drop

- A file arrives under its own name: `clipd drop report.pdf` gives
  `~/Drop/report.pdf`.
- A folder arrives with its structure: `clipd drop photos` gives
  `~/Drop/photos/…`.
- `clipd drop photos/*` sends what your shell expands that to (normally skipping
  hidden files), straight into `~/Drop`.
- Nothing is overwritten or merged. A second `report.pdf` becomes
  `report-1.pdf`; a second `photos` folder becomes `photos-1`.
- Files are readable only by you and never executable, and get macOS's
  quarantine attribute like a download.
- Only regular files and folders are sent. Symlinks, hard links, devices and
  the like are skipped, as `rsync` does by default, and the reply says how many:
  `…; skipped 3 symlinks or other special files`. A symlink is never followed,
  so it cannot pull in anything outside the folder you named.

A drop is all or nothing. Files are received into a hidden staging folder inside
`~/Drop` and appear under their real names only once everything has arrived and
checked out, so a transfer that fails partway leaves nothing behind. In the rare
case that the connection drops just after the files were published, `clipd`
reports a failure even though they arrived, so check `~/Drop` before re-sending.

### Progress

In a terminal, drops show progress on stderr: bytes, percentage of the current
file, speed, elapsed time and time remaining. Piped input shows bytes and time
only, because its size is unknown. `clipd drop --no-progress …` hides it, and it
is off whenever stderr is not a terminal.

### Big files

Each drop is limited to 256 MiB and 256 files by default, and each transfer to
30 minutes. Going over tells you which limit you hit and what to change:

```text
clipd: error: drop is larger than the 256 MiB limit; raise max_drop_bytes in the Mac's clipd config, then run clipd restart
```

To send something like a 20 GB video, change that line in the Mac's
`~/.config/clipd/config.json` to 20 GiB, then restart the daemon:

```json
"max_drop_bytes": 21474836480,
```

```bash
clipd restart
```

On a slow link, raise `max_transfer_seconds` as well: 20 GB at 10 MB/s takes
about 35 minutes, and running out of time says so in the same way. The Mac needs free disk space for the whole drop while it
arrives. See [Configuration](#configuration) for all the limits.

The clipboard has its own, smaller limit (`max_payload_bytes`, 10 MiB) because
it is held in memory. For anything bigger, `… | clipd drop --name file` sends it
as a file instead.

### Non-interactive commands

`ssh <host> 'clipd drop file'` runs your shell with `-c`, which is never
interactive, even with `ssh -t`. Bash reads `.bashrc` for commands sent over
SSH, but Debian's `.bashrc` returns early for non-interactive shells, and
`clipd setup` appends below that line. So on Debian the function is never
defined and you get `clipd: command not found`. zsh reads `.zshrc` only when
interactive, so a zsh host behaves the same way.

Ask for an interactive shell explicitly:

```bash
ssh debian 'bash -ic "clipd drop report.pdf"'
ssh zsh-host 'zsh -ic "clipd drop report.pdf"'
```

A file name with spaces needs one more layer of quoting:

```bash
ssh debian "bash -ic 'clipd drop \"report final.pdf\"'"
```

What `-i` costs:

- Without a terminal, bash prints `bash: no job control in this shell`. It is
  harmless; add `ssh -t` if you would rather not see it.
- Your whole interactive startup file runs, so anything it prints, waits for or
  starts (a tmux session, another shell) happens first.
- The command may be saved to your shell history, depending on your settings.
- The zsh form assumes `$ZDOTDIR` is unset, since setup writes `~/.zshrc`.

Use `ssh -t` only for commands you would type. It turns the remote stdin into a
terminal, which can corrupt binary data piped through `ssh`.

Alternatively, move the clipd block above the early return in `.bashrc`; then
plain `ssh <host> 'clipd drop file'` works.

### After upgrading

After updating clipd on the Mac, rerun `clipd setup <host>` for each host to
update its shell function. Update the Mac first: a new function needs a new
daemon, and an old daemon refuses its requests with an error rather than
mistaking them for clipboard text. A host you have not rerun setup on keeps
working, with one exception: a function from before folders kept their
structure can no longer send files or folders with `clipd drop`, and gets an
error telling you to rerun `clipd setup` for that host. Copying and
`clipd drop --name` still work there.

## Security

**The daemon listens only on a private UNIX socket.** It reaches another machine
only through your SSH connection: SSH encrypts the channel, verifies the host
key against `known_hosts` and authenticates you, and the socket's `0600`
permissions decide who on that machine may write to it.

On the remote host that socket lives in `~/.clipd/`, a `0700` directory, and
`clipd setup` pins the bind mask that creates it `0600`. Two layers rather than
one because OpenSSH documents that not every operating system honours the mode
on a socket file — every one of them honours the mode on a directory.

clipd has no token and no TLS of its own because SSH has already done both
jobs. A token would also be worse: it would live as a file on the remote host,
and anyone who copied it could use it from anywhere until you rotated it. The
socket only works while your SSH connection is up.

**What this means:** anything running as you on the remote host can write to
your clipboard and send you files, including a build script or a package
install. Other accounts on that host cannot, because of the socket's
permissions.

There is no way around this. Letting a remote machine write to your clipboard
means trusting what runs there as you. In practice anything able to abuse it
already has your files and your shell history on that machine.

What limits the damage:

- **Bracketed paste**, on by default in modern shells, means pasted text ending
  in a newline is not executed until you press Enter. This is a guard against
  the accident, not a security boundary: it is the terminal's behaviour rather
  than clipd's, and anything that can write to the socket can still put whatever
  it likes on the clipboard.
- **Drops stay inside `~/Drop`.** Paths are checked, nothing is overwritten or
  made executable, and files get macOS quarantine where possible.
- **Everything has a limit.** Sizes, file counts, simultaneous connections and
  transfer time are all bounded, so a misbehaving sender cannot exhaust the
  Mac's memory or tie the daemon up indefinitely.

The exact guarantees, and their known exceptions, are in
[SECURITY.md](SECURITY.md).

## Configuration

`~/.config/clipd/config.json`, the same path on macOS and Linux;
`$XDG_CONFIG_HOME` wins when it is set. Every setting has a working default, so
the file is optional. `clipd install` writes one with every setting filled in.

| Setting | Default | What it controls |
|---|---|---|
| `address` | `~/.clipd.sock` | the daemon's socket path |
| `drop_dir` | `~/Drop` | where dropped files land |
| `max_payload_bytes` | `10485760` (10 MiB) | largest clipboard copy; up to 1 GiB |
| `max_drop_bytes` | `268435456` (256 MiB) | largest drop, all files together; up to 1 TiB |
| `max_drop_files` | `256` | most files in one drop; up to 65536 |
| `max_concurrent` | `8` | copies and drops handled at once; up to 64 |
| `max_transfer_seconds` | `1800` (30 minutes) | longest one transfer may take; up to 86400 (24 hours) |

**Sizes are in bytes and times in seconds.** Common values:

| | |
|---|---|
| 50 MiB | `52428800` |
| 1 GiB | `1073741824` |
| 10 GiB | `10737418240` |
| 100 GiB | `107374182400` |
| 1 hour | `3600` |

`clipd status` prints them back in human terms. Leaving a setting out, or
setting it to `0`, means the default, never unlimited. Unknown settings and
negative numbers are rejected, so a typo fails loudly.

Two limits interact. The clipboard is held in memory, so `max_payload_bytes`
times `max_concurrent` must fit in 2 GiB. Drops go to disk, and simultaneous
drops can each use up to `max_drop_bytes` of it.

**The config is read once, when the daemon starts.** After editing:

```bash
clipd restart
```

`clipd status` says `EDITED since the daemon started` when the file has changed
under a running daemon, so a forgotten restart shows up rather than looking like
a setting that did nothing.

`CLIPD_CONFIG` is the only environment variable clipd reads.

## Troubleshooting

Start on the Mac:

```bash
clipd status
```

It asks the daemon to identify itself rather than just checking the file
exists, because a daemon killed with `SIGKILL` leaves the socket behind and a
stale one looks identical in a directory listing. Exit code 0 means a clipd
daemon answered — not merely that something is listening.

**`clipd: removed the unused socket at …/.clipd/socket`**: sshd left the socket
from a previous login, so this session has no forward. `clipd` has already
removed it; log out and back in. See [Leftover sockets](#leftover-sockets) to
stop it happening.

**`clipd: this SSH session has no clipd forward`**: this session has no forward. You
connected in a way that skips clipd's SSH config block (a different host name,
`-o ClearAllForwardings=yes`), or reused a `ControlMaster` connection opened
before setup. Log out and back in; with ControlMaster, close the old master
first with `ssh -O exit <host>`.

**`Warning: remote port forwarding failed`** when you log in: you are logged in,
but this session could not create the socket. Either a leftover socket is in the
way (`clipd` removes it the first time you use it; then reconnect), or another
session to the same host already holds it. In that case `clipd` works through
the other session's forward for as long as that session stays open. With
`StreamLocalBindUnlink yes` on the server, the newest session takes the socket
over instead.

**`clipd: couldn't confirm the transfer`**: the forward is up but the Mac did
not answer. Run `clipd status` on the Mac. The Mac's log,
`~/Library/Logs/clipd/clipd.log`, records why a request was rejected.

**`clipd: error: … limit`**: see [Big files](#big-files).

`StreamLocalBindUnlink yes` in your own `~/.ssh/config` does **not** help: a
remote forward is bound by the remote's `sshd`, and the request the client sends
carries only a path, with no way to ask for an unlink. That was measured against
a real host — the option active in `ssh -G`, a stale socket in place, and the
forward still refused.

## Exit codes

```text
0   success
1   the daemon is not running, or an operation failed
4   configuration error
64  usage error
```

## Uninstall

```bash
clipd uninstall
```

That unloads and removes the LaunchAgent. Everything else is left in place.

### Everything clipd touches

**On the Mac:**

| Path | What it is |
|---|---|
| `/usr/local/bin/clipd` | the binary |
| `~/Library/LaunchAgents/com.clipd.agent.plist` | the launch agent, removed by `clipd uninstall` |
| `~/.config/clipd/config.json` | configuration |
| `~/.clipd.sock` | the socket, created by the daemon and removed when it stops |
| `~/Drop/` | files received by `clipd drop` |
| `~/Library/Logs/clipd/` | the daemon's output, written by launchd |
| `~/.ssh/config` | one block per host, between `# >>> clipd: <host> >>>` markers |
| `~/.ssh/config.clipd-backup` | a copy of the SSH config from before the first edit |

To remove the rest:

```bash
sudo rm /usr/local/bin/clipd
rm -rf ~/.config/clipd ~/Library/Logs/clipd ~/.clipd.sock
```

Delete the marked blocks from `~/.ssh/config` by hand. `~/Drop` holds files you
received, so it is left for you to look through.

**On each remote host**, no program is installed; these are the only changes:

| Path | What it is |
|---|---|
| `~/.bashrc`, `~/.zshrc` or `~/.profile` | the `clipd` function, between `# >>> clipd >>>` markers |
| `~/.bashrc.clipd-backup` (or the matching rc file) | a copy of that file from before the first edit |
| `~/.clipd/` | a `0700` directory, created by `clipd setup`, holding the socket |
| `~/.clipd/socket` | created by sshd when you connect; sshd leaves it behind afterwards, and clipd removes it once it is stale |
| `/etc/ssh/sshd_config.d/clipd.conf` | only if you added the [optional sshd setting](#leftover-sockets) |

Delete the marked block, then `rm -rf ~/.clipd`.

## License

MIT
