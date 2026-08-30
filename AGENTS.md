# AGENTS.md

Repository-wide instructions for AI agents working on clipd. Keep this file for
durable constraints, verified behavior, and clearly labelled known gaps. The
README is user-facing; SECURITY.md defines the published security boundary.

## Working agreement

- An explicit instruction from the current user/repository owner overrides this
  file. Do not infer permission for a product, security-boundary, release, or
  live-environment change that was not requested.
- Before editing, inspect `git status`, the affected code and tests, and relevant
  README/SECURITY/help text. Preserve all existing changes and untracked files.
- Reproduce a bug when practical, or establish concrete evidence when runtime
  reproduction is unavailable. Add a regression test for a reachable failure.
- Keep changes surgical. Add no dependency, abstraction, config option, broad
  refactor, or compatibility layer without a demonstrated need.
- Remove dead code and stale comments. Remove a regression test only after an
  explicitly approved behavior change, and replace its coverage where needed.
- Do not stage, commit, tag, publish, install clipd, or modify a real clipboard,
  drop directory, shell rc file, SSH config, remote account, or LaunchAgent
  unless the user explicitly requests that action.

## Product contract

The normal path is a macOS daemon on a local UNIX socket, an SSH
`RemoteForward` to a UNIX socket on the remote host, and a generated POSIX-shell
function that sends bytes through `nc` or `socat`. Raw bytes go to `pbcopy`; a
framed drop is published in the configured directory, by default `~/Drop`.

Preserve these product decisions unless the repository owner explicitly approves
a change supported by a concrete need:

- No clipd binary, package, service, or long-running process is installed on a
  remote host. `clipd setup` does persist a shell function in a remote rc file
  and create `~/.clipd`; do not describe the remote as completely unmodified.
- The remote client remains generated shell rather than a second binary.
- Built-in defaults are a valid configuration. Running or setting up clipd does
  not create a config file; `clipd install` persists one.
- UNIX-socket forwarding is the supported setup path. The loopback-TCP fallback
  is manual and `clipd setup` refuses to configure it.
- The Go module has no third-party runtime/module dependencies.
- Successfully published drop files belong to the user and are not
  automatically deleted. Daemon-owned stale staging directories are different
  and may be cleaned safely.

## Security boundaries

### Default UNIX-socket mode

- For forwarded traffic, SSH supplies transport encryption, host verification,
  and user authentication. Filesystem permissions authorize direct local and
  forwarded UNIX-socket access.
- The local socket is mode `0600`. The remote socket is placed in `~/.clipd`,
  which setup creates as `0700`; the generated SSH block also pins
  `StreamLocalBindMask 0177`.
- Anything running as the same remote user can use the forwarded capability to
  write to the clipboard or send files. This is explicitly accepted.
- There is intentionally no additional token or TLS layer in this mode. Do not
  add one without an attack that defeats SSH plus filesystem authorization and
  explicit approval from the repository owner.

### Loopback-TCP fallback

- The daemon can hold a TCP listener on literal loopback addresses or
  `localhost`. It must refuse empty-host and non-loopback addresses because the
  protocol itself has no authentication.
- A loopback port has no per-user filesystem permissions. Other local accounts
  on the Mac can reach it, and other accounts on a remote host can reach a
  remotely forwarded loopback port. This is a consciously weaker fallback, not
  equivalent protection to the UNIX socket.
- Listener validation and TCP exposure are security-sensitive. Never claim that
  clipd has no TCP socket or that the network is categorically out of scope.

### Untrusted peers and resources

Treat every connected peer and every byte it supplies as untrusted, including an
SSH-authorized sender. Bounds must cover connections, concurrent work, buffered
payloads and their concurrency product, archive entries/files/extracted bytes,
total wire bytes, frame/reply/log size, idle time, absolute lifetime, and log
rate. These are per-request or in-flight bounds; cumulative successfully
published files are intentionally not bounded. Never log clipboard/file contents
or unbounded peer-controlled strings.

Custom-socket claims must match the code. `requirePrivateSocketDir` currently
checks only the immediate parent's Unix mode bits. It does not validate every
ancestor, ownership, or macOS ACLs. Do not claim those protections unless they
are implemented and tested.

## Dependencies, portability, and deployed interfaces

- Go 1.24 in `go.mod` is a compatibility promise. Code must work with Go 1.24
  and current stable Go; do not accidentally rely on a newer local toolchain.
- “No third-party dependencies” means Go module/runtime dependencies. clipd
  intentionally invokes OS tools such as `ssh`, `pbcopy`, `xattr`, and
  `launchctl`; CI may download development tools such as `govulncheck`.
