package main

import (
	"context"
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
