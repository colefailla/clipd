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

	give := "Host other\n  User someone\n\n" +
		blockStart + "\nHost debian\n  RemoteForward a:b\n" + blockEnd + "\n\nHost third\n  User x\n"
	got := stripBlock(give)

	if strings.Contains(got, "RemoteForward") || strings.Contains(got, blockStart) {
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

	give := "Host debian\n  User cole\n  RemoteForward mine:yours"
	if got := stripBlock(give); got != give {
		t.Errorf("stripBlock changed an unmanaged file:\n%s", got)
	}
}

// TestInstallSSHConfigIsIdempotent cannot be parallel: it points HOME at a
// temp directory, which is process-wide state.
func TestInstallSSHConfigIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	block := blockStart + "\nHost debian\n  RemoteForward a:b\n" + blockEnd + "\n"

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
	if n := strings.Count(string(data), blockStart); n != 1 {
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

	block := blockStart + "\nHost debian\n  RemoteForward a:b\n" + blockEnd + "\n"
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

	path, _, err := installSSHConfig("debian", blockStart+"\nHost debian\n"+blockEnd+"\n")
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
