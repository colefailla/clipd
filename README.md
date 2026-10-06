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

**Nothing is installed on the remote machine.** `clipd` there is a shell
function that pipes into `nc`, which every Unix box already has. The daemon
runs only on the Mac.

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
Setup also safely clears a confirmed stale remote socket so the next SSH
connection can bind the forward. Active listeners are preserved. If recovery
cannot be verified, setup reports failure and explains that its config files
were already updated; it does not silently claim the host is ready.

Connect normally:

```bash
ssh debian
```

Setup configures OpenSSH connection sharing (`ControlMaster auto` and
`ControlPersist yes`). The SSH connection and forward stay alive on the Mac
after shell logout. Later logins reuse them. Aliases with the same `HostName`,
port, user and jump host share the connection; other destinations stay separate.
No server configuration change or additional service is required.

Run it once per host you use. Both halves — the remote function and the local
`Host` block — are written together, so hosts you have not re-run it on keep
working as they were; they just do not get the newer behaviour until you do.
Destination names are matched as typed. To use both `ssh debian` and
`ssh debian.local`, run setup for both names; each gets its own forwarding block.
Different machines also keep separate blocks. SSH aliases, DNS names, literal
IPv4 and IPv6 addresses, and `user@host` destinations are supported. For example:

```bash
clipd setup nas.example
clipd setup 192.0.2.10
clipd setup cole@2001:db8::10
```

Setting up two accounts on the same machine works: `alice@server` and
`bob@server` get separate blocks, scoped with `Match ... user`, rather than the
second replacing the first.

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

### Hosts that cannot forward a socket

`clipd setup` only knows how to forward a socket, and refuses to run when the
daemon is set to a TCP address. Almost nothing needs the alternative: OpenSSH
has forwarded UNIX sockets since 6.7, released in 2014, so every current Linux
and macOS host is covered. The exceptions are Windows OpenSSH, which still
cannot, and systems old enough to predate 6.7.

Those are configured by hand. **Read the cost at the end before you do.**

1. While the daemon is still on a socket, print the function `setup` would
   install, to start from:

   ```bash
   clipd setup -print <host>
   ```

2. Point the daemon at a loopback port and restart it:

   ```json
   { "address": "127.0.0.1:8199" }
   ```

   ```bash
   clipd restart
   ```

3. Forward the port instead of the socket, in `~/.ssh/config`:

   ```text
   Host oldbox
     RemoteForward 8199 127.0.0.1:8199
   ```

4. Paste the function into the remote's rc file with two edits: replace every
   `nc -U "$_clipd_sock"` with `nc -N 127.0.0.1 8199` — drop the `-N` if the
   remote is macOS — and delete the `[ ! -S "$_clipd_sock" ]` check, which has
   no meaning for a port.

**The cost.** A socket is protected by its `0600` mode inside a `0700`
directory, so only you can write to it. A loopback port has no permissions at
all: every account on that host can reach port 8199 and put things on your
clipboard or drop files on your Mac. sshd binds remote forwards to loopback, so
it is not exposed to the network — but it is exposed to everyone logged into
that machine. Use it only where you would trust all of them.

## Usage

On the remote host:

```bash
ls -l | clipd                       # copy stdout
clipd < notes.txt                   # copy a file's contents
cat ~/.ssh/id_ed25519.pub | clipd   # grab a public key

clipd drop report.pdf                        # send a file to ~/Drop
clipd drop src/*.go                          # send several
pg_dump mydb | clipd drop --name db.sql      # send output as a file
journalctl -u nginx | clipd drop --name nginx.log
```

`--name` matters when the thing you want has no file on disk. With arguments,
`tar` carries the names; in a pipeline there is nothing to name, so you supply
one. The flag is required rather than guessed from whether stdin is a terminal,
so a drop with a pipe or a redirect on stdin sends the file rather than an empty
one.

One caveat about non-interactive use: Debian and its derivatives ship a
`.bashrc` that returns early for non-interactive shells, and `clipd setup`
appends below that line. So `ssh <host> 'clipd drop file'` reports
`clipd: command not found` on those hosts — the function is never defined. Use
`ssh -t <host> 'clipd drop file'`, which allocates a terminal and makes the shell
interactive, or move the clipd block above that early return.

The exit status is the daemon's answer, which makes `clipd drop x && rm x` safe:
a rejected drop exits non-zero, and one whose `tar` fails sends nothing at all.