- The remote may assume a POSIX shell and standard POSIX utilities. Archive-form
  drops additionally require `tar`, and transport requires a compatible `nc` or
  `socat`. A new non-POSIX remote dependency requires explicit justification.
  `mktemp` is not part of the current remote requirement.
- Test command variants rather than assuming GNU/Linux, BSD, and macOS flags are
  interchangeable, especially for `nc` and `tar`.

The remote shell function and daemon are upgraded independently. Treat these as
deployed compatibility surfaces:

- non-magic input remains raw clipboard data;
- `clipd:magic:v1` retains its current framing and JSON meaning;
- `clipd: ok: ` and `clipd: error: ` remain machine-readable prefixes;
- incompatible framing uses a new version plus an old-client transition;
- documented config keys, protocol versions, status prefixes, exit codes,
  marker text, persistent paths, and the launchd label require migration and
  regression coverage when changed.

Protocol changes must test raw/old clients, malformed and oversized frames,
unknown request types/fields, half-close behavior, response parsing, and version
skew. Never reinterpret a malformed structured request as clipboard data.

## High-risk implementation rules

### Generated shell and setup

`shellFunction` is sourced from `.bashrc`, `.zshrc`, or `.profile`; a parse error
can break every new shell on the remote host.

- Use POSIX syntax, but also source and execute the complete block in native
  dash/sh, Bash, and Zsh. Syntax-only `sh -n` is insufficient.
- Work under `set -u`; only guard a positional parameter while it may be unset.
  Keep scratch state and traps in a subshell and use `_clipd_`-prefixed names.
- Encode generated source values with `shellQuote`, quote runtime expansions,
  validate filenames before building JSON, escape JSON quotes/backslashes, and
  reject terminal/control characters. Test Unicode, newlines, invalid bytes,
  spaces, quotes, backslashes, and leading dashes.
- Put `--` before user-controlled tar operands. Preserve the daemon reply and
  report failure if tar, transport, or daemon publication fails, so
  `clipd drop file && rm file` is safe. Upstream status in
  `producer | clipd drop --name file` remains the invoking shell's responsibility.
- Archive staging with a predictable `$$` name is safe only inside clipd's
  private `0700` directory with `umask 077`, noclobber, bounded retries, and
  cleanup traps. It is not safe in a shared directory.

`cmd/clipd/setup.go` edits valuable files on two machines and is not a
cross-machine transaction:

- Bound remote command time and captured output; treat probe results as
  untrusted. Quote shell and SSH-config values with their separate rules.
- Validate marker ordering before mutation. Repeated setup must preserve
  unrelated content, multiple hosts, destination spelling, account scope, and
  idempotency.
- Local SSH-config updates preserve an existing symlink by atomically replacing
  its resolved regular-file target. Setup attempts `ssh -G` validation first,
  but currently proceeds unverified if `ssh` or a temporary candidate file is
  unavailable. Preserve the first pre-clipd backup and restrictive permissions.
- Remote rc updates validate and stage first, then intentionally use `cat >` to
  preserve inode, symlink, ownership, and mode. This final write is not
  crash-atomic. Do not call it atomic; changing it requires explicit analysis of
  both interruption safety and metadata/symlink preservation.
- A first-version backup is a recovery aid, not a current snapshot or
  cross-machine rollback. Partial success must be reported clearly and retry
  must be safe.
- `ssh -G` proves client-side parsing, matching, and expansion only. It does not
  prove remote sshd policy, socket creation/permissions, or successful
  forwarding. Those claims require a disposable integration host.
- `StreamLocalBindUnlink` exists in client and server configuration, and for a
  remote forward only the server's copy has any effect. Measured against a real
  host: the client option active in `ssh -G`, a stale socket in place, and the
  forward still refused. The generated block therefore does not emit it; do not
  add it back. This is also the example of why `ssh -G` proves parsing and
  matching but never remote behavior. Account-scoped blocks use
  `Match originalhost` because `Match host` sees the post-`HostName` value.

Tests must use temporary homes/configs and fake clients/listeners. Ordinary tests
must not mutate the developer's live environment.

### Drops, protocol, and server

- Treat every tar header, name, type, size, and body as attacker-controlled.
  Flatten paths to basenames; reject or safely handle absolute paths, `..`, both
  separator styles, symlink/hardlink/device/FIFO entries, unsupported bodies,
  long/control/bidirectional names, collisions, and leading dashes.
- Accept only regular-file payloads, keep received files mode `0600`, preserve
  file/entry/extracted/wire/collision limits, and do not rely only on tar's
  declared sizes.
