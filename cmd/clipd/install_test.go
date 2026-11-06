package main

import (
	"context"
	"errors"
	"github.com/colefailla/clipd/internal/launchagent"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallRejectsInvalidConfigBeforeMutation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS installation")
	}
	previous := installAgent
	defer func() { installAgent = previous }()
	called := false
	installAgent = func(context.Context, launchagent.Options) (launchagent.Result, error) {
		called = true
		return launchagent.Result{}, errors.New("unexpected installation")
	}
	for _, config := range []string{
		`{"max_payload_bytes":1073741824,"max_concurrent":8}`,
		`{"address":"./clipd.sock"}`,
		`{"drop_dir":"Drop"}`,
		`{"drop_dir":"~someone/Drop"}`,
	} {
		t.Run(config, func(t *testing.T) {
			called = false
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, "config.json")
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			got := runCLI(t, "install", "-config", path)
			if called {
				t.Fatal("invalid config reached service installation")
			}
			if got.code != exitConfig {
				t.Fatalf("%+v", got)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != config {
				t.Fatal("config changed")
			}
			if _, err := os.Stat(filepath.Join(home, "Library")); !os.IsNotExist(err) {
				t.Fatal("installation reached service mutation")
			}
			if !strings.Contains(got.stderr, "max_payload_bytes") && !strings.Contains(got.stderr, "path") {
				t.Fatalf("unhelpful error: %s", got.stderr)
			}
		})
	}
}

func TestInstallAcceptsStablePaths(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS installation")
	}
	previous := installAgent
	defer func() { installAgent = previous }()
	for _, dir := range []string{"~/Drop", "/tmp/clipd-unused-drop"} {
		t.Run(dir, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, "config.json")
			if err := os.WriteFile(path, []byte(`{"drop_dir":"`+dir+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			called := false
			installAgent = func(_ context.Context, opts launchagent.Options) (launchagent.Result, error) {
				called = true
				if opts.ConfigPath != path {
					t.Errorf("config path = %q", opts.ConfigPath)
				}
				return launchagent.Result{}, errors.New("test stops before launchctl")
			}
			got := runCLI(t, "install", "-config", path)
			if !called || got.code != exitFailure {
				t.Fatalf("%+v; called=%v", got, called)
			}
		})
	}
}
