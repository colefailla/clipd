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

func TestLoadRejectsTrailingData(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"second object":         `{"address":"~/.first.sock"} {"address":"~/.second.sock"}`,
		"extra closing brace":   `{"address":"~/.clipd.sock"}}`,
		"extra closing bracket": `{"address":"~/.clipd.sock"}]`,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("Load accepted %q", data)
			}
		})
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

// TestSaveTightensTheExistingDefaultDirectory: the default app directory is
// clipd's to restrict even when another invocation created it first.
func TestSaveTightensTheExistingDefaultDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := Default().Save(path); err != nil {
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

func TestSaveLeavesCustomClipdDirectoryModeAlone(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	dir := filepath.Join(t.TempDir(), "shared", AppName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := Default().Save(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("custom directory mode = %04o, want 0755", perm)
	}
}

func TestSavePreservesConfigSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks may require elevated privileges on Windows")
	}

	root := t.TempDir()
	target := filepath.Join(root, "dotfiles", "clipd.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	linkDir := filepath.Join(root, "config")
	if err := os.Mkdir(linkDir, 0o755); err != nil {
		t.Fatalf("mkdir link directory: %v", err)
	}
	link := filepath.Join(linkDir, FileName)
	linkTarget, err := filepath.Rel(linkDir, target)
	if err != nil {
		t.Fatalf("relative target: %v", err)
	}
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	want := Default()
	want.DropDir = "~/FromDotfiles"
	if err := want.Save(link); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Save replaced the config symlink")
	}
	got, err := Load(target)
	if err != nil {
		t.Fatalf("Load target: %v", err)
	}
	if got != want {
		t.Errorf("target config = %+v, want %+v", got, want)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat target: %v", err)
		}
		if perm := info.Mode().Perm(); perm != FilePerm {
			t.Errorf("target mode = %04o, want %04o", perm, FilePerm)
		}
	}
}

func TestSaveRefusesDanglingConfigSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks may require elevated privileges on Windows")
	}

	dir := t.TempDir()
	link := filepath.Join(dir, FileName)
	if err := os.Symlink(filepath.Join(dir, "missing.json"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := Default().Save(link); err == nil {
		t.Fatal("Save accepted a dangling config symlink")
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat dangling symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dangling symlink was replaced: mode=%v", info.Mode())
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

// TestV2ConfigGetsAMigrationMessage: strict parsing is right, but "unknown
// field" tells someone upgrading nothing. This is the one case worth naming.
func TestV2ConfigGetsAMigrationMessage(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	v2 := `{"server_address":"mac.local","port":8199,"token":"secret",
	        "server_fingerprint":"sha256:aa","tls_cert_path":"","timeout_ms":5000}`
	if err := os.WriteFile(path, []byte(v2), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a v2 config")
	}
	for _, want := range []string{"v2 config", "token", "mv "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

// TestUnknownKeyStillReportsGenerically: the v2 check must not swallow an
// ordinary typo into a misleading upgrade message.
func TestUnknownKeyStillReportsGenerically(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"drop_directory":"~/Inbox"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := Load2Err(t, path)
	if strings.Contains(err.Error(), "v2 config") {
		t.Errorf("a typo was reported as a v2 config: %v", err)
	}
	if !strings.Contains(err.Error(), "drop_directory") {
		t.Errorf("err = %v, want it to name the unknown key", err)
	}
}

// Load2Err is Load, asserting that it failed.
func Load2Err(t *testing.T, path string) error {
	t.Helper()
	if _, err := Load(path); err != nil {
		return err
	}
	t.Fatalf("Load(%s) succeeded, want an error", path)
	return nil
}

// TestDefaultPathIsXDGShaped pins the move off ~/Library/Application Support.
//
// clipd is a command-line daemon, and the tools it sits beside — git, gh, btop,
// clipper — all keep their config in ~/.config on macOS too. Using one path on
// both platforms also spares every shell example an escaped space.
func TestDefaultPathIsXDGShaped(t *testing.T) {
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if strings.Contains(path, "Application Support") {
		t.Errorf("DefaultPath = %q, want it out of Application Support", path)
	}
	if !strings.Contains(path, filepath.Join(".config", AppName, FileName)) {
		t.Errorf("DefaultPath = %q, want it under .config/%s", path, AppName)
	}
}

// TestXDGConfigHomeWins: honouring the variable is the whole point of it.
func TestXDGConfigHomeWins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/somewhere/else")

	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := filepath.Join("/somewhere/else", AppName, FileName)
	if path != want {
		t.Errorf("DefaultPath = %q, want %q", path, want)
	}
}

func TestRelativeXDGConfigHomeIsRejected(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	if _, err := DefaultPath(); err == nil {
		t.Fatal("DefaultPath accepted a relative XDG_CONFIG_HOME")
	}
}

// TestResolvePathDoesNotFailOverALegacyConfig: an upgrader's old config holds
// only settings v3 removed, so defaults are the right answer. Failing here
// would crash-loop the daemon under launchd's KeepAlive for no gain.
func TestResolvePathDoesNotFailOverALegacyConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if _, err := ResolvePath(""); err != nil {
		t.Errorf("ResolvePath: %v", err)
	}
}
