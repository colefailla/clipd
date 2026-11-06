package main

import (
	"context"
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

func TestStatusPreservesDetails(t *testing.T) {
	oldStatus, oldRead := agentStatus, readInstalledAgent
	t.Cleanup(func() { agentStatus, readInstalledAgent = oldStatus, oldRead })
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	socket := healthSocket(t, protocol.StatusOK+protocol.Pong+"\n")
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Address = socket
	cfg.DropDir = filepath.Join(dir, "drops")
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	agentStatus = func(context.Context) (launchagent.State, error) {
		return launchagent.State{PlistInstalled: true, PlistPath: "/service.plist", Loaded: true, PID: 12345, LastExitStatus: 7, LogPath: "/service.log"}, nil
	}
	readInstalledAgent = func(context.Context) (launchagent.Spec, error) {
		return launchagent.Spec{ProgramArguments: []string{"/installed/clipd", "serve"}, EnvironmentVariables: map[string]string{"CLIPD_CONFIG": path}}, nil
	}
	got := runCLI(t, "status", "-config", path)
	wants := []string{"clipd " + version, "Receiver is responding.", socket, "0600", path, cfg.DropDir, "10 MiB", "256 MiB", "256 files", "concurrent   8", "30m", "Limits reflect the config file"}
	if runtime.GOOS == "darwin" {
		wants = append(wants, "pid 12345", "/installed/clipd", "/service.plist", "/service.log", "last exit    7", "Config changed since startup.")
	}
	if got.code != 0 {
		t.Fatalf("status: %+v", got)
	}
	for _, want := range wants {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("missing %q in %s", want, got.stdout)
		}
	}
	cfg.Address = filepath.Join(dir, "missing.sock")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	got = runCLI(t, "status", "-config", path)
	if got.code != 1 || !strings.Contains(got.stdout, "Configured limits:") || !strings.Contains(got.stdout, cfg.DropDir) {
		t.Fatalf("failure hid details: %+v", got)
	}
}
