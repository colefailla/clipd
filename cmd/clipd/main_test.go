package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/server"
	"github.com/colefailla/clipd/internal/transport"
)

const testToken = "test-token-of-sufficient-length"

type result struct {
	code   int
	stdout string
	stderr string
}

// exec runs the CLI with an isolated environment and captures its output.
func exec(t *testing.T, stdin string, piped bool, args ...string) result {
	t.Helper()
	// An empty environment keeps the developer's real CLIPD_* variables
	// from leaking into the tests.
	return execEnv(t, stdin, piped, func(string) string { return "" }, args...)
}

// execEnv is exec with an explicit fake environment, for tests about the
// CLIPD_* variables themselves.
func execEnv(t *testing.T, stdin string, piped bool, getenv func(string) string, args ...string) result {
	t.Helper()

	var out, errOut bytes.Buffer
	e := &env{
		stdin:       strings.NewReader(stdin),
		stdout:      &out,
		stderr:      &errOut,
		stdinIsPipe: piped,
		getenv:      getenv,
	}
	code := run(context.Background(), args, e)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// writeConfig creates a config file and returns its path.
func writeConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return path
}

// startDaemon runs a real server on a loopback port with a fake clipboard.
func startDaemon(t *testing.T) (addr string, clip *clipboard.Fake, fingerprint string) {
	t.Helper()

	tlsConfig, pin, err := transport.Ephemeral()
	if err != nil {
		t.Fatalf("generate test certificate: %v", err)
	}

	clip = &clipboard.Fake{}
	srv, err := server.New(server.Options{
		Token:      testToken,
		TLS:        tlsConfig,
		Clipboard:  clip,
		MaxPayload: 1 << 20,
		Timeout:    2 * time.Second,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ln, err := server.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("server.Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ln.Addr().String(), clip, transport.FormatFingerprint(pin)
}

// TestEndToEndCopy is the whole point of the tool: piped stdin, no
// subcommand, bytes land on the daemon's clipboard.
func TestEndToEndCopy(t *testing.T) {
	t.Parallel()

	addr, clip, fingerprint := startDaemon(t)
	host, port := splitHostPort(t, addr)

	cfg := config.Default()
	cfg.ServerAddress = host
	cfg.Port = port
	cfg.Token = testToken
	cfg.ServerFingerprint = fingerprint
	path := writeConfig(t, cfg)

	input := "CONTAINER ID   IMAGE     STATUS\nabc123         nginx     Up 2 hours\n"
	got := exec(t, input, true, "-config", path)

	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	// Silence on success, like pbcopy: anything on stdout would corrupt a
	// pipeline that continues past clipd.
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
	if string(clip.Data()) != input {
		t.Errorf("clipboard = %q, want %q", clip.Data(), input)
	}
}

func TestEndToEndCopyVerbose(t *testing.T) {
	t.Parallel()

	addr, _, fingerprint := startDaemon(t)
	host, port := splitHostPort(t, addr)

	cfg := config.Default()
	cfg.ServerAddress = host
	cfg.Port = port
	cfg.Token = testToken
	cfg.ServerFingerprint = fingerprint
	path := writeConfig(t, cfg)

	got := exec(t, "hello\n", true, "-config", path, "-v")
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("verbose output went to stdout: %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "copied 6 bytes") {
		t.Errorf("stderr = %q, want a byte count", got.stderr)
	}
}

// TestEndToEndCopyFile covers `clipd <file>`, where the argument is not a
// known subcommand.
func TestEndToEndCopyFile(t *testing.T) {
	t.Parallel()

	addr, clip, fingerprint := startDaemon(t)
	host, port := splitHostPort(t, addr)

	cfg := config.Default()
	cfg.ServerAddress = host
	cfg.Port = port
	cfg.Token = testToken
	cfg.ServerFingerprint = fingerprint
	path := writeConfig(t, cfg)

	content := "services:\n  jellyfin:\n    image: jellyfin\n"
	file := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got := exec(t, "", false, "-config", path, file)
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	if string(clip.Data()) != content {
		t.Errorf("clipboard = %q, want %q", clip.Data(), content)
	}
}

func TestEndToEndExitCodes(t *testing.T) {
	t.Parallel()

	addr, _, fingerprint := startDaemon(t)
	host, port := splitHostPort(t, addr)

	base := func() config.Config {
		cfg := config.Default()
		cfg.ServerAddress = host
		cfg.Port = port
		cfg.Token = testToken
		cfg.ServerFingerprint = fingerprint
		cfg.TimeoutMS = 2000
		return cfg
	}

	tests := []struct {
		name   string
		cfg    config.Config
		args   []string
		stdin  string
		want   int
		detail string
	}{
		{
			name:  "bad token",
			cfg:   func() config.Config { c := base(); c.Token = "definitely-the-wrong-token"; return c }(),
			stdin: "data",
			want:  exitAuth,
		},
		{
			name:  "unreachable server",
			cfg:   func() config.Config { c := base(); c.Port = 1; return c }(),
			stdin: "data",
			want:  exitFailure,
		},
		{
			name:  "payload over the local limit",
			cfg:   func() config.Config { c := base(); c.MaxPayloadBytes = 4; return c }(),
			stdin: "far too much data",
			want:  exitTooLarge,
		},
		{
			name:  "no server configured",
			cfg:   func() config.Config { c := config.Default(); c.Token = testToken; return c }(),
			stdin: "data",
			want:  exitConfig,
		},
		{
			name:  "no token configured",
			cfg:   func() config.Config { c := base(); c.Token = ""; return c }(),
			stdin: "data",
			want:  exitConfig,
		},
		{
			// The security-relevant code: a pinned fingerprint that does not
			// match the daemon's key must exit 5, distinguishable from a
			// daemon that is merely down.
			name: "wrong fingerprint",
			cfg: func() config.Config {
				c := base()
				c.ServerFingerprint = "sha256:" + strings.Repeat("cd", 32)
				return c
			}(),
			stdin: "data",
			want:  exitTLS,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.cfg)
			args := append([]string{"-config", path}, tc.args...)
			got := exec(t, tc.stdin, true, args...)

			if got.code != tc.want {
				t.Errorf("exit code = %d, want %d (stderr: %s)", got.code, tc.want, got.stderr)
			}
			// Errors belong on stderr so a pipeline consuming stdout is
			// unaffected by a failure.
			if got.stderr == "" {
				t.Error("no error message on stderr")
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty on failure", got.stdout)
			}
		})
	}
}

// TestSubcommandsWinOverFilenames pins down the one genuine ambiguity in the
// CLI: `clipd serve` is a command, `clipd serve.txt` is a file.
func TestSubcommandsWinOverFilenames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"copy", "serve", "setup", "configure", "status", "install", "uninstall", "version", "help",
	} {
		if !isCommand(name) {
			t.Errorf("%q is not dispatched as a command", name)
		}
	}

	// Anything else is a filename, including near-misses on a command name.
	// A user with a file called "serve" reaches it as `clipd copy serve`.
	for _, name := range []string{
		"serve.txt", "./serve", "notes.md", "compose.yaml", "-", "Serve", "copyright",
	} {
		if isCommand(name) {
			t.Errorf("%q was mistaken for a command", name)
		}
	}

	// An unknown word is treated as a filename, and the error says so.
	got := exec(t, "", false, "-config", writeConfig(t, config.Default()), "notacommand")
	if got.code == exitOK {
		t.Fatal("copying a nonexistent file succeeded")
	}
	if !strings.Contains(got.stderr, "no such file") {
		t.Errorf("stderr = %q, want a file-not-found error", got.stderr)
	}
	if !strings.Contains(got.stderr, "clipd help") {
		t.Errorf("stderr = %q, want a pointer to the command list", got.stderr)
	}
}

