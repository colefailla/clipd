# clipd

[![CI](https://github.com/colefailla/clipd/actions/workflows/ci.yml/badge.svg)](https://github.com/colefailla/clipd/actions/workflows/ci.yml)

Send command output and files from a remote machine to your Mac, over the SSH
connection you already have.

```bash
ssh debian
tail -50 error.log | clipd        # now in the Mac's clipboard
clipd drop report.pdf             # now in the Mac's ~/Drop
pg_dump mydb | clipd drop db.sql  # output that never touched disk
```

**Nothing is installed on the remote machine.** `clipd` there is a shell
function that pipes into `nc`, which every Unix box already has. The daemon
runs only on the Mac.

## How it works

```text
Linux                                      macOS

  tail -50 error.log
     │ stdout
     ▼
  clipd ──▶ ~/.clipd.sock ══ ssh -R ══▶ ~/.clipd.sock ──▶ clipd serve
  (a shell function)                                            │
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
- **The daemon replies.** A copy prints `clipd: copied 47 bytes` instead of
  succeeding silently or hanging with no output.

The structured-frame design, the stale-socket recovery, the umask handling and
the path-or-host address are all taken from clipper. It solved these problems
first.

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
clipd < notes.txt                   # copy a file's contents
cat ~/.ssh/id_ed25519.pub | clipd   # grab a public key

clipd drop report.pdf               # send a file to ~/Drop
clipd drop src/*.go                 # send several
pg_dump mydb | clipd drop db.sql    # send output as a file
journalctl -u nginx | clipd drop    # name invented from the clock
```

The last two forms matter when the thing you want has no file on disk. With
arguments, `tar` carries the names; in a pipeline there is nothing to name, so
you supply one — or let the daemon build `drop-20260826-143022.bin` rather than
lose the bytes.

Content is sent byte for byte — newlines, tabs and the trailing newline are
preserved. Input over `max_payload_bytes` (10 MiB by default) is rejected rather
than truncated.

Dropped files are **flattened**: everything lands directly in `~/Drop` under its
own basename, with no subdirectories recreated. Files are never overwritten; a
second `report.pdf` arrives as `report-1.pdf`.

## Security

**Nothing listens on the network.** The daemon binds a UNIX socket in your home
directory. There is no port to scan, and nothing on your local network can reach
it — on public wifi or anywhere else.

The socket reaches another machine only when you forward it over SSH. By then
SSH has encrypted the channel, verified the host key against `known_hosts`, and
authenticated you; the socket's `0600` permissions decide who on that machine
may write to it.

clipd has no token and no TLS of its own because SSH has already done both
jobs. A token would also be worse: it would live as a file on the remote host,
and anyone who copied it could use it from anywhere until you rotated it. The
socket cannot be copied, and it disappears when the session ends.

**What this means:** anything running as you on the remote host can write to
your clipboard and send you files, including a build script or a package
install. Other accounts on that host cannot, because of the socket's
permissions.

There is no way around this. Letting a remote machine write to your clipboard
means trusting what runs there as you. In practice anything able to abuse it
already has your files and your shell history on that machine.

Two things limit what it can do:

- **Bracketed paste**, on by default in modern shells, means pasted text ending
  in a newline is not executed until you press Enter.
- **Dropped files** are flattened to basenames so an archive cannot write
  outside the drop directory, are never overwritten, never made executable, and
  carry macOS's quarantine attribute so Gatekeeper treats them like downloads.
  Symlinks, hard links and device nodes in an archive are skipped.

`address` also takes a `host:port` instead of a socket, for remotes whose SSH
cannot forward one — OpenSSH before 6.7, and Windows. It must be loopback;
anything reachable is refused at startup.

Prefer the socket wherever SSH can forward one. A loopback port is reachable by
any user on the machine, where a `0600` socket is not.

## Configuration

`~/.config/clipd/config.json` — the same path on macOS and Linux, and
`$XDG_CONFIG_HOME` wins when it is set. Every value has a working default, so a
daemon with no config file is a working daemon.

```json
{
  "address": "~/.clipd.sock",
  "drop_dir": "~/Drop",
  "max_payload_bytes": 10485760,
  "max_drop_bytes": 268435456,
  "max_drop_files": 256,
  "max_concurrent": 8
}
```

**Sizes are in bytes.** Common values:

| | Bytes |
|---|---|
| 10 MB | `10485760` |
| 50 MB | `52428800` |
| 256 MB | `268435456` |
| 1 GB | `1073741824` |

`clipd status` prints them back in human terms, so you write bytes and read
`10 MiB`.

**The config is read once, when the daemon starts.** After editing:

```bash
clipd restart
```

`clipd status` says `EDITED since the daemon started` when the file has changed
under a running daemon, so a forgotten restart shows up rather than looking like
a setting that did nothing.

`max_payload_bytes` is capped at 1 GB, because clipboard content is held in
memory. The drop limits have no ceiling — those stream to disk.

Unknown keys are rejected rather than ignored, so a typo fails loudly.
`CLIPD_CONFIG` is the only environment variable clipd reads.

Run `clipd help config` or `clipd help security` for detail.

## Troubleshooting

```bash
clipd status
```

It dials the socket rather than just checking the file exists, because a daemon
killed with `SIGKILL` leaves the socket behind and a stale one looks identical
in a directory listing. Exit code 0 means the daemon answered.

**`clipd: ~/.clipd.sock is missing`** on the remote — the forward isn't up.
Reconnect. If you use `ControlMaster`, kill the old master first with
`ssh -O exit <host>`; otherwise you reuse a connection that predates the
forward.

**`remote port forwarding failed`** — a stale socket on the remote from an
unclean disconnect. `ssh <host> rm .clipd.sock`, or set
`StreamLocalBindUnlink yes` in the remote's `/etc/ssh/sshd_config` to stop it
recurring.

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

**On each remote host** — nothing is installed, so there are two things:

| Path | What it is |
|---|---|
| `~/.bashrc`, `~/.zshrc` or `~/.profile` | the `clipd` function, between `# >>> clipd >>>` markers |
| `~/.clipd.sock` | created by sshd while you are connected, removed when you disconnect |

Delete the marked block and the socket is gone on its own.

## License

MIT
