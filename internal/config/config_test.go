package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMissingFileYieldsWorkingDefaults is the property that makes `brew
// install` enough: a machine with no config at all still runs a daemon.
func TestMissingFileYieldsWorkingDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the defaults are not a valid configuration: %v", err)
	}
	if cfg.Address != DefaultAddress {
		t.Errorf("Address = %q, want %q", cfg.Address, DefaultAddress)
	}
	if cfg.DropDir != DefaultDropDir {
		t.Errorf("DropDir = %q, want %q", cfg.DropDir, DefaultDropDir)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "clipd", "config.json")
	want := Config{
		Address:         "~/.custom.sock",
		DropDir:         "~/Inbox",
		MaxPayloadBytes: 5 << 20,
		MaxDropBytes:    32 << 20,
		MaxDropFiles:    12,
		MaxConcurrent:   4,
	}
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// TestPartialFileKeepsDefaults: a hand-written config naming one key must not
// blank out every other setting.
func TestPartialFileKeepsDefaults(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"drop_dir":"~/Inbox"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DropDir != "~/Inbox" {
		t.Errorf("DropDir = %q, want the file value", cfg.DropDir)
	}
	if cfg.Address != DefaultAddress {
		t.Errorf("Address = %q, want the default", cfg.Address)
	}
	if cfg.MaxPayloadBytes != DefaultMaxPayloadBytes {
		t.Errorf("MaxPayloadBytes = %d, want the default", cfg.MaxPayloadBytes)
	}
}

// TestUnknownKeysAreRejected: a typo should fail loudly rather than silently
// leaving a default in place, which is the failure nobody notices.
func TestUnknownKeysAreRejected(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"drop_directory":"~/Inbox"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an unknown key")
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*Config){
		"empty address":     func(c *Config) { c.Address = "" },
		"zero payload":      func(c *Config) { c.MaxPayloadBytes = 0 },
		"payload too large": func(c *Config) { c.MaxPayloadBytes = MaxAllowedPayloadBytes + 1 },
		"negative drops":    func(c *Config) { c.MaxDropFiles = -1 },
		"negative bytes":    func(c *Config) { c.MaxDropBytes = -1 },
		"negative workers":  func(c *Config) { c.MaxConcurrent = -1 },
	}
	for name, mutate := range tests {
		cfg := Default()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted %s", name)
		}
	}
}

func TestSavePermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	dir := filepath.Join(t.TempDir(), "clipd")
	path := filepath.Join(dir, "config.json")
	if err := Default().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != FilePerm {
		t.Errorf("file mode = %04o, want %04o", perm, FilePerm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != DirPerm {
		t.Errorf("directory mode = %04o, want %04o", perm, DirPerm)
	}
}

// TestSaveTightensAnExistingDirectory: MkdirAll leaves an existing directory's
// mode alone, so a directory created loosely by something else is not fixed by
// creation and has to be chmod-ed explicitly.
func TestSaveTightensAnExistingDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	dir := filepath.Join(t.TempDir(), "clipd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := Default().Save(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != DirPerm {
		t.Errorf("directory mode = %04o, want %04o", perm, DirPerm)
	}
}

func TestResolvePathPrefersTheFlag(t *testing.T) {
	t.Parallel()

	got, err := ResolvePath("/explicit/path.json")
	if err != nil || got != "/explicit/path.json" {
		t.Errorf("ResolvePath = %q, %v; want the flag value", got, err)
	}
}

// TestResolvePathFromEnvironment cannot run in parallel: t.Setenv mutates
// process-wide state.
func TestResolvePathFromEnvironment(t *testing.T) {
	t.Setenv(EnvConfig, "/from/env.json")

	if got, err := ResolvePath(""); err != nil || got != "/from/env.json" {
		t.Errorf("ResolvePath = %q, %v; want the environment value", got, err)
	}
	if got, err := ResolvePath("/explicit.json"); err != nil || got != "/explicit.json" {
		t.Errorf("the flag must win over the environment, got %q, %v", got, err)
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()

	good := map[string]int64{
		"1024": 1024, "1KB": 1 << 10, "1KiB": 1 << 10,
		"10MB": 10 << 20, "1GB": 1 << 30, " 5 MB ": 5 << 20, "2m": 2 << 20,
	}
	for give, want := range good {
		got, err := ParseSize(give)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", give, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", give, got, want)
		}
	}

	for _, give := range []string{"", "-1", "0", "abc", "10PB", "9999999999999999999GB"} {
		if _, err := ParseSize(give); err == nil {
			t.Errorf("ParseSize(%q) succeeded, want an error", give)
		}
	}
}

func TestFormatSize(t *testing.T) {
	t.Parallel()

	tests := map[int64]string{
		512: "512 bytes", 1 << 10: "1 KiB", 10 << 20: "10 MiB", 1 << 30: "1 GiB",
	}
	for give, want := range tests {
		if got := FormatSize(give); !strings.HasPrefix(got, strings.Fields(want)[0]) {
			t.Errorf("FormatSize(%d) = %q, want something like %q", give, got, want)
		}
	}
}
