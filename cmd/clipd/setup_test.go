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
		{"modern netcat", remoteFacts{hasNC: true, ncUnix: true, ncShutdown: true}, "nc -N -U"},
		{"netcat without -N", remoteFacts{hasNC: true, ncUnix: true}, "nc -U"},
		{"socat fallback", remoteFacts{hasSocat: true}, "socat"},
		{"netcat and socat", remoteFacts{hasNC: true, hasSocat: true}, "socat"},
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
		"netcat without -U": {hasNC: true},
		"nothing at all":    {},
	}
	for name, facts := range tests {
		_, err := clientCommand(facts)
		if err == nil {
			t.Errorf("%s: clientCommand succeeded, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "netcat-openbsd") {
			t.Errorf("%s: err = %v, want it to name the package that fixes it", name, err)
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

	clients := []string{`nc -N -U "$sock"`, `nc -U "$sock"`, `socat - UNIX-CLIENT:"$sock"`}
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

	block := shellFunction(`nc -N -U "$sock"`, "/home/cole/.clipd.sock", true)

	if !strings.Contains(block, `clipd:magic:v1\n{"type":"drop"}\n`) {
		t.Errorf("the drop path does not emit the magic frame:\n%s", block)
	}
	if !strings.Contains(block, `COPYFILE_DISABLE=1 tar cf - "$@"`) {
		t.Errorf("the drop path does not pipe a tar stream:\n%s", block)
	}
	// The socket check has to come first: without it, a session opened before
	// setup ran fails with a netcat error instead of an explanation.
	if !strings.Contains(block, `if [ ! -S "$sock" ]`) {
		t.Errorf("the function does not check the socket exists:\n%s", block)
	}
	if !strings.HasPrefix(block, blockStart) || !strings.HasSuffix(strings.TrimSpace(block), blockEnd) {
		t.Errorf("the block is not bracketed by markers:\n%s", block)
	}
}

func TestShellFunctionRefusesDropWithoutTar(t *testing.T) {
	t.Parallel()

	block := shellFunction(`nc -N -U "$sock"`, "/home/cole/.clipd.sock", false)
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
		start + "\nHost debian\n  RemoteForward a:b\n" + end + "\n\nHost third\n  User x\n"
	got := stripBlock(give, start, end)

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
	if got := stripBlock(give, start, end); got != give {
		t.Errorf("stripBlock changed an unmanaged file:\n%s", got)
	}
}

// TestInstallSSHConfigIsIdempotent cannot be parallel: it points HOME at a
// temp directory, which is process-wide state.
func TestInstallSSHConfigIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	start, end := sshMarkers("debian")
	block := start + "\nHost debian\n  RemoteForward a:b\n" + end + "\n"

	path, changed, err := installSSHConfig("debian", block)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !changed {
		t.Error("the first install reported no change")
	}

	_, changed, err = installSSHConfig("debian", block)
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
	block := start + "\nHost debian\n  RemoteForward a:b\n" + end + "\n"
	if _, _, err := installSSHConfig("debian", block); err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "IdentityFile ~/.ssh/work_ed25519") {
		t.Errorf("the existing configuration was lost:\n%s", data)
	}
	if !strings.Contains(string(data), "RemoteForward a:b") {
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
	path, _, err := installSSHConfig("debian", start+"\nHost debian\n"+end+"\n")
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

	tests := map[string]string{
		"debian":            "debian",
		"cole@debian":       "debian",
		"cole@debian.local": "debian.local",
		"root@10.0.0.5":     "10.0.0.5",
	}
	for give, want := range tests {
		if got := hostPattern(give); got != want {
			t.Errorf("hostPattern(%q) = %q, want %q", give, got, want)
		}
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
		block := start + "\nHost " + h + "\n  RemoteForward /home/c/.clipd.sock:/Users/c/.clipd.sock\n" + end + "\n"
		if _, _, err := installSSHConfig(h, block); err != nil {
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
	legacy := blockStart + "\nHost debian\n  RemoteForward old:old\n" + blockEnd + "\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	start, end := sshMarkers("debian")
	block := start + "\nHost debian\n  RemoteForward new:new\n" + end + "\n"
	if _, _, err := installSSHConfig("debian", block); err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sshDir, "config"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "old:old") {
		t.Errorf("the legacy block survived, forwarding the socket twice:\n%s", data)
	}
	if !strings.Contains(string(data), "new:new") {
		t.Errorf("the new block is missing:\n%s", data)
	}
}

// TestShellFunctionHandlesBothDropForms pins the two shapes the daemon
// distinguishes: files on the command line become a tar, and a pipeline
// becomes one named stream.
func TestShellFunctionHandlesBothDropForms(t *testing.T) {
	t.Parallel()

	block := shellFunction(`nc -N -U "$sock"`, "/home/cole/.clipd.sock", true)

	if !strings.Contains(block, "if [ -t 0 ]; then") {
		t.Errorf("the function does not branch on whether stdin is a pipe:\n%s", block)
	}
	if !strings.Contains(block, `COPYFILE_DISABLE=1 tar cf - "$@"`) {
		t.Errorf("the file form does not tar its arguments:\n%s", block)
	}
	if !strings.Contains(block, `"name":"%s"`) {
		t.Errorf("the pipe form does not send a name:\n%s", block)
	}
	// A nameless file in the drop directory is worse than an ugly one.
	if !strings.Contains(block, "drop-$(date") {
		t.Errorf("the pipe form invents no name when none is given:\n%s", block)
	}
	// The name lands inside a JSON string, so those two characters must be
	// escaped or a filename with a quote produces a parse error on the daemon.
	if !strings.Contains(block, "sed 's/") {
		t.Errorf("the pipe form does not escape the name for JSON:\n%s", block)
	}
}
