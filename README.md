# clipd

[![CI](https://github.com/colefailla/clipd/actions/workflows/ci.yml/badge.svg)](https://github.com/colefailla/clipd/actions/workflows/ci.yml)

Send command output from a remote machine to your Mac's clipboard.

```bash
ssh debian
docker ps | clipd
```

The output is now in the Mac's clipboard.

[OSC 52](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html) does the same
job through a terminal escape sequence, and is simpler when it works. clipd is
for the cases where it doesn't: Terminal.app doesn't support it, and in some
multiplexer and nested-SSH setups the sequence doesn't reliably reach the local
terminal.

## How it works

```text
Linux                                  macOS

  docker ps
     │ stdout
     ▼
  clipd  ────── TLS 1.3 ──────▶  clipd serve
  (exits)                             │
                                      ▼
                                   pbcopy ──▶ clipboard
```

clipd uses the same binary on both machines:

- On macOS, `clipd` runs as a LaunchAgent and writes received data to the
  system clipboard.
- On the remote machine, `clipd` reads stdin or a file and sends it to the Mac.

Nothing runs in the background on Linux. Each invocation connects, sends its
input, and exits.

Connections use TLS 1.3, a pinned server key, and a shared authentication
token.

## Install

These commands install a binary with `sudo`, so they verify it first. The
checksum step must print `OK`; if it prints anything else, the download is not
what the release workflow produced and should not be installed.

### macOS

```bash
ASSET=clipd_darwin_arm64   # Intel Macs: clipd_darwin_amd64
BASE=https://github.com/colefailla/clipd/releases/latest/download
curl -fsSL "$BASE/$ASSET" -o "$ASSET"
curl -fsSL "$BASE/SHA256SUMS" -o SHA256SUMS
shasum -a 256 --ignore-missing -c SHA256SUMS
sudo install -m 0755 "$ASSET" /usr/local/bin/clipd
rm "$ASSET" SHA256SUMS
```

### Linux

```bash
ASSET=clipd_linux_amd64   # ARM64: clipd_linux_arm64
BASE=https://github.com/colefailla/clipd/releases/latest/download
curl -fsSL "$BASE/$ASSET" -o "$ASSET"
curl -fsSL "$BASE/SHA256SUMS" -o SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
sudo install -m 0755 "$ASSET" /usr/local/bin/clipd
rm "$ASSET" SHA256SUMS
```

### Verifying provenance

`SHA256SUMS` proves a download is internally consistent with the release. To
check that the release itself was built by this repository's workflow from this
repository's source, rather than uploaded by hand, verify its signed build
provenance with the GitHub CLI:

```bash
gh attestation verify clipd_darwin_arm64 --repo colefailla/clipd
```

### From source

With Go installed, either platform can also use:

```bash
go install github.com/colefailla/clipd/cmd/clipd@latest
```

## Setup

On the Mac:

```bash
clipd install
```

This creates the configuration and TLS keypair, installs the LaunchAgent, and
prints the token and server fingerprint needed by clients.

On the remote machine:

```bash
clipd configure \
  -server <mac-address> \
  -fingerprint '<fingerprint>' \
  -token -
```

`-token -` reads the token from stdin, so paste it when prompted. Passing it as
`-token '<token>'` would put the secret on a command line, where other local
users can read it — on Linux, straight out of `/proc` — and where the shell
records it in history.

Then test it:

```bash
echo hello | clipd
```

Run `clipd status` to check the configuration and connection.

## Usage

Copy command output:

```bash
ls -la | clipd
docker ps | clipd
git diff | clipd
```

Copy a file:

```bash
clipd notes.txt
```

Explicitly use the `copy` command:

```bash
clipd copy notes.txt
echo hello | clipd copy
```

Show the number of bytes copied:

```bash
docker ps | clipd -v
```

A successful copy produces no output unless `-v` is used. Errors are written
to stderr.

Input is sent as-is without trimming or re-encoding.

The default limit is 10 MiB. Input over the limit is rejected rather than
truncated. To raise it for a single copy:

```bash
clipd copy -max-payload 20MB large.log
```

## Commands

```text
clipd                   Copy stdin
clipd copy [file]       Copy stdin or a file
clipd configure         Configure a client
clipd install           Install the macOS LaunchAgent
clipd uninstall         Remove the macOS LaunchAgent
clipd serve             Run the server in the foreground
clipd setup             Create or inspect server configuration
clipd status            Show configuration and connection status
clipd version           Show version information
clipd help [command]    Show help
```

## Configuration

Configuration is stored at:

```text
macOS:  ~/Library/Application Support/clipd/config.json
Linux:  ~/.config/clipd/config.json
```

`$XDG_CONFIG_HOME` is respected on Linux.

The default port is `8199`. The server listens on `0.0.0.0` by default so
remote machines can reach it.

A LAN IP, `.local` hostname, or Tailscale address can be used as the server
address.

Run:

```bash
clipd help config
clipd help <command>
```

for the config file format and per-command options.

## Exit codes

```text
0   success
1   connection or server failure
2   authentication failure
3   payload too large
4   configuration error
5   TLS handshake or fingerprint mismatch
64  usage error
```

## Security

clipd exposes a network service that can write to your Mac's clipboard. Only
expose it on networks you trust, or restrict access with a firewall.

Connections are encrypted with TLS 1.3. Clients authenticate using a randomly
generated token and verify the server using a pinned public-key fingerprint.

Anyone with the authentication token can write to your clipboard. Treat the
token as a secret.

That is worth stating concretely, because a clipboard is an unusual thing to
grant write access to. clipd sends bytes verbatim, so someone holding the token
can place anything at all on the clipboard — including text ending in a
newline. If you then paste into a shell, a trailing newline runs the line
without you pressing return. Most modern terminals defend against this with
bracketed paste, which makes a multi-line paste inert until you confirm it, but
Terminal.app in its default configuration is exactly the setup clipd exists to
serve. Treat the token as controlling what your shell might execute, not just
what you might read.

The token is stored locally in the clipd configuration file, which is created
with user-only permissions. Clipboard contents and authentication tokens are
not logged. `clipd status` warns if either the config file or the daemon's
private key has become readable by other users.

The daemon buffers each payload whole before writing it to the clipboard — a
copy that fails partway has to leave the clipboard untouched rather than
truncated — so its memory use would otherwise be the payload limit times the
copy limit. `max_memory_bytes` bounds that product directly, at 64 MiB by
default. Copies beyond it wait for room rather than being refused, and the
budget is never smaller than one maximum payload.

Connections are budgeted separately from copies, because the two cost very
different amounts. Holding a socket open is cheap and is allowed generously;
performing a copy reads a payload into memory and runs `pbcopy`, and is limited
by `max_concurrent`. A connection only competes for the second budget once it
has presented a valid token, so peers that connect and say nothing cannot crowd
out real clients. They also get a shorter deadline than the configurable
`timeout_ms`, which applies from the moment a client authenticates.

On top of that, connections that have not yet authenticated are rationed per
source address once enough are outstanding to matter. Authenticated clients are
never rationed, so parallel copies from your own machine are unaffected.

If the server fingerprint changes unexpectedly, do not accept the new
fingerprint without determining why it changed.

To replace a compromised token, on the Mac:

```bash
clipd setup -rotate
```

To replace the server keypair:

```bash
clipd setup -rotate-cert
```

Both print the new values. Clients must be reconfigured with
`clipd configure` afterward, and will fail to copy until they are.

To report a vulnerability, and for the threat model in full, see
[SECURITY.md](SECURITY.md).

## Troubleshooting

**`connect: connection refused`** The daemon isn't running, or the address or
port is wrong. Run `clipd status` on the Mac; if launchd shows it as not
loaded, run `clipd install`. Check that the client's `-server` and `-port`
match what the Mac is listening on.

**The client hangs, then times out.** Usually the macOS firewall dropping the
connection. Allow incoming connections for clipd in System Settings → Network
→ Firewall → Options.

**`server rejected the token`** The two machines have different tokens. Run
`clipd setup` on the Mac to print the current one, then `clipd configure
-token '<token>'` on the client.

**`server key fingerprint ... does not match the pinned ...`** The daemon is
presenting a different key. If you rotated it with `clipd setup -rotate-cert`,
re-run `clipd configure -fingerprint`. If you didn't, investigate before
changing anything. `clipd status` shows both fingerprints.

**`did not respond with TLS: it may be running clipd v1, which is unencrypted`**
The daemon is older than the client. Upgrade clipd on the Mac.

**`this daemon requires TLS; upgrade clipd on the client machine`** The client
is older than the daemon. Upgrade clipd on the client.

## Building

Requires Go 1.24 or later.

```bash
make build
make check
make dist
```

clipd has no third-party Go dependencies.

## License

MIT. See [LICENSE](LICENSE).