func TestNoArgsWithoutPipePrintsUsage(t *testing.T) {
	t.Parallel()

	got := exec(t, "", false)
	if got.code != exitUsage {
		t.Errorf("exit code = %d, want %d", got.code, exitUsage)
	}
	if !strings.Contains(got.stderr, "Usage:") {
		t.Errorf("stderr = %q, want usage text", got.stderr)
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()

	t.Run("general", func(t *testing.T) {
		t.Parallel()
		got := exec(t, "", false, "help")
		if got.code != exitOK {
			t.Errorf("exit code = %d", got.code)
		}
		for _, want := range []string{"serve", "configure", "status", "install"} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("help output does not mention %q", want)
			}
		}
	})

	t.Run("per command", func(t *testing.T) {
		t.Parallel()
		got := exec(t, "", false, "help", "copy")
		if got.code != exitOK {
			t.Errorf("exit code = %d", got.code)
		}
		if !strings.Contains(got.stdout, "clipd copy") {
			t.Errorf("stdout = %q", got.stdout)
		}
	})

	t.Run("unknown topic", func(t *testing.T) {
		t.Parallel()
		got := exec(t, "", false, "help", "nonsense")
		if got.code != exitUsage {
			t.Errorf("exit code = %d, want %d", got.code, exitUsage)
		}
	})

	t.Run("-h flag", func(t *testing.T) {
		t.Parallel()
		got := exec(t, "", false, "-h")
		if got.code != exitOK {
			t.Errorf("exit code = %d, want 0", got.code)
		}
		if !strings.Contains(got.stdout, "Usage:") {
			t.Errorf("stdout = %q, want usage text", got.stdout)
		}
		// Exactly once: the flag package's automatic usage print is
		// suppressed, so help must not also appear on stderr.
		if got.stderr != "" {
			t.Errorf("stderr = %q, want empty (usage printed twice?)", got.stderr)
		}
	})

	t.Run("subcommand -h goes to stdout", func(t *testing.T) {
		t.Parallel()
		got := exec(t, "", false, "copy", "-h")
		if got.code != exitOK {
			t.Errorf("exit code = %d, want 0", got.code)
		}
		if !strings.Contains(got.stdout, "clipd copy") {
			t.Errorf("stdout = %q, want the copy usage", got.stdout)
		}
		if got.stderr != "" {
			t.Errorf("stderr = %q, want empty", got.stderr)
		}
	})

	t.Run("unknown flag reports the error once", func(t *testing.T) {
		t.Parallel()
		got := exec(t, "", false, "-nonsense")
		if got.code != exitUsage {
			t.Errorf("exit code = %d, want %d", got.code, exitUsage)
		}
		if !strings.Contains(got.stderr, "-nonsense") {
			t.Errorf("stderr = %q, want it to name the flag", got.stderr)
		}
		if n := strings.Count(got.stderr, "Usage:"); n != 1 {
			t.Errorf("usage printed %d times on stderr, want once", n)
		}
	})
}

