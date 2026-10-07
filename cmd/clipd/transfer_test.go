package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/server"
)

func TestGeneratedClientStreamsToRealReceiver(t *testing.T) {
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc unavailable")
	}
	for _, shell := range []string{"sh", "dash", "bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			binary, err := exec.LookPath(shell)
			if err != nil {
				t.Skip("shell unavailable")
			}
			dir, err := os.MkdirTemp("", "clipd-e2e-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "socket")
			destination := filepath.Join(dir, "received")
			fake := &clipboard.Fake{}
			srv, err := server.New(server.Options{Clipboard: fake, DropDir: destination, MaxPayload: 1024, MaxDropBytes: 1 << 20, MaxTransfer: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			ln, err := server.Listen(socket)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ctx, ln) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			book := filepath.Join(dir, "author", "book title")
			if err := os.MkdirAll(filepath.Join(book, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"audio.m4b": "audio", "nested/quote\".cue": "cue", "--use-compress-program=bad": "safe"} {
				if err := os.WriteFile(filepath.Join(book, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("/etc/passwd", filepath.Join(book, "link")); err != nil {
				t.Fatal(err)
			}
			facts := remoteFacts{os: "Linux", hasNC: true, ncUnix: true, ncShutdown: true}
			if runtime.GOOS == "darwin" {
				facts.os = "Darwin"
			}
			client, err := clientCommand(facts)
			if err != nil {
				t.Fatal(err)
			}
			block := shellFunction(client, socket, true)
			script := "set -u\n" + block + "\nclipd drop 'author/book title'\nclipd drop 'author/book title/audio.m4b'\nclipd drop 'author/book title'\n"
			command := exec.CommandContext(ctx, binary, "-c", script)
			command.Dir = dir
			out, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("transfer failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "skipped 1 symlink or other special file") {
				t.Fatalf("symlink skip not reported: %s", out)
			}
			if _, err := os.Lstat(filepath.Join(destination, "book title", "link")); !os.IsNotExist(err) {
				t.Fatalf("symlink was recreated: %v", err)
			}
			for _, name := range []string{"book title/audio.m4b", "book title/nested/quote\".cue", "book title/--use-compress-program=bad", "audio.m4b", "book title-1/audio.m4b"} {
				if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
					t.Fatalf("missing %s: %v", name, err)
				}
			}
			failed := exec.CommandContext(ctx, binary, "-c", block+"\nclipd drop 'author/book title/audio.m4b' missing\n")
			failed.Dir = dir
			output, err := failed.CombinedOutput()
			if err == nil {
				t.Fatalf("failed tar reported success: %s", output)
			}
			if _, err := os.Stat(filepath.Join(destination, "audio-1.m4b")); !os.IsNotExist(err) {
				t.Fatal("partial transfer was published")
			}
			if fake.WriteCount() != 0 {
				t.Fatal("structured transfer touched clipboard")
			}
			if strings.Contains(string(out), "clipd: progress:") {
				t.Fatal("noninteractive transfer printed progress")
			}
			if runtime.GOOS == "darwin" {
				if scriptPath, err := exec.LookPath("script"); err == nil {
					ttyCtx, stop := context.WithTimeout(ctx, 10*time.Second)
					tty := exec.CommandContext(ttyCtx, scriptPath, "-q", "/dev/null", binary, "-c", block+"\nclipd drop 'author/book title/audio.m4b'\n")
					tty.Dir = dir
					ttyOutput, ttyErr := tty.CombinedOutput()
					stop()
					if ttyErr != nil || !strings.Contains(string(ttyOutput), "clipd: progress:") || !strings.Contains(string(ttyOutput), "clipd: ok:") {
						t.Fatalf("interactive progress: %s, %v", ttyOutput, ttyErr)
					}
				}
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "drop.") {
					t.Fatal("client created a payload spool")
				}
			}
		})
	}
}

func TestRemoteHelpWorksWithoutForward(t *testing.T) {
	block := shellFunction(`nc -U "$_clipd_sock"`, "/nonexistent/socket", true)
	out, err := exec.Command("sh", "-c", block+"\nclipd -h\n").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "clipd restart") {
		t.Fatalf("help = %s, %v", out, err)
	}
}

