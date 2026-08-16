//go:build darwin

package launchagent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// plistPerm is the conventional mode for a LaunchAgent plist. It holds no
	// secrets — the token lives in the 0600 config file, never here.
	plistPerm fs.FileMode = 0o644

	// logDirPerm keeps the daemon's log private to its user.
	logDirPerm fs.FileMode = 0o700

	// launchctlTimeout bounds each launchctl invocation.
	launchctlTimeout = 15 * time.Second
)

// launchctlPath is the binary this package drives.
//
// A variable rather than a constant so tests can substitute a stand-in that
// records its arguments and returns a chosen exit code. That makes the
// install and uninstall paths — the code that manages a real system service —
// testable without touching the user's actual LaunchAgent.
var launchctlPath = "/bin/launchctl"

// Options controls installation.
type Options struct {
	// ExecutablePath is the clipd binary launchd will run. Empty means "the
	// binary currently running".
	ExecutablePath string

	// ConfigPath, when non-empty, is pinned into the agent's environment. A
	// LaunchAgent inherits none of the user's shell environment, so without
	// this a custom --config would be silently ignored at boot.
	ConfigPath string
}

// Result reports what Install actually did, so the CLI can show the user the
// paths involved rather than making them guess.
type Result struct {
	PlistPath      string
	ExecutablePath string
	LogPath        string
}

// State describes the installed agent.
type State struct {
	// PlistInstalled reports whether the plist file exists on disk.
	PlistInstalled bool
	PlistPath      string

	// Loaded reports whether launchd knows about the service in gui/<uid>.
	Loaded bool

	// PID is the running daemon's process ID, or 0 if it is not running.
	PID int

	// LastExitStatus is the exit status of the previous run. Non-zero after a
	// crash, which is the single most useful field when debugging "it stopped
	// working" — launchd will have restarted it, hiding the failure.
	LastExitStatus int

	LogPath string
}

// PlistPath returns ~/Library/LaunchAgents/com.clipd.agent.plist.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// LogPath returns the file launchd redirects the daemon's output to.
func LogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", "clipd", "clipd.log"), nil
}

