package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRecoversSocketForEachAlias(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh unavailable")
	}
	for _, state := range []string{"stale", "active", "invalid-file", "print"} {
		t.Run(state, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "clipd-setup-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			local, remote := filepath.Join(dir, "mac"), filepath.Join(dir, "remote")
			for _, path := range []string{filepath.Join(local, ".ssh"), filepath.Join(remote, ".clipd")} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			configPath := filepath.Join(local, "clipd.json")
			if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`{"address":%q}`, filepath.Join(local, "daemon"))), 0o600); err != nil {
				t.Fatal(err)
			}
			sshPath := filepath.Join(local, ".ssh/config")
			original := "Host debian debian.local\n  HostName example.invalid\n  User cole\n"
			if err := os.WriteFile(sshPath, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			rc := filepath.Join(remote, ".bashrc")
			if err := os.WriteFile(rc, []byte("# unrelated shell settings\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(remote, ".clipd/socket")
			if state == "invalid-file" {
				if err := os.WriteFile(socket, []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				ln, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				ln.(*net.UnixListener).SetUnlinkOnClose(false)
				if state == "active" {
					defer ln.Close()
				} else {
					ln.Close()
				}
			}
			fake := "#!/bin/sh\ncase \"$*\" in\n  *'-G '*) exec " + shellQuote(ssh) + " -F " + shellQuote(sshPath) + " \"$@\" ;;\n  *'-O exit'*) exit 0 ;;\n  *'sh -s')\n    script=$(cat)\n    case $script in\n      *'os=%s'*) printf 'home=%s\\nos=Linux\\nshell=/bin/bash\\nnc=yes\\nnc_unix=yes\\nnc_shutdown=yes\\ntar=yes\\n' " + shellQuote(remote) + " ;;\n      *) printf '%s\\n' \"$script\" | HOME=" + shellQuote(remote) + " sh ;;\n    esac ;;\n  *) exit 99 ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fake), 0o700); err != nil {
				t.Fatal(err)
			}
			nc := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' " + shellQuote("nc: "+socket+": Connection refused") + " >&2\nexit 1\n"
			if state == "active" {
				nc = "#!/bin/sh\ncat >/dev/null\nexit 0\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "nc"), []byte(nc), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", local)
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			destinations := []string{"debian", "debian.local", "nas.example", "192.0.2.10", "2001:db8::10", "alice@192.0.2.20", "alice@2001:db8::20"}
			for _, alias := range append(append([]string{}, destinations...), "debian") {
				var out, errOut bytes.Buffer
				e := &env{stdout: &out, stderr: &errOut, getenv: func(string) string { return "" }}
				args := []string{alias}
				if state == "print" {
					args = []string{"-print", alias}
				}
				code := cmdSetup(context.Background(), e, &globalOptions{configPath: configPath}, args)
				if state == "invalid-file" {
					if code != exitFailure || !strings.Contains(errOut.String(), "setup files were updated") {
						t.Fatalf("partial failure: %d %s", code, errOut.String())
					}
				} else if code != exitOK {
					t.Fatalf("%s setup failed: %d %s", alias, code, errOut.String())
				}
			}
			content, err := os.ReadFile(sshPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(content), original) {
				t.Fatal("unrelated SSH settings changed")
			}
			_, statErr := os.Lstat(socket)
			if state == "print" {
				if string(content) != original || statErr != nil {
					t.Fatal("print mode mutated config or socket")
				}
				return
			}
			for _, alias := range destinations {
				start, _ := sshMarkers(alias)
				if strings.Count(string(content), start) != 1 {
					t.Fatalf("duplicate or missing alias block: %s", content)
				}
				if err := verifyReconnectForward(context.Background(), alias, socket, filepath.Join(local, "daemon")); err != nil {
					t.Fatal(err)
				}
			}
			if state == "stale" {
				if !os.IsNotExist(statErr) {
					t.Fatalf("stale socket remains: %v", statErr)
				}
			} else if statErr != nil {
				t.Fatalf("protected socket/file removed: %v", statErr)
			}
			rcContent, err := os.ReadFile(rc)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(rcContent), blockStart) != 1 || !strings.HasPrefix(string(rcContent), "# unrelated shell settings\n") {
				t.Fatal("remote function duplicated or unrelated content changed")
			}
		})
	}
}
