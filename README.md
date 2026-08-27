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

On the **Mac only**:

```bash
brew install clipd
```

Or download a binary:

```bash
curl -fsSL https://github.com/colefailla/clipd/releases/latest/download/clipd_darwin_arm64 -o clipd
sudo install -m 0755 clipd /usr/local/bin/clipd
rm clipd
```

Then start it at login:

```bash
clipd install
```

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

The probe exists because `nc` is not one program. The OpenBSD build speaks UNIX
sockets with `-U` and half-closes with `-N`; the "traditional" build shipped by
default on some Debian systems does neither, and guessing wrong gives you either
`invalid option` or a copy that hangs with no output. Asking the host removes
the guess — and if it genuinely can't, the error names the package that fixes it.

Both edits are bracketed by `# >>> clipd >>>` markers, so re-running replaces
the block rather than adding another, and uninstalling means deleting between
the markers. Your SSH config is backed up before the first edit.

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

That is why clipd has no token and no TLS of its own. Both would duplicate a
decision SSH has already made — and a token stored on the remote host would be
a stealable secret that works from anywhere until rotated, where the socket is
an ephemeral capability that dies with the session and cannot be copied off the
box.

**What this does mean:** anything running as you on the remote host can write to
your clipboard and send you files. Other accounts there cannot, because of the
socket's permissions, but your own processes can — including a build script or a
package install. That is inherent to letting a remote machine write to your
clipboard at all. It also matters less than it sounds: anything positioned to
abuse it already has your files, your history and your keystrokes on that
machine.

Two things limit what it can do:

- **Bracketed paste**, on by default in modern shells, means pasted text ending
  in a newline is not executed until you press Enter.
- **Dropped files** are flattened to basenames so an archive cannot write
  outside the drop directory, are never overwritten, never made executable, and
  carry macOS's quarantine attribute so Gatekeeper treats them like downloads.
  Symlinks, hard links and device nodes in an archive are skipped.

`address` also accepts a **loopback** `host:port`, for hosts whose SSH cannot
forward a UNIX socket — OpenSSH gained that ability only in 6.7, and its Windows
build still lacks it. That is a tunnel endpoint like the socket, not a network
service: a reachable address is **refused at startup**, not warned about,
because nothing here authenticates and no warning makes that safe.

A port gives up one thing against a socket: any user on the machine can reach
loopback, where a `0600` socket admits only its owner. Prefer the socket
wherever SSH can forward one.

## Configuration

`~/Library/Application Support/clipd/config.json`. Every value has a working
default, so a daemon with no config file is a working daemon.

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

Removes the LaunchAgent. The config, logs and the shell functions on remote
hosts are left in place — delete the block between the clipd markers in their
rc files.

## License

MIT
