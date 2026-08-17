package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load of a missing file returned an error: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("port = %d, want %d", cfg.Port, DefaultPort)
	}
	if cfg.BindAddress != DefaultBindAddress {
		t.Errorf("bind address = %q, want %q", cfg.BindAddress, DefaultBindAddress)
	}
	// A loopback default would make the entire remote use case impossible
	// out of the box, so guard against it regressing.
	if strings.HasPrefix(cfg.BindAddress, "127.") || cfg.BindAddress == "localhost" {
		t.Errorf("bind address %q is loopback-only", cfg.BindAddress)
	}
	if cfg.Token != "" {
		t.Error("a fresh config must not invent a token")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "config.json")
	want := Config{
		ServerAddress:   "192.168.1.50",
		Port:            9000,
		BindAddress:     "0.0.0.0",
		Token:           "a-token-of-sufficient-length",
		MaxPayloadBytes: 2 << 20,
		TimeoutMS:       7500,
	}
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestSavePermissions is the one that matters for the token: the file holds it
// in plaintext, so anything readable beyond the owner is a leak.
func TestSavePermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	dir := filepath.Join(t.TempDir(), "clipd")
	path := filepath.Join(dir, "config.json")

	cfg := Default()
	cfg.Token = "a-token-of-sufficient-length"
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != FilePerm {
		t.Errorf("config file mode = %04o, want %04o", got, FilePerm)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != DirPerm {
		t.Errorf("config directory mode = %04o, want %04o", got, DirPerm)
	}
}

// TestSaveTightensExistingDirectory covers the upgrade case: a directory that
// already exists with loose permissions is not fixed by MkdirAll.
func TestSaveTightensExistingDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	dir := filepath.Join(t.TempDir(), "clipd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("prepare directory: %v", err)
	}

	cfg := Default()
	cfg.Token = "a-token-of-sufficient-length"
	if err := cfg.Save(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	if got := info.Mode().Perm(); got != DirPerm {
		t.Errorf("directory mode = %04o, want %04o", got, DirPerm)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := Default()
	cfg.Token = "a-token-of-sufficient-length"
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("Save left a temporary file behind: %s", entry.Name())
		}
	}
}

func TestLoadPartialConfigKeepsDefaults(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server_address":"mac.local"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ServerAddress != "mac.local" {
		t.Errorf("server address = %q", cfg.ServerAddress)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("port = %d, want the default %d", cfg.Port, DefaultPort)
	}
	if cfg.MaxPayloadBytes != DefaultMaxPayloadBytes {
		t.Errorf("max payload = %d, want the default %d", cfg.MaxPayloadBytes, DefaultMaxPayloadBytes)
	}
}

// TestLoadRejectsUnknownFields keeps a typo from silently leaving the user on
// a default they did not intend.
func TestLoadRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server_adress":"typo.local"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load accepted a misspelled field")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load accepted malformed JSON")
	}
}

func TestApplyEnv(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		EnvServer:     "10.0.0.5",
		EnvPort:       "9999",
		EnvBind:       "192.168.1.10",
		EnvToken:      "env-token-value-long-enough",
		EnvMaxPayload: "2MB",
		EnvTimeout:    "12s",
	}

	cfg := Default()
	if err := cfg.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}

	if cfg.ServerAddress != "10.0.0.5" {
		t.Errorf("server = %q", cfg.ServerAddress)
	}
	if cfg.Port != 9999 {
		t.Errorf("port = %d", cfg.Port)
	}
	if cfg.BindAddress != "192.168.1.10" {
		t.Errorf("bind = %q", cfg.BindAddress)
	}
	if cfg.Token != "env-token-value-long-enough" {
		t.Errorf("token = %q", cfg.Token)
	}
	if cfg.MaxPayloadBytes != 2<<20 {
		t.Errorf("max payload = %d", cfg.MaxPayloadBytes)
	}
	if cfg.Timeout() != 12*time.Second {
		t.Errorf("timeout = %s", cfg.Timeout())
	}
}

func TestApplyEnvRejectsBadValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
	}{
		{"port", map[string]string{EnvPort: "eight-thousand"}},
		{"max payload", map[string]string{EnvMaxPayload: "lots"}},
		// A bare number is ambiguous, and guessing wrong turns a 30 second
		// timeout into 30 nanoseconds.
		{"timeout without a unit", map[string]string{EnvTimeout: "30"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			if err := cfg.ApplyEnv(func(k string) string { return tc.env[k] }); err == nil {
				t.Error("ApplyEnv accepted an invalid value")
			}
		})
	}
}