- Use `os.Root`-relative operations wherever the API permits. Path-based
  exceptions such as hard-link publication and `xattr` quarantine require
  explicit audit and tests against root-path replacement.
- Stage privately and publish only after parsing succeeds. Rollback must not
  delete a file another process substituted. Stale cleanup may remove only the
  exact marker-owned, expected staging shape.
- Preserve bounded frame/raw reads, connections/goroutines, replies, rejection
  drains, deadlines, and logs. Validate payload/concurrency products without
  overflow.
- A socket at clipd's configured private path is removable only after bounded
  probing concludes nothing accepts it. Refuse and preserve live foreign
  listeners and every non-socket file.
- Keep literal loopback acceptance and empty/non-loopback refusal covered for
  IPv4 and IPv6.

Current tar-integrity limitation: Go's tar reader accepts EOF at an entry
boundary without proving that end-of-archive blocks arrived. The receiver can
therefore publish a valid prefix after an exact-boundary transport cut; a normal
generated client is expected to report transport failure, but a malicious sender
need not. Keep this as an accepted limitation until the repository owner approves
a compatible framing/end-marker design. Do not claim an end marker would provide
no protection; any proposal must address compatibility and resource bounds.

### Config and LaunchAgent

- Config defaults remain valid without a file. Reject unknown keys and invalid
  cross-field combinations, validate payload/concurrency products without
  overflow, preserve config symlinks and restrictive permissions, and refuse
  ambiguous/dangling targets.
- A new config option needs a demonstrated user requirement, default, validation,
  documentation, and compatibility tests.
- LaunchAgent replacement must preflight before mutation and preserve a working
  prior installation on failure. Keep label references synchronized and retain
  Darwin/Linux build-tag coverage. Use injected `launchctl` failures in tests.

## Known gaps at the current HEAD

These are not protections to preserve or assume. Address them only when they are
in scope, add regression coverage, and remove/update this list when fixed:

- A pre-existing interactive alias named `clipd` makes the generated function
  definition fail to parse in Bash and Zsh; no regression test covers it.
- Generated shell invokes important utilities directly, so interactive aliases
  or same-named functions can alter `tar`, `grep`, `sed`, `cat`, `rm`, and the
  selected transport command.
- Generated SSH blocks end with a comment rather than a real `Host *` (or
  equivalent) scope reset. A directive appended after the last managed block can
  therefore remain scoped to clipd's preceding `Host` or `Match` stanza.
- A named-drop frame can contain invalid UTF-8. Go's JSON decoder replaces those
  bytes with U+FFFD, so the receiver may publish a silently renamed file instead
  of rejecting the request.
- The `hasTar` branch disables named stdin drops even though only archive-form
  drops inherently require tar.
- Remote archive pre-spooling is outside daemon limits, can fill remote disk, and
  can leave private staging data after `SIGKILL` or power loss.
- Quarantine invokes `xattr` without a context/deadline, so not every helper is
  currently time-bounded.
- Hard-link publication and quarantine use path-based operations because
  `os.Root` lacks those APIs; opened-root confinement is therefore not absolute.
- Backup-path errors are not fully classified, and LaunchAgent rollback restores
  a working prior plist/job rather than the exact previous enabled/loaded state.
- Custom socket validation covers only the immediate parent's mode bits, as
  described in the security section.

## Verification and documentation

Run focused tests while working. Before presenting a code change as complete,
run from the repository root:

```sh
git diff --check
go build ./...
make check
```

`make check` is the canonical local `gofmt -s`, host/Linux vet, and race-test
suite. Keep Makefile and CI aligned rather than duplicating their command matrix
here. Also run, when relevant:

- `make dist` for build tags, release/build changes, or platform/architecture
  effects;
- Go 1.24/stable and macOS/Linux coverage for toolchain/platform behavior;
- native Bash/Zsh/dash sourcing and execution for generated-shell changes;
- `ssh -G` for client config and a disposable real host for sshd/forwarding;
- failure injection for setup, publication/rollback, helpers, and LaunchAgent;
- `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` for security-sensitive
  release/toolchain work when network access is appropriate.

After testing, inspect `git status` and remove only artifacts created by the
current work. Keep behavior/security claims synchronized across `README.md`,
`SECURITY.md`, `cmd/clipd/help.go`, examples, and tests.

Comments should explain a verified non-obvious invariant or tradeoff, not preserve
an audit narrative. Update or remove them when behavior changes. If the user asks
for a commit, use a concise lowercase imperative message consistent with project
history.
