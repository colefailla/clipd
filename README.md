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
for cases where it does not. Terminal.app doesn't support it, and in some
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

### macOS

Apple Silicon:

```bash
curl -fsSL https://github.com/colefailla/clipd/releases/latest/download/clipd_darwin_arm64 -o clipd
sudo install -m 0755 clipd /usr/local/bin/clipd
rm clipd
```

For Intel Macs, use `clipd_darwin_amd64`.

### Linux

x86-64:

```bash
curl -fsSL https://github.com/colefailla/clipd/releases/latest/download/clipd_linux_amd64 -o clipd
sudo install -m 0755 clipd /usr/local/bin/clipd
rm clipd
```

For ARM64, use `clipd_linux_arm64`.

With Go installed, either platform can also use:

```bash
go install github.com/colefailla/clipd/cmd/clipd@latest
```

Releases include `SHA256SUMS` and signed build provenance. To confirm a binary
came from this repository's release workflow:

```bash
gh attestation verify clipd_darwin_arm64 --repo colefailla/clipd
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

Run `clipd status` to check the configuration and connection. It exits 0 only
if the probe succeeds, so it works as a preflight in a script — and will fail
one if the daemon is not up yet.

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
truncated. Both ends enforce it: the client before sending, and the daemon
before accepting. To copy something larger, raise the daemon's limit on the
Mac (and restart it):

```bash
clipd setup -max-payload 20MB
launchctl kickstart -k gui/$(id -u)/com.clipd.agent
```

Then raise it for a single copy on the client:

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

clipd sends bytes verbatim, so someone with the token can put anything on your clipboard, including text
ending in a newline that a paste into a shell would run without you pressing return.

The token is stored locally in the clipd configuration file, which is created
with user-only permissions. Clipboard contents and authentication tokens are
not logged. `clipd status` warns if either the config file or the daemon's
private key has become readable by other users.

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

To report a vulnerability, see [SECURITY.md](SECURITY.md).

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