func TestApplyEnvIgnoresUnsetVariables(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.ServerAddress = "from-file.local"
	if err := cfg.ApplyEnv(func(string) string { return "" }); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.ServerAddress != "from-file.local" {
		t.Errorf("an unset variable overwrote the file value: %q", cfg.ServerAddress)
	}
}

// TestOverrideEnvMatchesApplyEnv keeps the list and the code that reads it in
// step, by watching which names ApplyEnv actually asks for.
//
// Nothing fails loudly when they diverge: commands that decline the overlay
// report which overrides they are ignoring by ranging over OverrideEnv, so a
// variable missing from the list is one the user is told nothing about while
// it is quietly dropped.
func TestOverrideEnvMatchesApplyEnv(t *testing.T) {
	t.Parallel()

	asked := make(map[string]bool)
	cfg := Default()
	if err := cfg.ApplyEnv(func(name string) string {
		asked[name] = true
		return ""
	}); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}

	listed := make(map[string]bool, len(OverrideEnv))
	for _, name := range OverrideEnv {
		if listed[name] {
			t.Errorf("OverrideEnv lists %s twice", name)
		}
		listed[name] = true
		if !asked[name] {
			t.Errorf("OverrideEnv lists %s, which ApplyEnv never reads", name)
		}
	}
	for name := range asked {
		if !listed[name] {
			t.Errorf("ApplyEnv reads %s, which OverrideEnv does not list", name)
		}
	}
	if listed[EnvConfig] {
		t.Errorf("%s selects which file to read rather than overriding a value in it, so it must stay out of OverrideEnv", EnvConfig)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	valid := func() Config {
		c := Default()
		c.ServerAddress = "mac.local"
		c.Token = "a-token-of-sufficient-length"
		c.ServerFingerprint = "sha256:" + strings.Repeat("ab", 32)
		return c
	}

	if err := valid().ValidateClient(); err != nil {
		t.Errorf("ValidateClient on a good config: %v", err)
	}
	if err := valid().ValidateServer(); err != nil {
		t.Errorf("ValidateServer on a good config: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		client bool
		server bool
	}{
		{"no server address", func(c *Config) { c.ServerAddress = "" }, true, false},
		// A client with no pin cannot verify what it is talking to, which is
		// not better than plaintext.
		{"no fingerprint", func(c *Config) { c.ServerFingerprint = "" }, true, false},
		{"no token", func(c *Config) { c.Token = "" }, true, true},
		{"no bind address", func(c *Config) { c.BindAddress = "" }, false, true},
		{"port zero", func(c *Config) { c.Port = 0 }, true, true},
		{"port too high", func(c *Config) { c.Port = 70000 }, true, true},
		{"payload zero", func(c *Config) { c.MaxPayloadBytes = 0 }, true, true},
		{"payload above the ceiling", func(c *Config) { c.MaxPayloadBytes = MaxAllowedPayloadBytes + 1 }, true, true},
		{"timeout zero", func(c *Config) { c.TimeoutMS = 0 }, true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid()
			tc.mutate(&cfg)
			if tc.client && cfg.ValidateClient() == nil {
				t.Error("ValidateClient accepted an invalid config")
			}
			if tc.server && cfg.ValidateServer() == nil {
				t.Error("ValidateServer accepted an invalid config")
			}
		})
	}
}

func TestAddressFormatting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		server     string
		bind       string
		port       int
		wantDial   string
		wantListen string
	}{
		{"ipv4", "192.168.1.50", "0.0.0.0", 8199, "192.168.1.50:8199", "0.0.0.0:8199"},
		{"hostname", "mac.local", "0.0.0.0", 8199, "mac.local:8199", "0.0.0.0:8199"},
		// A bare IPv6 literal concatenated with :port would be unparseable.
		{"ipv6", "fd00::1", "::", 8199, "[fd00::1]:8199", "[::]:8199"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{ServerAddress: tc.server, BindAddress: tc.bind, Port: tc.port}
			if got := cfg.DialAddress(); got != tc.wantDial {
				t.Errorf("DialAddress = %q, want %q", got, tc.wantDial)
			}
			if got := cfg.ListenAddress(); got != tc.wantListen {
				t.Errorf("ListenAddress = %q, want %q", got, tc.wantListen)
			}
		})
	}
}