func TestNamedInputDoesNotRequireTar(t *testing.T) {
	dir, err := os.MkdirTemp("", "clipd-no-tar-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := server.Listen(filepath.Join(dir, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	block := shellFunction(`printf 'clipd: ok: dropped\n'`, filepath.Join(dir, "socket"), false)
	out, err := exec.Command("sh", "-c", block+"\nprintf payload | clipd drop --name report.txt\n").CombinedOutput()
	if err != nil {
		t.Fatalf("named input required tar: %s, %v", out, err)
	}
}

func TestClientRejectsAmbiguousReplies(t *testing.T) {
	dir, err := os.MkdirTemp("", "clipd-reply-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "socket")
	ln, err := server.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, reply := range []string{"clipd: ok: first\nclipd: error: second\n", "clipd: ok: first\nclipd: progress: late\n", "unexpected\n", ""} {
		client := filepath.Join(dir, "client")
		if err := os.WriteFile(client, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' "+shellQuote(reply)+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		block := shellFunction(shellQuote(client), socket, true)
		out, err := exec.Command("sh", "-c", block+"\nprintf payload | clipd\n").CombinedOutput()
		if err == nil {
			t.Fatalf("accepted ambiguous reply %q: %s", reply, out)
		}
	}
}

// staleSocket leaves a socket file with nothing bound to it, as sshd does when
// a session ends.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientRemovesStaleSocket(t *testing.T) {
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc unavailable")
	}
	facts := remoteFacts{os: "Linux", hasNC: true, ncUnix: true, ncShutdown: true}
	if runtime.GOOS == "darwin" {
		facts.os = "Darwin"
	}
	client, err := clientCommand(facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, shell := range []string{"sh", "dash", "bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			binary, err := exec.LookPath(shell)
			if err != nil {
				t.Skip("shell unavailable")
			}
			dir, err := os.MkdirTemp("", "clipd-stale-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "socket")
			if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("body"), 0600); err != nil {
				t.Fatal(err)
			}
			block := shellFunction(client, socket, true)
			for _, use := range []string{"printf hi | clipd", "clipd drop file.txt", "printf hi | clipd drop --name x.txt"} {
				staleSocket(t, socket)
				command := exec.Command(binary, "-c", "set -u\n"+block+"\n"+use+"\n")
				command.Dir = dir
				out, err := command.CombinedOutput()
				if err == nil {
					t.Fatalf("%s: succeeded against a stale socket: %s", use, out)
				}
				if !strings.Contains(string(out), "left behind by an earlier SSH session") {
					t.Fatalf("%s: stale socket not explained: %s", use, out)
				}
				if strings.Contains(string(out), "tar failed") || strings.Contains(string(out), "did not complete") {
					t.Fatalf("%s: misleading failure message: %s", use, out)
				}
				if _, err := os.Lstat(socket); !os.IsNotExist(err) {
					t.Fatalf("%s: stale socket was not removed: %v", use, err)
				}
			}
		})
	}
}

// A forward whose Mac daemon is down accepts the connection and then closes
// it. That socket belongs to a live session and must survive.
func TestClientPreservesLiveSocketWithoutReply(t *testing.T) {
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc unavailable")
	}
	facts := remoteFacts{os: "Linux", hasNC: true, ncUnix: true, ncShutdown: true}
	if runtime.GOOS == "darwin" {
		facts.os = "Darwin"
	}
	client, err := clientCommand(facts)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "clipd-live-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "socket")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	out, err := exec.Command("sh", "-c", shellFunction(client, socket, true)+"\nprintf hi | clipd\n").CombinedOutput()
	if err == nil {
		t.Fatalf("succeeded without a reply: %s", out)
	}
	if !strings.Contains(string(out), "did not complete") || strings.Contains(string(out), "left behind") {
		t.Fatalf("live socket failure misreported: %s", out)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("live socket was removed: %v", err)
	}
}
