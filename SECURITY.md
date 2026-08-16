# Security policy

## Reporting a vulnerability

Report suspected vulnerabilities privately through GitHub's
[report a vulnerability](https://github.com/colefailla/clipd/security/advisories/new)
form, which opens a draft advisory visible only to the maintainer.

Please do not open a public issue for a vulnerability first.

clipd is a personal project maintained by one person, so there is no response
time commitment. Expect an acknowledgement within a week or so, and a fix or an
explanation of why something is working as intended after that.

Useful things to include: the clipd version (`clipd version`), both operating
systems involved, and the smallest sequence of steps that shows the problem.

## Supported versions

Fixes go onto `main` and into the next tagged release. Older tags are not
patched — there is only ever one supported version, the latest one.

## What clipd assumes

clipd's threat model is narrow, and knowing its shape is the fastest way to
judge whether something is a bug.

**In scope.** An attacker on the same network as the daemon, who can reach the
listening port and send it anything. They should not be able to write to the
clipboard, read what was copied, learn the token, impersonate the daemon to a
client, or stop the daemon from serving its owner.

**Also in scope.** An attacker who can observe or modify traffic between the two
machines. TLS 1.3 with a pinned server key is what answers this.

**Out of scope.** Anyone who already has the token, the daemon's private key, or
a shell on either machine. The token authorises clipboard writes by design, so
holding it is not an escalation, it is the intended grant. Likewise, a local
user who can read `~/Library/Application Support/clipd/` has already won:
those files are protected by filesystem permissions and nothing more.

**Out of scope.** The contents of the clipboard as a vector. clipd copies bytes
verbatim, and anyone holding the token can put arbitrary bytes — including
control characters and trailing newlines — on the clipboard. See the README's
security section for what that means in practice.

## Verifying a download

Releases ship `SHA256SUMS` and signed build provenance. The README's install
instructions verify the checksum, and

```bash
gh attestation verify clipd_darwin_arm64 --repo colefailla/clipd
```

confirms a binary came out of this repository's release workflow rather than
from someone else's machine.