func TestTimeoutFallback(t *testing.T) {
	t.Parallel()

	if got := (Config{TimeoutMS: 0}).Timeout(); got != DefaultTimeout {
		t.Errorf("Timeout with zero ms = %s, want %s", got, DefaultTimeout)
	}
	if got := (Config{TimeoutMS: -5}).Timeout(); got != DefaultTimeout {
		t.Errorf("Timeout with negative ms = %s, want %s", got, DefaultTimeout)
	}
	if got := (Config{TimeoutMS: 250}).Timeout(); got != 250*time.Millisecond {
		t.Errorf("Timeout = %s", got)
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1024", 1024, false},
		{"1KB", 1 << 10, false},
		{"1kb", 1 << 10, false},
		{"1KiB", 1 << 10, false},
		{"10MB", 10 << 20, false},
		{"10 MB", 10 << 20, false},
		{"1G", 1 << 30, false},
		{"512B", 512, false},
		{"", 0, true},
		{"lots", 0, true},
		{"0", 0, true},
		{"-5MB", 0, true},
		{"2GB", 0, true}, // above the ceiling
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ParseSize(%q) = %d, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSize(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	t.Parallel()

	if got, err := ParseDuration("5s"); err != nil || got != 5*time.Second {
		t.Errorf("ParseDuration(5s) = %s, %v", got, err)
	}
	for _, bad := range []string{"", "5", "-1s", "0s", "soon"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) accepted an invalid duration", bad)
		}
	}
}

func TestFormatSize(t *testing.T) {
	t.Parallel()

	tests := map[int64]string{
		512:      "512 bytes",
		2 << 10:  "2 KiB",
		10 << 20: "10 MiB",
		1 << 30:  "1 GiB",
	}
	for in, want := range tests {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestResolvePathPrefersTheFlag(t *testing.T) {
	t.Parallel()

	if got, err := ResolvePath("/explicit/path.json"); err != nil || got != "/explicit/path.json" {
		t.Errorf("ResolvePath with a flag = %q, %v", got, err)
	}
}

// TestResolvePathFromEnvironment cannot run in parallel: t.Setenv mutates
// process-wide state.
func TestResolvePathFromEnvironment(t *testing.T) {
	t.Setenv(EnvConfig, "/from/env.json")

	if got, err := ResolvePath(""); err != nil || got != "/from/env.json" {
		t.Errorf("ResolvePath from the environment = %q, %v", got, err)
	}
	if got, err := ResolvePath("/explicit/path.json"); err != nil || got != "/explicit/path.json" {
		t.Errorf("the flag must win over the environment, got %q, %v", got, err)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Parallel()

	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("DefaultPath = %q, want an absolute path", path)
	}
	if filepath.Base(path) != FileName {
		t.Errorf("DefaultPath = %q, want it to end in %s", path, FileName)
	}
	if filepath.Base(filepath.Dir(path)) != AppName {
		t.Errorf("DefaultPath = %q, want it inside a %s directory", path, AppName)
	}
}

func TestExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if Exists(filepath.Join(dir, "nope.json")) {
		t.Error("Exists reported a missing file as present")
	}
	if Exists(dir) {
		t.Error("Exists reported a directory as a config file")
	}

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !Exists(path) {
		t.Error("Exists reported an existing file as missing")
	}
}

func TestMemoryBudgetDerivation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload int64
		memory  int64
		want    int64
	}{
		{"unset uses the default", DefaultMaxPayloadBytes, 0, DefaultMaxMemoryBytes},
		{"explicit value wins", DefaultMaxPayloadBytes, 128 << 20, 128 << 20},
		// A payload limit above the default budget has to raise the budget, or
		// the server would reject copies at the very limit it advertises.
		{"large payload raises the floor", 512 << 20, 0, 512 << 20},
		{"payload just under the default", 63 << 20, 0, DefaultMaxMemoryBytes},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := Default()
			cfg.MaxPayloadBytes = tc.payload
			cfg.MaxMemoryBytes = tc.memory
			if got := cfg.MemoryBudget(); got != tc.want {
				t.Errorf("MemoryBudget() = %d, want %d", got, tc.want)
			}
			if got := cfg.MemoryBudget(); got < cfg.MaxPayloadBytes {
				t.Errorf("budget %d is below the payload limit %d; no copy at the limit could run",
					got, cfg.MaxPayloadBytes)
			}
		})
	}
}

