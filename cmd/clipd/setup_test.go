package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientCommandPicksWhatTheHostHas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		facts remoteFacts
		want  string
	}{
		{"linux, modern netcat", remoteFacts{os: "Linux", hasNC: true, ncUnix: true, ncShutdown: true}, "nc -N -U"},
		{"socat fallback", remoteFacts{os: "Linux", hasSocat: true}, "socat"},
		{"traditional netcat with socat", remoteFacts{os: "Linux", hasNC: true, hasSocat: true}, "socat"},
		// macOS netcat has -N, but there it takes a probe count for a write
		// timeout rather than half-closing. Passing it OpenBSD-style fails
		// outright, and this netcat needs no flag to close on stdin EOF.
		{"macos remote", remoteFacts{os: "Darwin", hasNC: true, ncUnix: true, ncShutdown: true}, "nc -U"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := clientCommand(tc.facts)
			if err != nil {
				t.Fatalf("clientCommand: %v", err)
			}
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("clientCommand = %q, want it to start with %q", got, tc.want)
			}
		})
	}
}

// TestClientCommandNamesTheFix is the whole point of probing: when the host
// cannot do it, the message has to say which package to install rather than
// leaving the user to decode "invalid option -- 'U'".
func TestClientCommandNamesTheFix(t *testing.T) {
	t.Parallel()

	tests := map[string]remoteFacts{
		"netcat without -U":             {os: "Linux", hasNC: true},
		"nothing at all":                {os: "Linux"},
		"netcat that cannot half-close": {os: "Linux", hasNC: true, ncUnix: true},
	}
	for name, facts := range tests {
		_, err := clientCommand(facts)
		if err == nil {
			t.Errorf("%s: clientCommand succeeded, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "netcat-openbsd") && !strings.Contains(err.Error(), "socat") {
			t.Errorf("%s: err = %v, want it to name something that fixes it", name, err)
		}
	}
}

func TestRCFileForShell(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"/bin/zsh":      "$HOME/.zshrc",
		"/usr/bin/bash": "$HOME/.bashrc",
		"/bin/sh":       "$HOME/.profile",
		"/usr/bin/fish": "$HOME/.profile",
		"":              "$HOME/.profile",
	}
	for shell, want := range tests {
		if got := rcFileFor(remoteFacts{shell: shell}); got != want {
			t.Errorf("rcFileFor(%q) = %q, want %q", shell, got, want)
		}
	}
}

// TestShellFunctionIsValidShell runs the generated function through `sh -n`.
//
// It is generated text that will be pasted into someone's .bashrc, where a
// syntax error does not just break clipd — it breaks every new shell on that
// host, which is a genuinely bad afternoon for whoever hits it.
func TestShellFunctionIsValidShell(t *testing.T) {
	t.Parallel()

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}

	clients := []string{`nc -N -U "$_clipd_sock"`, `nc -U "$_clipd_sock"`, `socat - UNIX-CLIENT:"$_clipd_sock"`}
	for _, client := range clients {
		for _, hasTar := range []bool{true, false} {
			block := shellFunction(client, "/home/cole/.clipd.sock", hasTar)

			cmd := exec.Command(sh, "-n")
			cmd.Stdin = strings.NewReader(block)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("generated shell is invalid (client=%q tar=%v): %v\n%s\n%s",
					client, hasTar, err, out, block)
			}
		}
	}
}

// TestShellFunctionSendsTheRightFrame pins the two shapes the daemon parses:
// a bare stream for the clipboard, and magic plus envelope plus tar for a drop.
func TestShellFunctionSendsTheRightFrame(t *testing.T) {
	t.Parallel()

	block := shellFunction(`nc -N -U "$_clipd_sock"`, "/home/cole/.clipd.sock", true)

	if !strings.Contains(block, `clipd:magic:v1\n{"type":"drop"}\n`) {
		t.Errorf("the drop path does not emit the magic frame:\n%s", block)
	}
	// The -- is load-bearing: without it a file named
	// "--use-compress-program=curl" is read by tar as an option and executed.
	if !strings.Contains(block, `COPYFILE_DISABLE=1 tar cf - -- "$@"`) {
		t.Errorf("the drop path does not pipe a tar stream with -- :\n%s", block)
	}
	// The socket check has to come first: without it, a session opened before
	// setup ran fails with a netcat error instead of an explanation.
	if !strings.Contains(block, `if [ ! -S "$_clipd_sock" ]`) {
		t.Errorf("the function does not check the socket exists:\n%s", block)
	}
	if !strings.HasPrefix(block, blockStart) || !strings.HasSuffix(strings.TrimSpace(block), blockEnd) {
		t.Errorf("the block is not bracketed by markers:\n%s", block)
	}
}

