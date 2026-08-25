package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

type result struct {
	code   int
	stdout string
	stderr string
}

// runCLI runs the CLI with an isolated environment and captures its output.
func runCLI(t *testing.T, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	e := &env{
		stdout: &out,
		stderr: &errOut,
		// An empty environment keeps the developer's real CLIPD_CONFIG from
		// leaking into the tests.
		getenv: func(string) string { return "" },
	}
	code := run(context.Background(), args, e)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func TestBareInvocationPrintsUsage(t *testing.T) {
	t.Parallel()

	got := runCLI(t)
	if got.code != exitUsage {
		t.Errorf("exit code = %d, want %d", got.code, exitUsage)
	}
	if !strings.Contains(got.stderr, "clipd setup") {
		t.Errorf("stderr = %q, want usage text", got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
}

func TestHelpGoesToStdoutExactlyOnce(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"-h"}, {"help"}} {
		got := runCLI(t, args...)
		if got.code != exitOK {
			t.Errorf("%v: exit code = %d, want 0", args, got.code)
		}
		if !strings.Contains(got.stdout, "Commands:") {
			t.Errorf("%v: stdout = %q, want usage text", args, got.stdout)
		}
		// The flag package prints its own usage on -h; that is suppressed, so
		// help must not also appear on stderr.
		if got.stderr != "" {
			t.Errorf("%v: stderr = %q, want empty (usage printed twice?)", args, got.stderr)
		}
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	t.Parallel()

	got := runCLI(t, "teleport")
	if got.code != exitUsage {
		t.Errorf("exit code = %d, want %d", got.code, exitUsage)
	}
	if !strings.Contains(got.stderr, "teleport") {
		t.Errorf("stderr = %q, want it to name the command", got.stderr)
	}
}

func TestUnknownFlagReportsTheErrorOnce(t *testing.T) {
	t.Parallel()

	got := runCLI(t, "-nonsense")
	if got.code != exitUsage {
		t.Errorf("exit code = %d, want %d", got.code, exitUsage)
	}
	if !strings.Contains(got.stderr, "-nonsense") {
		t.Errorf("stderr = %q, want it to name the flag", got.stderr)
	}
	if n := strings.Count(got.stderr, "Commands:"); n != 1 {
		t.Errorf("usage printed %d times, want once", n)
	}
}

func TestSubcommandHelpGoesToStdout(t *testing.T) {
	t.Parallel()

	got := runCLI(t, "setup", "-h")
	if got.code != exitOK {
		t.Errorf("exit code = %d, want 0", got.code)
	}
	if !strings.Contains(got.stdout, "clipd setup") {
		t.Errorf("stdout = %q, want the setup usage", got.stdout)
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q, want empty", got.stderr)
	}
}

// TestHelpTopicsAreReachable guards against a topic being documented in the
// usage text but missing from the map, which reads as a broken promise.
func TestHelpTopicsAreReachable(t *testing.T) {
	t.Parallel()

	for _, topic := range []string{
		"serve", "setup", "drop", "config", "security",
		"install", "uninstall", "status", "version", "help",
	} {
		got := runCLI(t, "help", topic)
		if got.code != exitOK {
			t.Errorf("help %s: exit code = %d, want 0", topic, got.code)
		}
		if len(strings.TrimSpace(got.stdout)) == 0 {
			t.Errorf("help %s printed nothing", topic)
		}
	}
}

func TestUnknownHelpTopicIsAUsageError(t *testing.T) {
	t.Parallel()

	got := runCLI(t, "help", "quantum")
	if got.code != exitUsage {
		t.Errorf("exit code = %d, want %d", got.code, exitUsage)
	}
}

func TestSetupNeedsExactlyOneHost(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"setup"}, {"setup", "a", "b"}} {
		got := runCLI(t, args...)
		if got.code != exitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, got.code, exitUsage)
		}
	}
}

func TestVersionPrintsBuildInformation(t *testing.T) {
	t.Parallel()

	got := runCLI(t, "version")
	if got.code != exitOK {
		t.Errorf("exit code = %d, want 0", got.code)
	}
	for _, want := range []string{"clipd", "commit", "built", "platform"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("stdout = %q, want it to mention %q", got.stdout, want)
		}
	}
}
