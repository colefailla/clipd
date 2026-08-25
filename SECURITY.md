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

clipd has no authentication of its own. The daemon listens on a UNIX socket,
and SSH decides who reaches it. So the following are the intended grant rather
than escalations:

- Anything running as you on a host you have forwarded the socket to can write
  to your clipboard and send you files. Other accounts on that host cannot,
  because the socket is mode 0600, but your own processes can.
- Anyone with a shell as your user on either machine. The socket and the config
  are protected by filesystem permissions and nothing more.
- Reaching a daemon deliberately configured to listen on a TCP address. Nothing
  authenticates behind it, which is why the socket is the supported
  configuration and the daemon warns at startup when it is not used.

What *is* in scope: anything that writes outside the drop directory, escapes
the socket's permissions, or lets a peer consume unbounded daemon resources.

See the README's security section, or `clipd help security`, for the full
model.
