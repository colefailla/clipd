//go:build darwin

package launchagent

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests drive the real Install/Uninstall/Status code paths against a
// stand-in launchctl, so the logic that manages a system service is verified
// without ever touching the developer's actual LaunchAgent. HOME is
// redirected to a temp directory, so the plist and log directory are created
// somewhere disposable.
//
// None of these can run in parallel: they set a package variable and the
// environment, both of which are process-wide.

// fakeLaunchctl installs a stand-in that appends its arguments to calls.log
// and behaves according to the supplied shell body. It returns the directory
// holding the recording.
func fakeLaunchctl(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, "launchctl")
	content := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\n%s\n", filepath.Join(dir, "calls.log"), body)
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatalf("write stand-in: %v", err)
	}

	previous := launchctlPath
	launchctlPath = script
	t.Cleanup(func() { launchctlPath = previous })

	return dir
}

// calls returns the recorded launchctl invocations, in order.
func calls(t *testing.T, dir string) []string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read calls: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// fakeHome redirects HOME so plist and log paths land somewhere disposable.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// fakeExecutable creates a file that passes resolveExecutable's checks.
func fakeExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clipd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

func TestInstallWritesAndLoads(t *testing.T) {
	home := fakeHome(t)
	fakeLaunchctl(t, "exit 0")
	execPath := fakeExecutable(t)

	res, err := Install(context.Background(), Options{ExecutablePath: execPath})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	wantPlist := filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
	if res.PlistPath != wantPlist {
		t.Errorf("PlistPath = %q, want %q", res.PlistPath, wantPlist)
	}
	// An explicit -exec path is recorded as given: a package-manager path is
	// deliberately a symlink whose retargeting is the upgrade mechanism, so
	// resolving it would bake in a versioned target the next upgrade deletes.
	if res.ExecutablePath != execPath {
		t.Errorf("ExecutablePath = %q, want %q", res.ExecutablePath, execPath)
	}

	info, err := os.Stat(wantPlist)
	if err != nil {
		t.Fatalf("plist was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != plistPerm {
		t.Errorf("plist mode = %04o, want %04o", perm, plistPerm)
	}

	data, err := os.ReadFile(wantPlist)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	// launchd silently refuses to load a plist it cannot parse, so a
	// malformed one produces a daemon that simply never starts.
	if err := xml.Unmarshal(data, new(struct{})); err != nil {
		t.Errorf("plist is not well-formed XML: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "<string>"+execPath+"</string>") {
		t.Errorf("plist does not point at the binary:\n%s", body)
	}
	if !strings.Contains(body, "<string>serve</string>") {
		t.Error("plist does not pass the serve subcommand")
	}

	// The log directory has to exist before launchd tries to redirect into it.
	if _, err := os.Stat(filepath.Dir(res.LogPath)); err != nil {
		t.Errorf("log directory was not created: %v", err)
	}
}

func TestInstallCallsLaunchctlInOrder(t *testing.T) {
	fakeHome(t)
	dir := fakeLaunchctl(t, "exit 0")

	if _, err := Install(context.Background(), Options{ExecutablePath: fakeExecutable(t)}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := calls(t, dir)
	if len(got) != 3 {
		t.Fatalf("got %d launchctl calls, want 3: %v", len(got), got)
	}

	uid := currentUID(t)
	// bootout first: bootstrap fails outright on an already-loaded label, so
	// without this `clipd install` would be a one-shot command that breaks on
	// every upgrade.
	if want := "bootout gui/" + uid + "/" + Label; got[0] != want {
		t.Errorf("call 1 = %q, want %q", got[0], want)
	}
	// enable next: a service the user once disabled stays disabled through
	// bootstrap, and the failure mode is invisible.
	if want := "enable gui/" + uid + "/" + Label; got[1] != want {
		t.Errorf("call 2 = %q, want %q", got[1], want)
	}
	if !strings.HasPrefix(got[2], "bootstrap gui/"+uid+" ") {
		t.Errorf("call 3 = %q, want a bootstrap into gui/%s", got[2], uid)
	}
	// The domain must be gui/<uid>, never the system domain: that is what
	// gives the daemon access to the logged-in user's pasteboard.
	if strings.Contains(strings.Join(got, " "), "system/") {
		t.Error("a call targeted the system domain instead of gui/<uid>")
	}
}

// TestInstallRetriesBootstrap covers the real-world race: bootout is
// asynchronous, so bootstrap can arrive while the old job is still tearing
// down and fail with EBUSY.
func TestInstallRetriesBootstrap(t *testing.T) {
	fakeHome(t)
	dir := fakeLaunchctl(t, `
if [ "$1" = "bootstrap" ]; then
	n=$(cat "$(dirname "$0")/count" 2>/dev/null || echo 0)
	n=$((n + 1))
	echo $n > "$(dirname "$0")/count"
	if [ "$n" -lt 3 ]; then
		echo "Bootstrap failed: 36: Operation now in progress" >&2
		exit 36
	fi
fi
exit 0`)

	if _, err := Install(context.Background(), Options{ExecutablePath: fakeExecutable(t)}); err != nil {
		t.Fatalf("Install did not survive a transient bootstrap failure: %v", err)
	}

	var bootstraps int
	for _, call := range calls(t, dir) {
		if strings.HasPrefix(call, "bootstrap ") {
			bootstraps++
		}
	}
	if bootstraps != 3 {
		t.Errorf("bootstrap attempted %d times, want 3 (two failures then success)", bootstraps)
	}
}

func TestInstallGivesUpAndReportsWhy(t *testing.T) {
	fakeHome(t)
	dir := fakeLaunchctl(t, `
if [ "$1" = "bootstrap" ]; then
	echo "Load failed: 5: Input/output error" >&2
	exit 5
fi
exit 0`)

	_, err := Install(context.Background(), Options{ExecutablePath: fakeExecutable(t)})
	if err == nil {
		t.Fatal("Install reported success despite bootstrap always failing")
	}
	// The launchctl output is the only clue to why a load failed, so it has
	// to survive into the error.
	if !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("error = %v, want it to include launchctl's output", err)
	}

	var bootstraps int
	for _, call := range calls(t, dir) {
		if strings.HasPrefix(call, "bootstrap ") {
			bootstraps++
		}
	}
	if bootstraps != 5 {
		t.Errorf("bootstrap attempted %d times, want it to stop after 5", bootstraps)
	}
}

func TestInstallPinsTheConfigPath(t *testing.T) {
	fakeHome(t)
	fakeLaunchctl(t, "exit 0")

	const configPath = "/custom/location/clipd.json"
	res, err := Install(context.Background(), Options{
		ExecutablePath: fakeExecutable(t),
		ConfigPath:     configPath,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	data, err := os.ReadFile(res.PlistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	// A LaunchAgent inherits no shell environment, so without this the agent
	// would silently read the default config instead of the chosen one.
	if !strings.Contains(string(data), configPath) {
		t.Errorf("plist does not pin the config path:\n%s", data)
	}
	if !strings.Contains(string(data), "CLIPD_CONFIG") {
		t.Error("plist does not set CLIPD_CONFIG")
	}
}

func TestInstallRejectsABadExecutable(t *testing.T) {
	fakeHome(t)
	fakeLaunchctl(t, "exit 0")

	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "clipd")
	if err := os.WriteFile(notExecutable, []byte("plain file"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// launchd re-runs this path at every login, so a path that is not
	// runnable produces a daemon that silently never starts.
	for name, path := range map[string]string{
		"not executable": notExecutable,
		"a directory":    dir,
		"missing":        filepath.Join(dir, "absent"),
	} {
		if _, err := Install(context.Background(), Options{ExecutablePath: path}); err == nil {
			t.Errorf("Install accepted %s as the binary", name)
		}
	}
}

func TestUninstall(t *testing.T) {
	home := fakeHome(t)
	dir := fakeLaunchctl(t, "exit 0")

	if _, err := Install(context.Background(), Options{ExecutablePath: fakeExecutable(t)}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("setup: plist missing: %v", err)
	}

	got, err := Uninstall(context.Background())
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if got != plistPath {
		t.Errorf("returned path = %q, want %q", got, plistPath)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Error("the plist was not removed")
	}

	all := calls(t, dir)
	last := all[len(all)-1]
	if want := "bootout gui/" + currentUID(t) + "/" + Label; last != want {
		t.Errorf("last call = %q, want %q", last, want)
	}
}

// TestUninstallIsSafeToRepeat: removing something already gone is a success,
// not an error. Uninstall should never fail because it has nothing to do.
func TestUninstallIsSafeToRepeat(t *testing.T) {
	fakeHome(t)
	fakeLaunchctl(t, `
echo "Boot-out failed: 3: No such process" >&2
exit 3`)

	if _, err := Uninstall(context.Background()); err != nil {
		t.Errorf("Uninstall on a machine with nothing installed: %v", err)
	}
}

func TestUninstallReportsRealFailures(t *testing.T) {
	fakeHome(t)
	fakeLaunchctl(t, `
echo "Boot-out failed: 1: Operation not permitted" >&2
exit 1`)

	if _, err := Uninstall(context.Background()); err == nil {
		t.Error("Uninstall swallowed a genuine launchctl failure")
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name      string
		script    string
		install   bool
		wantLoad  bool
		wantPID   int
		wantExit  int
		wantPlist bool
	}{
		{
			name:      "not installed",
			script:    "exit 113",
			install:   false,
			wantLoad:  false,
			wantPlist: false,
		},
		{
			name: "installed and running",
			script: `
if [ "$1" = "list" ]; then
	echo '{'
	echo '	"LimitLoadToSessionType" = "Aqua";'
	echo '	"Label" = "com.clipd.agent";'
	echo '	"LastExitStatus" = 0;'
	echo '	"PID" = 4242;'
	echo '}'
fi
exit 0`,
			install:   true,
			wantLoad:  true,
			wantPID:   4242,
			wantPlist: true,
		},
		{
			name: "loaded but not running after a crash",
			script: `
if [ "$1" = "list" ]; then
	echo '{'
	echo '	"LastExitStatus" = 78;'
	echo '}'
fi
exit 0`,
			install:   true,
			wantLoad:  true,
			wantPID:   0,
			wantExit:  78,
			wantPlist: true,
		},
		{
			// launchctl exits non-zero for an unknown label. That means "not
			// loaded", not "the system is broken".
			name:      "plist present but not loaded",
			script:    "exit 113",
			install:   true,
			wantLoad:  false,
			wantPlist: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeHome(t)
			fakeLaunchctl(t, tc.script)

			if tc.install {
				// Write the plist directly: Install would run the stand-in,
				// which is scripted for `list` rather than bootstrap here.
				plistPath, err := PlistPath()
				if err != nil {
					t.Fatalf("PlistPath: %v", err)
				}
				if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(plistPath, []byte("<plist/>"), plistPerm); err != nil {
					t.Fatalf("write plist: %v", err)
				}
			}

			state, err := Status(context.Background())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if state.PlistInstalled != tc.wantPlist {
				t.Errorf("PlistInstalled = %v, want %v", state.PlistInstalled, tc.wantPlist)
			}
			if state.Loaded != tc.wantLoad {
				t.Errorf("Loaded = %v, want %v", state.Loaded, tc.wantLoad)
			}
			if state.PID != tc.wantPID {
				t.Errorf("PID = %d, want %d", state.PID, tc.wantPID)
			}
			if state.LastExitStatus != tc.wantExit {
				t.Errorf("LastExitStatus = %d, want %d", state.LastExitStatus, tc.wantExit)
			}
		})
	}
}

func TestPaths(t *testing.T) {
	home := fakeHome(t)

	plistPath, err := PlistPath()
	if err != nil {
		t.Fatalf("PlistPath: %v", err)
	}
	// launchd requires the filename to match the label.
	if base := filepath.Base(plistPath); base != Label+".plist" {
		t.Errorf("plist filename = %q, want %q", base, Label+".plist")
	}
	if want := filepath.Join(home, "Library", "LaunchAgents"); filepath.Dir(plistPath) != want {
		t.Errorf("plist directory = %q, want %q", filepath.Dir(plistPath), want)
	}

	logPath, err := LogPath()
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	if want := filepath.Join(home, "Library", "Logs", "clipd"); filepath.Dir(logPath) != want {
		t.Errorf("log directory = %q, want %q", filepath.Dir(logPath), want)
	}
}

func TestResolveExecutable(t *testing.T) {
	good := fakeExecutable(t)

	got, err := resolveExecutable(good)
	if err != nil {
		t.Fatalf("resolveExecutable: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolved to %q, want an absolute path", got)
	}

	// An explicit symlink is honoured, not resolved: for a Homebrew-style
	// layout the public path is a symlink and its target is version-specific,
	// so recording the target would break on the next upgrade. The link must
	// still point at something executable, which Stat (following links)
	// verifies.
	link := filepath.Join(t.TempDir(), "clipd-link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolved, err := resolveExecutable(link)
	if err != nil {
		t.Fatalf("resolveExecutable on a symlink: %v", err)
	}
	if resolved != link {
		t.Errorf("resolved to %q, want the symlink %q recorded as given", resolved, link)
	}

	for _, bad := range []string{
		filepath.Join(t.TempDir(), "does-not-exist"),
		t.TempDir(), // a directory
	} {
		if _, err := resolveExecutable(bad); err == nil {
			t.Errorf("resolveExecutable(%q) accepted an unusable path", bad)
		}
	}
}

func TestGUIDomain(t *testing.T) {
	domain, err := guiDomain()
	if err != nil {
		t.Fatalf("guiDomain: %v", err)
	}
	if !strings.HasPrefix(domain, "gui/") {
		t.Errorf("domain = %q, want the gui/<uid> form", domain)
	}
	if _, err := strconv.Atoi(strings.TrimPrefix(domain, "gui/")); err != nil {
		t.Errorf("domain = %q, want a numeric uid", domain)
	}
}

func TestParseLaunchctlInt(t *testing.T) {
	// The dict launchctl list emits, which this parsing depends on.
	const output = `{
	"LimitLoadToSessionType" = "Aqua";
	"Label" = "com.clipd.agent";
	"OnDemand" = false;
	"LastExitStatus" = 0;
	"PID" = 1234;
	"Program" = "/usr/local/bin/clipd";
}`

	tests := []struct {
		key  string
		want int
	}{
		{"PID", 1234},
		{"LastExitStatus", 0},
		{"NotPresent", 0},
		{"Label", 0}, // a string value is not an integer
	}
	for _, tc := range tests {
		if got := parseLaunchctlInt(output, tc.key); got != tc.want {
			t.Errorf("parseLaunchctlInt(%q) = %d, want %d", tc.key, got, tc.want)
		}
	}

	// A crashed service reports a non-zero status, which is the single most
	// useful field when a daemon "stopped working" and KeepAlive hid it.
	if got := parseLaunchctlInt(`"LastExitStatus" = 78;`, "LastExitStatus"); got != 78 {
		t.Errorf("crash status = %d, want 78", got)
	}
	if got := parseLaunchctlInt(`"LastExitStatus" = -1;`, "LastExitStatus"); got != -1 {
		t.Errorf("negative status = %d, want -1", got)
	}
	if got := parseLaunchctlInt("", "PID"); got != 0 {
		t.Errorf("empty output = %d, want 0", got)
	}
}

func TestIsNotLoaded(t *testing.T) {
	// These strings are what distinguishes "nothing to remove" from a real
	// failure. If Apple rewords them, uninstall starts failing on an
	// already-unloaded service, and this test is what catches it.
	for _, output := range []string{
		"Boot-out failed: 3: No such process",
		"Could not find specified service",
		"COULD NOT FIND SPECIFIED SERVICE",
	} {
		if !isNotLoaded(output) {
			t.Errorf("isNotLoaded(%q) = false, want true", output)
		}
	}

	for _, output := range []string{
		"Boot-out failed: 1: Operation not permitted",
		"",
		"some unrelated failure",
	} {
		if isNotLoaded(output) {
			t.Errorf("isNotLoaded(%q) = true, want false", output)
		}
	}
}

func TestRunLaunchctlSurfacesOutput(t *testing.T) {
	fakeLaunchctl(t, `
echo "something went wrong" >&2
exit 7`)

	out, err := runLaunchctl(context.Background(), "print", "gui/501/nope")
	if err == nil {
		t.Fatal("runLaunchctl reported success on a non-zero exit")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("error = %v, want it to carry the command output", err)
	}
	if !strings.Contains(out, "something went wrong") {
		t.Errorf("output = %q, want the command output returned", out)
	}
}

func currentUID(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	return u.Uid
}