func TestShellFunctionRefusesDropWithoutTar(t *testing.T) {
	t.Parallel()

	block := shellFunction(`nc -N -U "$_clipd_sock"`, "/home/cole/.clipd.sock", false)
	if strings.Contains(block, "tar cf") {
		t.Errorf("the function uses tar on a host that has none:\n%s", block)
	}
	if !strings.Contains(block, "no tar") {
		t.Errorf("the function does not explain why drop is unavailable:\n%s", block)
	}
}

func TestStripBlockRemovesAManagedSection(t *testing.T) {
	t.Parallel()

	start, end := sshMarkers("debian")
	give := "Host other\n  User someone\n\n" +
		start + "\nHost debian\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + end + "\n\nHost third\n  User x\n"
	got, err := stripBlock(give, start, end)
	if err != nil {
		t.Fatalf("stripBlock: %v", err)
	}

	if strings.Contains(got, "RemoteForward") || strings.Contains(got, start) {
		t.Errorf("the managed block survived:\n%s", got)
	}
	for _, want := range []string{"Host other", "Host third"} {
		if !strings.Contains(got, want) {
			t.Errorf("stripBlock removed unrelated content %q:\n%s", want, got)
		}
	}
}

func TestStripBlockLeavesUnmanagedFilesAlone(t *testing.T) {
	t.Parallel()

	start, end := sshMarkers("debian")
	give := "Host debian\n  User cole\n  RemoteForward mine:yours"
	got, err := stripBlock(give, start, end)
	if err != nil {
		t.Fatalf("stripBlock: %v", err)
	}
	if got != give {
		t.Errorf("stripBlock changed an unmanaged file:\n%s", got)
	}
}

// TestInstallSSHConfigIsIdempotent cannot be parallel: it points HOME at a
// temp directory, which is process-wide state.
func TestInstallSSHConfigIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	start, end := sshMarkers("debian")
	block := start + "\nHost debian\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + end + "\n"

	path, changed, err := installSSHConfig(t.Context(), "debian", block)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !changed {
		t.Error("the first install reported no change")
	}

	_, changed, err = installSSHConfig(t.Context(), "debian", block)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if changed {
		t.Error("the second install rewrote an already-current file")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := strings.Count(string(data), start); n != 1 {
		t.Errorf("the config holds %d managed blocks, want 1:\n%s", n, data)
	}
}

// TestInstallSSHConfigPreservesAndBacksUp: this file is often hand-tuned, so
// existing content must survive and a backup must exist before the first edit.
func TestInstallSSHConfigPreservesAndBacksUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := "Host work\n  User me\n  IdentityFile ~/.ssh/work_ed25519\n"
	configPath := filepath.Join(sshDir, "config")
	if err := os.WriteFile(configPath, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	start, end := sshMarkers("debian")
	block := start + "\nHost debian\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + end + "\n"
	if _, _, err := installSSHConfig(t.Context(), "debian", block); err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "IdentityFile ~/.ssh/work_ed25519") {
		t.Errorf("the existing configuration was lost:\n%s", data)
	}
	if !strings.Contains(string(data), "RemoteForward \"/home/c/.clipd/socket\"") {
		t.Errorf("the forward was not added:\n%s", data)
	}

	backup, err := os.ReadFile(configPath + ".clipd-backup")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != existing {
		t.Errorf("backup = %q, want the original file", backup)
	}
}

func TestInstallSSHConfigWritesRestrictivePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	start, end := sshMarkers("debian")
	path, _, err := installSSHConfig(t.Context(), "debian", start+"\nHost debian\n"+end+"\n")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// ssh itself refuses to use a config other users can write.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config mode = %04o, want no group or other access", perm)
	}
}

// TestHostPatternStripsTheUser pins a silent failure: ssh removes the user
// before matching Host patterns, so a block written as "Host cole@debian"
// matches nothing and the forward never happens, with no error anywhere.
func TestHostPatternStripsTheUser(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ user, host string }{
		"debian":            {"", "debian"},
		"cole@debian":       {"cole", "debian"},
		"cole@debian.local": {"cole", "debian.local"},
		"root@10.0.0.5":     {"root", "10.0.0.5"},
	}
	for give, want := range tests {
		user, host := splitDestination(give)
		if user != want.user || host != want.host {
			t.Errorf("splitDestination(%q) = %q, %q; want %q, %q", give, user, host, want.user, want.host)
		}
	}
}

