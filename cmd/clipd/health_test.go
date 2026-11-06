package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/launchagent"
	"github.com/colefailla/clipd/internal/protocol"
)

func healthSocket(t *testing.T, reply string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				got := make([]byte, len(protocol.Ping))
				if _, err := io.ReadFull(conn, got); err != nil {
					return
				}
				if string(got) != string(protocol.Ping) {
					return
				}
				if reply != "" {
					io.WriteString(conn, reply)
				} else {
					io.Copy(io.Discard, conn)
				}
			}()
		}
	}()
	return path
}

func TestReceiverHealthCheck(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		ok          bool
	}{
		{"clipd", protocol.StatusOK + protocol.Pong + "\n", true},
		{"foreign", "hello\n", false},
		{"silent", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := healthSocket(t, tc.reply)
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := waitForReceiver(ctx, socket)
			if (err == nil) != tc.ok {
				t.Fatalf("health: %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("check exceeded caller deadline")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForReceiver(ctx, "/nonexistent/clipd"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestServiceSocketUsesInstalledEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "installed.json")
	cfg := config.Default()
	cfg.Address = filepath.Join(dir, "socket")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIPD_CONFIG", filepath.Join(dir, "wrong.json"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "wrong"))
	spec := launchagent.Spec{ProgramArguments: []string{"/installed/clipd", "serve"}, EnvironmentVariables: map[string]string{"CLIPD_CONFIG": path}}
	got, err := serviceSocket(spec)
	if err != nil || got != cfg.Address {
		t.Fatalf("socket=%q err=%v", got, err)
	}
	spec.WorkingDirectory = dir
	spec.ProgramArguments = []string{"/installed/clipd", "-config", path, "serve", "-address", "./override.sock"}
	got, err = serviceSocket(spec)
	if err != nil || got != filepath.Join(dir, "override.sock") {
		t.Fatalf("override=%q err=%v", got, err)
	}
}

func TestInstallAndRestartConfirmReceiver(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS service commands")
	}
	oldInstall, oldRestart, oldRead, oldCheck := installAgent, restartAgent, readInstalledAgent, checkReceiver
	t.Cleanup(func() {
		installAgent, restartAgent, readInstalledAgent, checkReceiver = oldInstall, oldRestart, oldRead, oldCheck
	})
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfg := config.Default()
	cfg.Address = filepath.Join(dir, "socket")
	cfg.DropDir = filepath.Join(dir, "drops")
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	installAgent = func(context.Context, launchagent.Options) (launchagent.Result, error) {
		return launchagent.Result{ExecutablePath: "/installed/clipd", PlistPath: "/service.plist", LogPath: "/service.log"}, nil
	}
	restarts := 0
	restartAgent = func(context.Context) error { restarts++; return nil }
	readInstalledAgent = func(context.Context) (launchagent.Spec, error) {
		return launchagent.Spec{ProgramArguments: []string{"/installed/clipd", "serve"}, EnvironmentVariables: map[string]string{"CLIPD_CONFIG": path}, StandardErrPath: "/service.log"}, nil
	}
	for _, healthy := range []bool{true, false} {
		checkReceiver = func(_ context.Context, socket string) error {
			if socket != cfg.Address {
				t.Fatalf("checked wrong socket %s", socket)
			}
			if healthy {
				return nil
			}
			return context.DeadlineExceeded
		}
		for _, command := range []string{"install", "restart"} {
			selected := path
			if command == "restart" {
				selected = filepath.Join(dir, "unrelated.json")
			}
			got := runCLI(t, command, "-config", selected)
			if healthy {
				if got.code != 0 || !strings.Contains(got.stdout, "clipd is responding.") {
					t.Fatalf("%s: %+v", command, got)
				}
				if command == "install" {
					for _, want := range []string{"/installed/clipd", "/service.plist", "/service.log", path, cfg.Address, cfg.DropDir} {
						if !strings.Contains(got.stdout, want) {
							t.Errorf("install omitted %q: %s", want, got.stdout)
						}
					}
				}
			} else if got.code != 1 || !strings.Contains(got.stderr, "/service.log") || !strings.Contains(got.stderr, "within 5 seconds") || strings.Contains(got.stdout, "responding") {
				t.Fatalf("%s: %+v", command, got)
			}
		}
	}
	if restarts != 2 {
		t.Fatalf("restart calls=%d", restarts)
	}
}

func TestReceiverHealthRetriesStartup(t *testing.T) {
	dir, err := os.MkdirTemp("", "ch-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	result := make(chan error, 1)
	go func() { result <- waitForReceiver(context.Background(), socket) }()
	time.Sleep(150 * time.Millisecond)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		frame := make([]byte, len(protocol.Ping))
		if _, err := io.ReadFull(conn, frame); err == nil {
			io.WriteString(conn, protocol.StatusOK+protocol.Pong+"\n")
		}
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiver never became healthy")
	}
}