Newly generated clients stream archives directly, without a second copy on the
remote disk. They send a completion marker only after `tar` succeeds. The Mac
requires that marker and the end of the connection before publication. Failed
transfers clean up their private received staging files; remote originals remain. If the final reply is
lost after publication, files may already be present despite the client reporting
failure; check Drop before retrying.

Interactive drops show received bytes, percentage for the current file, average
speed, elapsed time and estimated time remaining. Percentage is per file, not
for the whole request. A publishing status follows receipt. Only the final `clipd: ok:` confirms publication. Stdin
drops show bytes and elapsed time because their total size is unknown. Progress
goes to stderr and is disabled when stderr is not a terminal; use
`clipd drop --no-progress file` to hide it in a terminal too.

After upgrading the Mac binary, rerun `clipd setup <host>` to update the remote
function. Existing functions remain supported: their archive drops still use
remote disk staging and flatten filenames. New functions require a new daemon;
an older daemon rejects the structured request instead of copying it as text.
Setup replaces its managed SSH block, so retain or reapply manual alias changes.

Content is sent byte for byte — newlines, tabs and the trailing newline are
preserved. Input over `max_payload_bytes` (10 MiB by default) is rejected rather
than truncated.

Files and directories land beneath the configured directory, normally `~/Drop`.
`clipd drop author/book` creates `~/Drop/book/`, retaining its tree.
`clipd drop author/book/*` sends the children expanded by your shell, normally
excluding hidden files, placing them directly beneath `~/Drop`. Existing trees
are never merged or overwritten: a second `book` becomes `book-1`, and a second
`report.pdf` becomes `report-1.pdf`. Conflicting paths within one request are
rejected. Streaming archives accept only regular files and directories.

## Security

**The default listener is a private UNIX socket.** SSH forwarding supplies
remote access. The manual TCP fallback binds loopback only; other local
accounts can reach that port. Directly reachable network listeners are refused.

The socket reaches another machine only when you forward it over SSH. By then
SSH has encrypted the channel, verified the host key against `known_hosts`, and
authenticated you; the socket's `0600` permissions decide who on that machine
may write to it.

On the remote host that socket lives in `~/.clipd/`, a `0700` directory, and
`clipd setup` pins the bind mask that creates it `0600`. Two layers rather than
one because OpenSSH documents that not every operating system honours the mode
on a socket file — every one of them honours the mode on a directory.

clipd has no token and no TLS of its own because SSH has already done both
jobs. A token would also be worse: it would live as a file on the remote host,
and anyone who copied it could use it from anywhere until you rotated it. The
forwarded capability ends with the connection, but its socket pathname can
remain afterward and block a later forward. It is not a portable credential.

**What this means:** anything running as you on the remote host can write to
your clipboard and send you files, including a build script or a package
install. Other accounts on that host cannot, because of the socket's
permissions.

There is no way around this. Letting a remote machine write to your clipboard
means trusting what runs there as you. In practice anything able to abuse it
already has your files and your shell history on that machine.

Two things limit what it can do:

- **Bracketed paste**, on by default in modern shells, means pasted text ending
  in a newline is not executed until you press Enter. This is a guard against
  the accident, not a security boundary: it is the terminal's behaviour rather
  than clipd's, and anything that can write to the socket can still put whatever
  it likes on the clipboard.
- **Dropped files** use validated relative paths, private staging, collision
  handling and mode `0600`; directories are `0700`. Streaming requests reject
  traversal, absolute paths, control characters and special entries. Legacy
  archive requests retain basename flattening.
- **Validation precedes publication.** Streaming archives require the sender's
  completion marker. Ordinary transfer failures leave no published subset.
  Publishing multiple names is not crash-atomic; cleanup failures are reported.
  Legacy requests retain the entry-boundary truncation limitation.
- **Filesystem confinement has exceptions.** Hard-link publication and
  quarantine use pathname APIs, so protection against replacement of the drop
  root is not absolute. Custom socket validation checks the immediate parent's
  mode bits, not all ancestors, ownership or macOS ACLs.
- **Quarantine** is applied where it can be: files get macOS's quarantine
  attribute so Gatekeeper treats them like downloads. It is best effort — a drop
  that landed safely is not reported as failed because the label could not be
  set — so it is defence in depth on top of the rules above, not one of them.

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
  "max_concurrent": 8,
  "max_transfer_seconds": 1800
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

### Limits and large transfers

Defaults work without a config file. `clipd install` writes all supported keys,
retaining configured values. Running or setting up clipd does not create a
daemon config file.