// TestSSHBlockScopesToTheUser: ssh strips the user before matching Host
// patterns, so `Host server` applies to every account on that machine. Setting
// up bob@server then replaced alice's block and pointed her connections at
// bob's home directory, where the forward fails with a permission error.
func TestSSHBlockScopesToTheUser(t *testing.T) {
	t.Parallel()

	withUser, err := sshBlockFor("alice@server", "/home/alice/.clipd/socket", "/Users/c/.clipd.sock")
	if err != nil {
		t.Fatalf("sshBlockFor: %v", err)
	}
	if !strings.Contains(withUser, `Match host "server" user "alice"`) {
		t.Errorf("a user-qualified destination did not produce a Match block:\n%s", withUser)
	}

	bare, err := sshBlockFor("server", "/home/c/.clipd/socket", "/Users/c/.clipd.sock")
	if err != nil {
		t.Fatalf("sshBlockFor: %v", err)
	}
	if !strings.Contains(bare, `Host "server"`) {
		t.Errorf("a bare destination did not produce a Host block:\n%s", bare)
	}
	// Both halves of the socket's protection on a shared remote host.
	for _, want := range []string{"StreamLocalBindMask 0177", "StreamLocalBindUnlink yes"} {
		if !strings.Contains(bare, want) {
			t.Errorf("the block is missing %q:\n%s", want, bare)
		}
	}
}

// TestSSHBlockRefusesAWideningPattern: `clipd setup '*'` would write a stanza
// matching every host, so the forward is attempted on every connection.
func TestSSHBlockRefusesAWideningPattern(t *testing.T) {
	t.Parallel()

	for _, destination := range []string{"*", "web?", "!prod", "a b"} {
		if _, err := sshBlockFor(destination, "/r/socket", "/l/socket"); err == nil {
			t.Errorf("sshBlockFor(%q) wrote a block for a pattern, not a host", destination)
		}
	}
}

// TestSSHBlockQuotesAndEscapes: a path with a space splits into the wrong
// number of arguments unquoted, and ssh expands %h and friends inside these
// values whatever the quoting.
func TestSSHBlockQuotesAndEscapes(t *testing.T) {
	t.Parallel()

	block, err := sshBlockFor("server", "/home/100%real/my socket", "/Users/c/.clipd.sock")
	if err != nil {
		t.Fatalf("sshBlockFor: %v", err)
	}
	if !strings.Contains(block, `"/home/100%%real/my socket"`) {
		t.Errorf("the remote path was not quoted and percent-escaped:\n%s", block)
	}
}

// TestStripBlockRefusesAnUnmatchedMarker: the previous version skipped from a
// start marker to the end of the file looking for a close that was not there,
// so a half-deleted block took the rest of ~/.ssh/config with it.
func TestStripBlockRefusesAnUnmatchedMarker(t *testing.T) {
	t.Parallel()

	content := "Host keep\n  User me\n" + blockStart + "\nHost gone\n"
	if _, err := stripBlock(content, blockStart, blockEnd); err == nil {
		t.Error("stripBlock accepted an unmatched start marker")
	}
}

// TestSeveralHostsCoexist is the property one shared marker broke: setup is run
// once per host, and each run must leave the others' forwards alone. With a
// single marker the second run stripped the first on its way to writing its
// own, so only the most recent host kept a forward.
func TestSeveralHostsCoexist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	hosts := []string{"debian", "debian.local", "10.0.0.5"}
	for _, h := range hosts {
		start, end := sshMarkers(h)
		block := start + "\nHost " + h + "\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + end + "\n"
		if _, _, err := installSSHConfig(t.Context(), h, block); err != nil {
			t.Fatalf("setup %s: %v", h, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, h := range hosts {
		if !strings.Contains(string(data), "Host "+h+"\n") {
			t.Errorf("%s lost its block after later setups:\n%s", h, data)
		}
	}
}

// TestLegacyUnhostedBlockIsReplaced covers the upgrade: a config written before
// the markers carried a host name has one unhosted block, which would otherwise
// survive and forward the same socket twice.
func TestLegacyUnhostedBlockIsReplaced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := blockStart + "\nHost debian\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + blockEnd + "\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	start, end := sshMarkers("debian")
	block := start + "\nHost debian\n  RemoteForward \"/home/c/.clipd/new\" \"/Users/c/.clipd.sock\"\n" + end + "\n"
	if _, _, err := installSSHConfig(t.Context(), "debian", block); err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sshDir, "config"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "old:old") {
		t.Errorf("the legacy block survived, forwarding the socket twice:\n%s", data)
	}
	if !strings.Contains(string(data), "/home/c/.clipd/new") {
		t.Errorf("the new block is missing:\n%s", data)
	}
}