func TestVersion(t *testing.T) {
	t.Parallel()

	got := exec(t, "", false, "version")
	if got.code != exitOK {
		t.Fatalf("exit code = %d", got.code)
	}
	for _, want := range []string{"clipd", "commit", "built", "go", "platform"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("version output does not mention %q:\n%s", want, got.stdout)
		}
	}
}

// TestGlobalFlagsWorkOnBothSides checks that the global flags are accepted
// before and after the subcommand.
func TestGlobalFlagsWorkOnBothSides(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, config.Default())

	before := exec(t, "", false, "-config", path, "status")
	after := exec(t, "", false, "status", "-config", path)

	if before.code != exitOK || after.code != exitOK {
		t.Fatalf("exit codes = %d and %d", before.code, after.code)
	}
	if !strings.Contains(before.stdout, path) || !strings.Contains(after.stdout, path) {
		t.Error("the -config flag was not honoured in both positions")
	}
}

func TestStatusRedactsTheToken(t *testing.T) {
	t.Parallel()

	// A closed loopback port: the reachability probe fails immediately
	// instead of putting a stray SYN on the user's LAN.
	cfg := config.Default()
	cfg.ServerAddress = "127.0.0.1"
	cfg.Port = 1
	cfg.Token = testToken
	cfg.TimeoutMS = 500
	path := writeConfig(t, cfg)

	got := exec(t, "", false, "-config", path, "status")
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	if strings.Contains(got.stdout, testToken) {
		t.Errorf("status printed the token in full:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "127.0.0.1:1") {
		t.Errorf("status did not report the server address:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "reachable") {
		t.Errorf("status did not report reachability:\n%s", got.stdout)
	}
}

// TestStatusReportsAReachableDaemon covers the happy path of the probe.
func TestStatusReportsAReachableDaemon(t *testing.T) {
	t.Parallel()

	addr, _, fingerprint := startDaemon(t)
	host, port := splitHostPort(t, addr)

	cfg := config.Default()
	cfg.ServerAddress = host
	cfg.Port = port
	cfg.Token = testToken
	cfg.ServerFingerprint = fingerprint
	path := writeConfig(t, cfg)

	got := exec(t, "", false, "-config", path, "status")
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "reachable    yes") {
		t.Errorf("status did not report the running daemon as reachable:\n%s", got.stdout)
	}
}

func TestSetupGeneratesAToken(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")

	got := exec(t, "", false, "-config", path, "setup")
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load the config setup wrote: %v", err)
	}
	if len(cfg.Token) < 32 {
		t.Errorf("token is %d characters, want a full-entropy value", len(cfg.Token))
	}
	// Setup is the pairing step, so this is the one command that prints the
	// token: the user has to be able to copy it to the client.
	if !strings.Contains(got.stdout, cfg.Token) {
		t.Error("setup did not display the token")
	}
	if cfg.BindAddress == "127.0.0.1" {
		t.Error("setup chose a loopback bind address, which no remote client could reach")
	}

	// Running it again must not silently invalidate the client's token.
	second := exec(t, "", false, "-config", path, "setup")
	if second.code != exitOK {
		t.Fatalf("second setup exit code = %d", second.code)
	}
	after, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if after.Token != cfg.Token {
		t.Error("a second setup replaced the token")
	}

	rotated := exec(t, "", false, "-config", path, "setup", "-rotate")
	if rotated.code != exitOK {
		t.Fatalf("setup -rotate exit code = %d", rotated.code)
	}
	final, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if final.Token == cfg.Token {
		t.Error("-rotate did not replace the token")
	}
}

