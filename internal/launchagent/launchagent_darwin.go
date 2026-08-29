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
	// secrets: clipd has none to hold, and the plist only names a binary and
	// a config path.
	plistPerm fs.FileMode = 0o644

	// logDirPerm keeps the daemon's log private to its user.
	logDirPerm fs.FileMode = 0o700

	// launchctlTimeout bounds each launchctl invocation.
	launchctlTimeout = 15 * time.Second

	// rollbackTimeout covers one bootout, one bootstrap, and the short retry
	// delays between bootstrap attempts. It is deliberately independent of the
	// caller's context: cancellation must not strand an upgrade halfway through.
	rollbackTimeout = 2*launchctlTimeout + time.Second
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

	// Resolve the launchd domain before touching the installed plist. If the
	// current account cannot be identified, the existing agent must remain
	// exactly as it was.
	domain, err := guiDomain()
	if err != nil {
		return res, err
	}

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

	// Keep the exact previous contents and mode so every failure after bootout
	// can restore a working installation. Errors other than a missing file are
	// real preflight failures; treating them as "not installed" could destroy a
	// plist that merely could not be read.
	previous, readErr := os.ReadFile(plistPath)
	hadPrevious := readErr == nil
	previousMode := plistPerm
	if hadPrevious {
		info, err := os.Stat(plistPath)
		if err != nil {
			return res, fmt.Errorf("inspect existing %s: %w", plistPath, err)
		}
		previousMode = info.Mode().Perm()
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return res, fmt.Errorf("read existing %s: %w", plistPath, readErr)
	}

	// A service the user once ran `launchctl disable` on stays disabled
	// through bootstrap, and the failure mode — loads fine, never starts — is
	// invisible. Do this before bootout so an enable failure cannot stop the
	// currently working agent. Enabling is idempotent.
	if _, err := runLaunchctl(ctx, "enable", domain+"/"+Label); err != nil {
		return res, fmt.Errorf("enable LaunchAgent: %w", err)
	}

	// Unload any previous incarnation next: bootstrap fails outright if the
	// label is already loaded. A missing service is the normal first-install
	// case; every other bootout error must stop before the plist is replaced.
	if out, err := runLaunchctl(ctx, "bootout", domain+"/"+Label); err != nil && !isNotLoaded(out) {
		return res, fmt.Errorf("unload existing LaunchAgent: %w", err)
	}

	// From this point onward the previous job has been stopped. Any failure
	// must run rollback even when ctx was canceled; otherwise an interrupted
	// install could leave neither the old nor the new agent running.
	rollback := func(cause error) error {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()

		rollbackErr := rollbackInstall(rollbackCtx, domain, plistPath, previous, previousMode, hadPrevious)
		if rollbackErr != nil {
			return fmt.Errorf("%w; rollback failed: %v", cause, rollbackErr)
		}
		if hadPrevious {
			return fmt.Errorf("%w (the previous LaunchAgent was restored)", cause)
		}
		return cause
	}

	if err := writeFileAtomic(plistPath, data, plistPerm); err != nil {
		cause := fmt.Errorf("write %s: %w", plistPath, err)
		return res, rollback(cause)
	}

	// bootout is asynchronous: the domain may still be tearing down the old
	// job when bootstrap arrives, which surfaces as EBUSY.
	if err := bootstrapWithRetry(ctx, domain, plistPath); err != nil {
		return res, rollback(fmt.Errorf("load LaunchAgent: %w", err))
	}
	return res, nil
}

// bootstrapWithRetry tolerates launchd's short asynchronous bootout window.
// It returns cancellation directly so callers can distinguish an interrupted
// install while still using a separate context for rollback.
func bootstrapWithRetry(ctx context.Context, domain, plistPath string) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := runLaunchctl(ctx, "bootstrap", domain, plistPath); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt == 4 {
			break
		}

		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

// rollbackInstall removes any replacement that launchd may have partially
// loaded, then restores the exact prior on-disk state. A previous plist is
// loaded again; a first install is returned to having no plist at all.
func rollbackInstall(ctx context.Context, domain, plistPath string, previous []byte, previousMode fs.FileMode, hadPrevious bool) error {
	var rollbackErrs []error

	if out, err := runLaunchctl(ctx, "bootout", domain+"/"+Label); err != nil && !isNotLoaded(out) {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("unload replacement: %w", err))
	}

	if !hadPrevious {
		if err := os.Remove(plistPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("remove replacement plist: %w", err))
		}
		return errors.Join(rollbackErrs...)
	}

	if err := writeFileAtomic(plistPath, previous, previousMode); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("restore previous plist: %w", err))
		return errors.Join(rollbackErrs...)
	}
	if err := bootstrapWithRetry(ctx, domain, plistPath); err != nil {
		rollbackErrs = append(rollbackErrs, fmt.Errorf("reload previous LaunchAgent: %w", err))
	}
	return errors.Join(rollbackErrs...)
}

// writeFileAtomic writes a complete plist to a temporary file in the same
// directory, then renames it into place. launchd can therefore see either the
// old complete plist or the new complete plist, never a truncated file.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Restart asks launchd to stop and start the agent.
//
// kickstart -k rather than bootout followed by bootstrap: it is one call, it
// leaves the agent enabled, and it works whether or not the daemon is currently
// running. Replacing the binary or editing the config does nothing on its own —
// launchd keeps executing what it already started — so this is the other half
// of every change to either.
func Restart(ctx context.Context) error {
	domain, err := guiDomain()
	if err != nil {
		return err
	}
	if out, err := runLaunchctl(ctx, "kickstart", "-k", domain+"/"+Label); err != nil {
		return fmt.Errorf("restart %s: %w: %s", Label, err, strings.TrimSpace(out))
	}
	return nil
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