// TestShellFunctionHandlesBothDropForms pins the two shapes the daemon
// distinguishes: files on the command line become a tar, and --name sends this
// shell's stdin under a name of its own.
//
// The distinction used to be drawn from whether stdin was a terminal, which
// made `ssh host 'clipd drop report.pdf'` — and every drop from a script or a
// cron job — send a header with no body, creating an empty file under the right
// name and reporting success.
func TestShellFunctionHandlesBothDropForms(t *testing.T) {
	t.Parallel()

	block := shellFunction(`nc -N -U "$_clipd_sock"`, "/home/cole/.clipd/socket", true)

	if strings.Contains(block, "[ -t 0 ]") {
		t.Errorf("the function still infers the drop form from the terminal:\n%s", block)
	}
	if !strings.Contains(block, `if [ "${1:-}" = "--name" ]`) {
		t.Errorf("the function does not branch on an explicit --name:\n%s", block)
	}
	if !strings.Contains(block, `COPYFILE_DISABLE=1 tar cf - -- "$@"`) {
		t.Errorf("the file form does not tar its arguments:\n%s", block)
	}
	if !strings.Contains(block, `"name":"%s"`) {
		t.Errorf("the --name form does not send a name:\n%s", block)
	}
	// The name lands inside a JSON string, so those two characters must be
	// escaped or a filename with a quote produces a parse error on the daemon.
	if !strings.Contains(block, "sed 's/") {
		t.Errorf("the --name form does not escape the name for JSON:\n%s", block)
	}
}

// TestShellFunctionSurvivesNounset: a plain `printf x | clipd` passes no
// arguments, and expanding $1 unguarded aborted the whole shell under `set -u`.
func TestShellFunctionSurvivesNounset(t *testing.T) {
	t.Parallel()

	block := shellFunction(`nc -N -U "$_clipd_sock"`, "/home/cole/.clipd/socket", true)
	if strings.Contains(block, `"$1"`) {
		t.Errorf("the function expands $1 unguarded:\n%s", block)
	}
	if !strings.Contains(block, `"${1:-}"`) {
		t.Errorf("the function does not guard its first argument:\n%s", block)
	}
}

// TestShellFunctionReportsFailureAsExitStatus: the daemon's reply is the only
// thing that knows whether a drop was accepted, and a pipeline reports only
// netcat's status. Without this, `clipd drop x && rm x` deleted files the
// daemon had refused.
func TestShellFunctionReportsFailureAsExitStatus(t *testing.T) {
	t.Parallel()

	block := shellFunction(`nc -N -U "$_clipd_sock"`, "/home/cole/.clipd/socket", true)
	if !strings.Contains(block, "'clipd: ok: '*) return 0 ;;") {
		t.Errorf("the function does not turn the daemon's reply into an exit status:\n%s", block)
	}
	// tar's own status is lost in the pipeline unless it is carried out of it.
	if !strings.Contains(block, "_clipd_tar") {
		t.Errorf("the function does not recover tar's exit status:\n%s", block)
	}
}

// TestShellFunctionDoesNotLeakVariables: this runs in the user's interactive
// shell, where a bare `name` or `sock` would overwrite one of theirs.
func TestShellFunctionDoesNotLeakVariables(t *testing.T) {
	t.Parallel()

	block := shellFunction(`nc -N -U "$_clipd_sock"`, "/home/cole/.clipd/socket", true)
	for _, bare := range []string{"\n  sock=", "\n      name=", "\n      esc="} {
		if strings.Contains(block, bare) {
			t.Errorf("the function assigns an unprefixed variable %q:\n%s", bare, block)
		}
	}
}

// TestMacOSRemoteDoesNotGetTheOpenBSDFlag is the case the OS probe exists for.
//
// Both netcats advertise -N, so a check for the flag's presence cannot tell
// them apart. On macOS it takes a probe count for a write timeout, and passing
// it the way OpenBSD's is passed fails with "invalid tcp adaptive write
// timeout value" on every copy — after setup has already reported success.
func TestMacOSRemoteDoesNotGetTheOpenBSDFlag(t *testing.T) {
	t.Parallel()

	got, err := clientCommand(remoteFacts{
		os: "Darwin", hasNC: true, ncUnix: true, ncShutdown: true,
	})
	if err != nil {
		t.Fatalf("clientCommand: %v", err)
	}
	if strings.Contains(got, "-N") {
		t.Errorf("clientCommand = %q, want no -N on a macOS remote", got)
	}
	if !strings.Contains(got, "-U") {
		t.Errorf("clientCommand = %q, want it to still use the socket", got)
	}
}

// TestProbeAsksForTheOperatingSystem: the client choice depends on it, so it
// has to be collected rather than assumed.
func TestProbeAsksForTheOperatingSystem(t *testing.T) {
	t.Parallel()

	if !strings.Contains(probeScript, "uname -s") {
		t.Errorf("the probe does not ask the host what it is:\n%s", probeScript)
	}
}