func TestConcurrencyDerivation(t *testing.T) {
	t.Parallel()

	cfg := Default()
	if got := cfg.Concurrency(); got != DefaultMaxConcurrent {
		t.Errorf("Concurrency() = %d, want the default %d", got, DefaultMaxConcurrent)
	}
	cfg.MaxConcurrent = 8
	if got := cfg.Concurrency(); got != 8 {
		t.Errorf("Concurrency() = %d, want 8", got)
	}
}

func TestValidateServerChecksResourceLimits(t *testing.T) {
	t.Parallel()

	base := func() Config {
		c := Default()
		c.Token = "token-of-sufficient-length"
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"budget below one payload", func(c *Config) {
			c.MaxPayloadBytes = 10 << 20
			c.MaxMemoryBytes = 1 << 20
		}, "smaller than max payload"},
		{"budget over the ceiling", func(c *Config) {
			c.MaxMemoryBytes = MaxAllowedMemoryBytes + 1
		}, "ceiling"},
		{"negative budget", func(c *Config) { c.MaxMemoryBytes = -1 }, "must be positive"},
		{"negative concurrency", func(c *Config) { c.MaxConcurrent = -1 }, "out of range"},
		{"absurd concurrency", func(c *Config) { c.MaxConcurrent = MaxAllowedConcurrent + 1 }, "out of range"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := base()
			tc.mutate(&cfg)
			err := cfg.ValidateServer()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}

	// The defaults, and an explicit budget at exactly one payload, are fine.
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"defaults", func(*Config) {}},
		{"budget exactly one payload", func(c *Config) { c.MaxMemoryBytes = c.MaxPayloadBytes }},
		{"budget at the ceiling", func(c *Config) { c.MaxMemoryBytes = MaxAllowedMemoryBytes }},
		{"concurrency at the ceiling", func(c *Config) { c.MaxConcurrent = MaxAllowedConcurrent }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := base()
			tc.mutate(&cfg)
			if err := cfg.ValidateServer(); err != nil {
				t.Errorf("ValidateServer() = %v, want nil", err)
			}
		})
	}
}

func TestParseSizeUpToRespectsItsCeiling(t *testing.T) {
	t.Parallel()

	// The memory ceiling is above the payload ceiling, so a value legal for one
	// must be rejected by the other.
	const twoGiB = "2GB"
	if _, err := ParseSize(twoGiB); err == nil {
		t.Error("ParseSize accepted a value above the payload ceiling")
	}
	got, err := ParseSizeUpTo(twoGiB, MaxAllowedMemoryBytes)
	if err != nil {
		t.Fatalf("ParseSizeUpTo(%q): %v", twoGiB, err)
	}
	if want := int64(2 << 30); got != want {
		t.Errorf("= %d, want %d", got, want)
	}

	// A value large enough to overflow int64 when multiplied by its suffix
	// must be rejected, not wrapped into something that looks acceptable.
	if _, err := ParseSizeUpTo("9000000000000000000GB", MaxAllowedMemoryBytes); err == nil {
		t.Error("an overflowing size was accepted")
	}
}

func TestEnvOverridesResourceLimits(t *testing.T) {
	t.Parallel()

	cfg := Default()
	env := map[string]string{
		EnvMaxConn:   "32",
		EnvMaxMemory: "256MB",
	}
	if err := cfg.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.MaxConcurrent != 32 {
		t.Errorf("MaxConcurrent = %d, want 32", cfg.MaxConcurrent)
	}
	if want := int64(256 << 20); cfg.MaxMemoryBytes != want {
		t.Errorf("MaxMemoryBytes = %d, want %d", cfg.MaxMemoryBytes, want)
	}

	bad := map[string]string{EnvMaxConn: "lots"}
	broken := Default()
	if err := broken.ApplyEnv(func(k string) string { return bad[k] }); err == nil {
		t.Error("a non-numeric concurrency was accepted")
	}
}