| Key | Default | Meaning and range |
|---|---|---|
| `address` | `~/.clipd.sock` | UNIX socket, or manual loopback TCP address |
| `drop_dir` | `~/Drop` | Destination for published files and directories |
| `max_payload_bytes` | 10485760 (10 MiB) | Clipboard bytes; 1 byte to 1 GiB |
| `max_drop_bytes` | 268435456 (256 MiB) | Total file bytes per drop; up to 1 TiB |
| `max_drop_files` | 256 | Regular files per drop; up to 65536 |
| `max_concurrent` | 8 | Simultaneous work; up to 64 |
| `max_transfer_seconds` | 1800 (30 minutes) | Connection lifetime, including receipt and publication; 1 to 86400 (24 hours) |

10 GiB is `10737418240` bytes; 100 GiB is `107374182400`. One hour is `3600`
seconds. Edit the config and run `clipd restart`. Older installed binaries do
not understand the new lifetime key. Omitted keys retain defaults; zero means
default for numeric limits and transfer lifetime, rather than unlimited.

Each active drop can stage up to its byte limit on the Mac, so simultaneous
drops multiply disk demand. Increasing a limit does not reserve disk space.
Clipboard size times concurrency must fit within a 2 GiB budget; buffer growth
can transiently exceed it. Published files belong to you and are never cleaned up.

Connections also have a fixed 30-second read-idle timeout and a 64-connection
limit. Archive entries, depth, framing bytes, collision attempts, replies and
peer-driven logs are bounded. Increasing lifetime does not disable the idle
timeout. Generated transports have a separate 24-hour inactivity/wait bound,
so publication is not cut short by socat's usual half-second EOF wait. The
daemon's configured lifetime is the stricter bound for a healthy connection.
Quarantine helpers have a two-second deadline. No automatic retry or
resume is performed.

Unknown keys are rejected rather than ignored, so a typo fails loudly.
`CLIPD_CONFIG` is the only environment variable clipd reads.

Run `clipd help config` or `clipd help security` for detail.

## Troubleshooting

```bash
clipd status
```

It asks the daemon to identify itself rather than just checking the file
exists, because a daemon killed with `SIGKILL` leaves the socket behind and a
stale one looks identical in a directory listing. Exit code 0 means a clipd
daemon answered — not merely that something is listening.

**A missing socket or `Connection refused`** on the remote means the forward
is unavailable. From the Mac, restore it:

```bash
clipd reconnect debian
```

Reconnect checks the Mac daemon and forwarding config, closes this destination's
clipd-managed shared SSH connection, safely clears a stale remote socket, then
starts the restored forward in the background using OpenSSH. It returns to the
Mac prompt. Normal logins remain `ssh debian`; logout does not close the shared
connection or forward. Resetting a shared connection disconnects shells using
it, so close those shells before running reconnect.

Setup also safely clears stale sockets. Rerun setup once per destination to
upgrade older SSH blocks to connection sharing. Existing earlier SSH options
that override the managed sharing settings cause setup/reconnect to report an
error rather than silently fall back to a connection that dies on shell logout.

Recovery uses nc. It sends a protocol ping and
removes a socket only after an explicit connection-refused error and a
file-identity recheck. Other active listeners, ambiguous errors, symlinks, and
non-socket files are preserved. A missing socket needs no inspection tool.
Stale recovery needs an explicit error, as OpenBSD nc provides on Debian.
Apple nc's generic failure is ambiguous and is preserved rather than deleted.

A Mac restart, network failure, or terminating the master can still leave a
remote socket behind; reconnect handles that exceptional state. Normal shell
logout keeps the shared connection alive. `clipd restart` reloads the local
clipboard daemon; it does not reset SSH.

`ssh -O exit debian` closes the shared connection and its forward. The forwarded
capability remains available while that background SSH connection exists.
OpenSSH keepalives check it every 60 seconds and stop it after three unanswered
checks. No separate clipd process is installed on the remote.

`StreamLocalBindUnlink yes` in the remote server's `sshd_config` is an optional
alternative for automatically replacing remote socket pathnames, including
those of active forwards. Its client-side equivalent cannot control a remote
forward. Clipd does not modify the server configuration.

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
| `~/.ssh/clipd-<hash>` | private OpenSSH control socket for a shared connection |

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
| `~/.bashrc.clipd-backup` (or the matching rc file) | a copy of that file from before the first edit |
| `~/.clipd/` | a `0700` directory, created by `clipd setup`, holding the socket |
| `~/.clipd/socket` | created by sshd for the shared forward; can remain stale after the SSH connection ends |

Delete the marked block and the socket is gone on its own; `rm -rf ~/.clipd`
removes the directory it lived in.

## License

MIT