func TestConfigureWithFlags(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")

	fingerprint := "sha256:" + strings.Repeat("ab", 32)
	got := exec(t, "", false, "-config", path, "configure",
		"-server", "192.168.1.50", "-port", "9000", "-token", testToken,
		"-fingerprint", fingerprint)
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	if strings.Contains(got.stdout, testToken) {
		t.Error("configure echoed the token in full")
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.ServerAddress != "192.168.1.50" || cfg.Port != 9000 || cfg.Token != testToken {
		t.Errorf("stored config = %+v", cfg)
	}

	// The file holds the token in plaintext and must not be readable by
	// anyone else. Windows has no Unix permission bits, so the check only
	// means something elsewhere.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat config: %v", err)
		}
		if perm := info.Mode().Perm(); perm != config.FilePerm {
			t.Errorf("config mode = %04o, want %04o", perm, config.FilePerm)
		}
	}
}

func TestConfigureTokenFromStdin(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")

	got := exec(t, testToken+"\n", true, "-config", path, "configure",
		"-server", "mac.local", "-token", "-",
		"-fingerprint", "sha256:"+strings.Repeat("ab", 32))
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Token != testToken {
		t.Errorf("token = %q, want the value from stdin", cfg.Token)
	}
}

// TestConfigureRefusesToPromptWhenPiped keeps a scripted misuse from hanging
// forever on a prompt nobody will answer.
func TestConfigureRefusesToPromptWhenPiped(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	got := exec(t, "", true, "-config", path, "configure")
	if got.code != exitUsage {
		t.Errorf("exit code = %d, want %d", got.code, exitUsage)
	}
}

func TestConfigureRejectsAWeakToken(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	got := exec(t, "", false, "-config", path, "configure",
		"-server", "mac.local", "-token", "short",
		"-fingerprint", "sha256:"+strings.Repeat("ab", 32))
	if got.code != exitConfig {
		t.Errorf("exit code = %d, want %d", got.code, exitConfig)
	}
	if config.Exists(path) {
		t.Error("a rejected configuration was still written to disk")
	}
}

