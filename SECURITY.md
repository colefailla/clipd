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

clipd has no authentication or encryption of its own. The default listener is
a private UNIX socket, and SSH protects forwarded traffic. So the following are the intended grant rather
than escalations:

- Anything running as you on a host you have forwarded the socket to can write
  to your clipboard and send you files. Other accounts on that host cannot,
  because the socket is mode 0600, but your own processes can.
- Anyone with a shell as your user on either machine. The socket and the config
  are protected by filesystem permissions and nothing more.
- Reaching a daemon deliberately configured to listen on loopback TCP. Other
  local accounts can reach it; no protocol authentication protects it. Empty
  hosts and non-loopback bindings are refused.

What *is* in scope: anything that writes outside the drop directory, escapes
the socket's permissions, or lets a peer consume unbounded daemon resources.

See the README's security section, or `clipd help security`, for the full
model.

Streaming drops validate relative paths, file types and resource limits before
publication, and require a successful-producer completion marker. Legacy drops
retain basename flattening and the documented entry-boundary truncation gap.
Publication is not crash-atomic. Hard-link publication and quarantine use path
APIs; opened-root confinement does not cover those operations absolutely.
Custom socket validation checks only the immediate parent's mode bits.