// Install writes the plist and loads it into the user's GUI domain.
//
// It requires no elevated privileges: everything happens under the user's own
// home directory and gui/<uid> domain.
func Install(ctx context.Context, opts Options) (Result, error) {
	var res Result

	execPath, err := resolveExecutable(opts.ExecutablePath)
	if err != nil {
		return res, err
	}
	res.ExecutablePath = execPath

	plistPath, err := PlistPath()
	if err != nil {
		return res, err
	}
	res.PlistPath = plistPath

	logPath, err := LogPath()
	if err != nil {
		return res, err
	}
	res.LogPath = logPath

	if err := os.MkdirAll(filepath.Dir(logPath), logDirPerm); err != nil {
		return res, fmt.Errorf("create log directory: %w", err)
	}

	spec := Spec{
		Label:            Label,
		ProgramArguments: []string{execPath, "serve"},
		RunAtLoad:        true,
		// KeepAlive is unconditional: for a personal utility, "always be
		// running" is the behaviour the user wants, and tuning
		// SuccessfulExit conditions would only create states where the
		// clipboard silently stops working. The cost is that a daemon
		// failing at startup — a bad config, say — will be restarted in a
		// loop; launchd throttles that to once every 10 seconds, and the
		// reason lands in the log file.
		KeepAlive:       true,
		StandardOutPath: logPath,
		StandardErrPath: logPath,
	}
	if opts.ConfigPath != "" {
		spec.EnvironmentVariables = map[string]string{"CLIPD_CONFIG": opts.ConfigPath}
	}

	data, err := spec.Marshal()
	if err != nil {
		return res, err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return res, fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	if err := os.WriteFile(plistPath, data, plistPerm); err != nil {
		return res, fmt.Errorf("write %s: %w", plistPath, err)
	}
	if err := os.Chmod(plistPath, plistPerm); err != nil {
		return res, fmt.Errorf("set permissions on %s: %w", plistPath, err)
	}

	domain, err := guiDomain()
	if err != nil {
		return res, err
	}

	// Unload any previous incarnation first: bootstrap fails outright if the
	// label is already loaded, which would make `clipd install` a one-shot
	// command that breaks on upgrade.
	_, _ = runLaunchctl(ctx, "bootout", domain+"/"+Label)

	// A service the user once ran `launchctl disable` on stays disabled
	// through bootstrap, and the failure mode — loads fine, never starts — is
	// invisible. Enabling is idempotent.
	_, _ = runLaunchctl(ctx, "enable", domain+"/"+Label)

	// bootout is asynchronous: the domain may still be tearing down the old
	// job when bootstrap arrives, which surfaces as EBUSY.
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := runLaunchctl(ctx, "bootstrap", domain, plistPath); err == nil {
			return res, nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return res, fmt.Errorf("load LaunchAgent: %w", lastErr)
}

// Uninstall unloads the agent and removes its plist.
//
// A missing plist or an already-unloaded service is not an error: uninstall
// is expected to be safe to run twice.
func Uninstall(ctx context.Context) (string, error) {
	plistPath, err := PlistPath()
	if err != nil {
		return "", err
	}
	domain, err := guiDomain()
	if err != nil {
		return plistPath, err
	}

	if out, err := runLaunchctl(ctx, "bootout", domain+"/"+Label); err != nil && !isNotLoaded(out) {
		return plistPath, fmt.Errorf("unload LaunchAgent: %w", err)
	}
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return plistPath, fmt.Errorf("remove %s: %w", plistPath, err)
	}
	return plistPath, nil
}

// Status reports what launchd currently knows about the agent.
func Status(ctx context.Context) (State, error) {
	var st State

	plistPath, err := PlistPath()
	if err != nil {
		return st, err
	}
	st.PlistPath = plistPath
	if info, err := os.Stat(plistPath); err == nil && !info.IsDir() {
		st.PlistInstalled = true
	}

	if logPath, err := LogPath(); err == nil {
		st.LogPath = logPath
	}

	// `launchctl list <label>` rather than `launchctl print`: print's output
	// is verbose and has been reshaped across macOS releases, while list has
	// emitted the same old-style dict for a decade. Load state and PID are
	// all that is needed here, and list reports both. It operates on the
	// caller's own domain, which for a user shell is the gui/<uid> domain the
	// agent is bootstrapped into.
	out, err := runLaunchctl(ctx, "list", Label)
	if err != nil {
		// A non-zero exit here means "no such service", not a broken system.
		return st, nil
	}
	st.Loaded = true
	st.PID = parseLaunchctlInt(out, "PID")
	st.LastExitStatus = parseLaunchctlInt(out, "LastExitStatus")
	return st, nil
}

// resolveExecutable determines the absolute path to embed in the plist.
// launchd re-executes this path at every login, so a relative path would
// produce an agent that silently stops working.
//
// An explicit override is embedded as given (absolutized only): a
// package-manager path like /opt/homebrew/bin/clipd is deliberately a
// symlink, and retargeting it is how upgrades work — resolving it would bake
// in a versioned path that the next upgrade deletes. Only the self-detected
// path is resolved through symlinks, since os.Executable may report a
// transient link rather than the binary itself.
func resolveExecutable(override string) (string, error) {
	path := override
	if path == "" {
		self, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate clipd executable: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		path = self
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	// Stat follows symlinks, so an override that is a link is still verified
	// to point at something executable.
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("clipd executable %s: %w", abs, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable file", abs)
	}
	return abs, nil
}

// guiDomain returns the gui/<uid> service domain for the current user.
func guiDomain() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("determine current user: %w", err)
	}
	if _, err := strconv.Atoi(u.Uid); err != nil {
		return "", fmt.Errorf("unexpected uid %q", u.Uid)
	}
	return "gui/" + u.Uid, nil
}

func runLaunchctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, launchctlTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, launchctlPath, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("launchctl %s: %w", strings.Join(args, " "), err)
	}
	return text, nil
}

// isNotLoaded recognises bootout's complaint about a service that was never
// there, which is a success for uninstall purposes.
func isNotLoaded(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no such process") ||
		strings.Contains(lower, "could not find specified service")
}

// parseLaunchctlInt pulls an integer out of launchctl list's dict output,
// which looks like: "PID" = 1234;
func parseLaunchctlInt(output, key string) int {
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*=\s*(-?\d+)`)
	m := re.FindStringSubmatch(output)
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}