func TestConfigureInteractive(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	// Answers to: address, port (blank keeps the default), token, fingerprint.
	answers := "192.168.1.50\n\n" + testToken + "\nsha256:" + strings.Repeat("ab", 32) + "\n"

	got := exec(t, answers, false, "-config", path, "configure")
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.ServerAddress != "192.168.1.50" {
		t.Errorf("server = %q", cfg.ServerAddress)
	}
	if cfg.Port != config.DefaultPort {
		t.Errorf("port = %d, want the default %d kept by the blank answer", cfg.Port, config.DefaultPort)
	}
	if cfg.Token != testToken {
		t.Errorf("token = %q", cfg.Token)
	}
}

// TestConfigureDoesNotPersistEnvOverrides pins the split between the two
// override channels: CLIPD_* variables exist for one-off runs and must never
// be baked into the file by a configure or setup that never mentioned them.
func TestConfigureDoesNotPersistEnvOverrides(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	envVars := map[string]string{
		"CLIPD_SERVER": "transient-host.local",
		"CLIPD_PORT":   "9999",
	}
	getenv := func(k string) string { return envVars[k] }

	got := execEnv(t, "", false, getenv, "-config", path, "configure",
		"-server", "real-mac.local", "-token", testToken,
		"-fingerprint", "sha256:"+strings.Repeat("ab", 32))
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.ServerAddress != "real-mac.local" {
		t.Errorf("server = %q, want the flag value, not the CLIPD_SERVER override", cfg.ServerAddress)
	}
	if cfg.Port != config.DefaultPort {
		t.Errorf("port = %d, want the default %d, not the CLIPD_PORT override", cfg.Port, config.DefaultPort)
	}
}

// TestSetupGeneratesATokenDespiteEnvToken covers the sharper end of the same
// bug: with CLIPD_TOKEN exported, setup must still generate a real secret
// rather than persisting the transient environment value as the daemon's.
func TestSetupGeneratesATokenDespiteEnvToken(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	getenv := func(k string) string {
		if k == "CLIPD_TOKEN" {
			return "transient-env-token-value-here"
		}
		return ""
	}

	got := execEnv(t, "", false, getenv, "-config", path, "setup")
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Token == "transient-env-token-value-here" {
		t.Error("setup persisted the CLIPD_TOKEN override instead of generating a token")
	}
	if len(cfg.Token) < 32 {
		t.Errorf("token is %d characters, want a full-entropy generated value", len(cfg.Token))
	}
}

// TestSetupReportsIgnoredDaemonOverrides covers the reporting half of the same
// split. The note is the only thing between a declined override and a silent
// change in behaviour, so it has to name every variable it declines —
// including the daemon-only ones, which are the ones setup exists to write.
func TestSetupReportsIgnoredDaemonOverrides(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	envVars := map[string]string{
		config.EnvBind:      "127.0.0.1",
		config.EnvMaxConn:   "99",
		config.EnvMaxMemory: "1GB",
	}
	getenv := func(k string) string { return envVars[k] }

	got := execEnv(t, "", false, getenv, "-config", path, "setup")
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	for name := range envVars {
		if !strings.Contains(got.stderr, name) {
			t.Errorf("the note does not name the ignored %s:\n%s", name, got.stderr)
		}
	}
	// Naming the command matters most for install, which has no flag for any
	// of these: "use flags instead" would leave that user nowhere to go.
	if !strings.Contains(got.stderr, "clipd setup") {
		t.Errorf("the note does not say which command stores these:\n%s", got.stderr)
	}
}

func TestBadTimeoutFlagIsRejected(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, config.Default())
	got := exec(t, "data", true, "-config", path, "-timeout", "30", "copy")
	// A malformed flag value is a usage error like any other bad flag;
	// exit 4 is reserved for problems with the config file or environment.
	if got.code != exitUsage {
		t.Errorf("exit code = %d, want %d", got.code, exitUsage)
	}
	if !strings.Contains(got.stderr, "timeout") {
		t.Errorf("stderr = %q, want it to mention the timeout", got.stderr)
	}
}

