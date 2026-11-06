# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately, through GitHub's
[report a vulnerability](https://github.com/colefailla/clipd/security/advisories/new)
form, rather than opening a public issue.

Useful things to include: the clipd version (`clipd version`), both operating
systems involved, and the shortest sequence of steps that shows the problem.

This is a personal project maintained by one person, so there is no response
time commitment, and fixes go onto `main` and into the next tagged release
rather than being backported.

## Out of scope

clipd has no authentication or encryption of its own. The daemon listens only on
a private UNIX socket, and SSH protects forwarded traffic. So the following are
the intended grant rather than escalations:

- Anything running as you on a host you have forwarded the socket to can write
  to your clipboard and send you files. Other accounts on that host cannot,
  because setup places the socket inside a private 0700 directory. The remote
  SSH server controls the socket mode; client settings do not enforce it.
- Anyone with a shell as your user on either machine. The socket and the config
  are protected by filesystem permissions and nothing more.

What *is* in scope: anything that writes outside the drop directory, escapes
the socket's permissions, or lets a peer consume unbounded daemon resources.

See the README's security section, or `clipd help security`, for the model in
plain terms. The rest of this file is the precise version.

## Guarantees and known exceptions

**Drops.** Streaming drops (current shell functions) write only regular files
and directories. Paths being received are validated; absolute paths, `..`,
control or bidirectional characters, and conflicting file paths refuse the
whole drop. Symlinks, hard links, devices and FIFOs are skipped, never
created or followed, and counted in the reply. Their names are not used as
filesystem paths. One that declares a body, or any other unsupported entry
type, refuses the drop. Files are received into a private staging directory inside the drop directory and published only after
the whole archive has parsed and the sender's completion marker, sent only after
`tar` succeeded, has arrived. Published files are `0600`, directories `0700`,
and existing names are never overwritten or merged. Archive drops from shell
functions generated before streaming are refused, with an instruction to rerun
`clipd setup`.

Exceptions: publishing several names is not crash-atomic. Hard-link publication
and the quarantine attribute use pathname APIs, so confinement to the opened
drop root is not absolute for those two operations. Quarantine is best effort.

**Resources.** Per-request bounds cover connections (64), concurrent work,
clipboard payload sizes and their product with concurrency (2 GiB), archive
entries, files and bytes, frame and reply sizes, the absolute lifetime of a
connection (`max_transfer_seconds`), reply writes and log rate. Reads have no
separate idle timeout: a silent sender holds its connection until that lifetime
ends, within the connection cap. Reading stops up to five seconds before the
lifetime ends, so the reason can be written without extending it. Cumulative
published files are not bounded. The payload budget is not a process-memory
ceiling: buffer capacity, allocation overhead and other state use additional
memory.

**Sockets.** The Mac socket is mode `0600`. A custom socket path is checked for
the immediate parent directory's mode bits only, not every ancestor, ownership
or macOS ACLs. On the remote, the generated shell function removes
`~/.clipd/socket` only when a ping to it returns no reply and the client exits
with status 1, and only if the file is still the same socket by inode. For
OpenBSD `nc` and `socat` the message must also include `: Connection
refused`; a path that merely contains the word "refused" does not count. macOS `nc`
prints nothing, and exits 1 silently for some other failures before connecting
too, so on a macOS remote a silent exit 1 is a heuristic for a stale socket
rather than proof; a wrong guess removes a live forward and costs a reconnect. A
timeout, a connection closed without a reply, or a signal never removes the
socket. These exit statuses and messages were measured for macOS `nc`. For
OpenBSD `nc` and `socat` they come from their source and documentation; the test
suite exercises OpenBSD `nc` only when it runs on Linux with netcat-openbsd
installed, and does not exercise `socat` unless it is installed. The ping's
transport flags (`nc -w 5`, `socat -T 5 -t 5`) are five-second inactivity
timeouts, not an absolute deadline: they end a connection that stays silent, not
a connect that blocks or a peer that keeps sending. Re-running `clipd setup`
applies the same check. Another session binding a new socket in that instant is
a race within the user's own account, accepted under the boundary above.