func TestCopyRejectsMultipleFiles(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, config.Default())
	got := exec(t, "", false, "-config", path, "copy", "a.txt", "b.txt")
	if got.code != exitUsage {
		t.Errorf("exit code = %d, want %d", got.code, exitUsage)
	}
}

func TestCopyWithoutInputOrPipe(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.ServerAddress = "mac.local"
	cfg.Token = testToken
	cfg.ServerFingerprint = "sha256:" + strings.Repeat("ab", 32)
	path := writeConfig(t, cfg)

	got := exec(t, "", false, "-config", path, "copy")
	if got.code == exitOK {
		t.Fatal("copy succeeded with no input")
	}
	if !strings.Contains(got.stderr, "no input") {
		t.Errorf("stderr = %q, want an explanation", got.stderr)
	}
}

// TestConfigIsValidJSON guards the file the user may hand-edit.
func TestConfigIsValidJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if got := exec(t, "", false, "-config", path, "setup"); got.code != exitOK {
		t.Fatalf("setup exit code = %d", got.code)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	for _, key := range []string{"server_address", "port", "bind_address", "token", "max_payload_bytes", "timeout_ms"} {
		if _, ok := generic[key]; !ok {
			t.Errorf("config is missing the %q key", key)
		}
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port from %q: %v", addr, err)
	}
	return host, port
}

// TestSubcommandHelpGoesToStdout checks that -h is treated as a request rather
// than a mistake. Help on stderr cannot be piped into a pager without
// redirection, which is the whole reason the convention exists.
func TestSubcommandHelpGoesToStdout(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"copy", "configure", "serve", "setup", "status", "install"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := exec(t, "", false, name, "-h")
			if got.code != exitOK {
				t.Errorf("exit code = %d, want %d", got.code, exitOK)
			}
			if !strings.Contains(got.stdout, "Options:") {
				t.Errorf("help did not reach stdout:\nstdout: %q\nstderr: %q", got.stdout, got.stderr)
			}
			if got.stderr != "" {
				t.Errorf("help wrote to stderr as well:\n%s", got.stderr)
			}
		})
	}
}

// TestUsageErrorGoesToStderr is the other half: an unrecognised flag is a
// mistake, so its usage text must not contaminate the command's output.
func TestUsageErrorGoesToStderr(t *testing.T) {
	t.Parallel()

	got := exec(t, "", false, "copy", "-nonsense")
	if got.code != exitUsage {
		t.Errorf("exit code = %d, want %d", got.code, exitUsage)
	}
	if got.stdout != "" {
		t.Errorf("a usage error wrote to stdout:\n%s", got.stdout)
	}
	if !strings.Contains(got.stderr, "Options:") {
		t.Errorf("usage text did not reach stderr:\n%s", got.stderr)
	}
}

func TestPermissionWarning(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if w := permissionWarning(path, "the token"); w != "" {
		t.Errorf("0600 file warned: %s", w)
	}

	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod %04o: %v", mode, err)
		}
		w := permissionWarning(path, "the daemon's private key")
		if w == "" {
			t.Errorf("mode %04o did not warn", mode)
			continue
		}
		if !strings.Contains(w, "the daemon's private key") {
			t.Errorf("mode %04o warned without naming the secret: %s", mode, w)
		}
	}

	// A file that is not there is not a problem: a client machine has no
	// keypair, and status must not invent a warning for it.
	if w := permissionWarning(filepath.Join(dir, "absent"), "the token"); w != "" {
		t.Errorf("missing file warned: %s", w)
	}
}

// TestSetupDoesNotPutTheTokenOnASuggestedCommandLine guards the pairing
// instructions: a command line is readable by other local users and is kept in
// shell history, so the token must be shown as a value to paste rather than
// baked into a command to run.
func TestSetupDoesNotPutTheTokenOnASuggestedCommandLine(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	got := exec(t, "", false, "-config", path, "setup")
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	for _, line := range strings.Split(got.stdout, "\n") {
		if !strings.Contains(line, "clipd configure") {
			continue
		}
		if strings.Contains(line, cfg.Token) {
			t.Errorf("the suggested command line carries the token:\n  %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(got.stdout, "-token -") {
		t.Error("setup did not suggest reading the token from stdin")
	}
}
