package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// sshQuote quotes a path and escapes SSH percent expansion.
func sshQuote(v string) string {
	return `"` + strings.ReplaceAll(v, "%", "%%") + `"`
}

// validSSHValue rejects values that cannot be encoded by sshQuote.
func validSSHValue(what, v string) error {
	if v == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if i := strings.IndexAny(v, "\"\\\n\r"); i >= 0 {
		return fmt.Errorf("%s %q contains %q, which an ssh config cannot carry", what, v, string(v[i]))
	}
	return nil
}

// validHostPattern requires a literal destination rather than an SSH pattern.
func validHostPattern(host string) error {
	if host == "" {
		return errors.New("the ssh destination has no host")
	}
	if strings.HasPrefix(host, "-") {
		return errors.New("the ssh host cannot start with a dash")
	}
	if i := strings.IndexAny(host, "*?!,/: \t"); i >= 0 {
		return fmt.Errorf("ssh destination %q contains %q; setup writes a block for one named host, so give it one",
			host, string(host[i]))
	}
	return validSSHValue("ssh destination", host)
}

// validateDestination checks a destination before passing it to ssh.
func validateDestination(destination string) error {
	if strings.HasPrefix(destination, "-") {
		return errors.New("the ssh destination cannot start with a dash")
	}
	user, host := splitDestination(destination)
	if err := validHostPattern(host); err != nil {
		return err
	}
	if strings.Contains(destination, "@") {
		return validSSHUser(user)
	}
	return nil
}

// Match user accepts patterns, so account names must be literal.
func validSSHUser(user string) error {
	if err := validSSHValue("ssh user", user); err != nil {
		return err
	}
	if strings.ContainsAny(user, "*?!, %@\t") {
		return fmt.Errorf("ssh user %q cannot be represented as a literal account match", user)
	}
	return nil
}

// sshBlockFor matches the destination spelling and the account found by setup.
// Markers retain the original spelling so rerunning setup replaces older blocks.
func sshBlockFor(destination, account, remoteSocket, localSocket string) (string, error) {
	if err := validateDestination(destination); err != nil {
		return "", err
	}
	user, host := splitDestination(destination)
	if user != "" {
		account = user
	}
	if err := validSSHUser(account); err != nil {
		return "", err
	}
	if err := validSSHValue("remote socket path", remoteSocket); err != nil {
		return "", err
	}
	if err := validSSHValue("local socket path", localSocket); err != nil {
		return "", err
	}
	start, end := sshMarkers(destination)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", start)
	// originalhost matches the alias before HostName substitution.
	fmt.Fprintf(&b, "Match originalhost %s user %s\n", sshQuote(host), sshQuote(account))
	fmt.Fprintf(&b, "  RemoteForward %s %s\n", sshQuote(remoteSocket), sshQuote(localSocket))
	// sshd controls the remote socket's bind mask and unlink policy.
	// Reset scope: the marker alone does not end an SSH configuration section.
	fmt.Fprintf(&b, "Host *\n%s\n", end)
	return b.String(), nil
}

// installSSHConfig replaces one destination's managed block and preserves the
// first backup. Existing config symlinks are retained by updating their targets.
func installSSHConfig(ctx context.Context, destination, block string) (path string, changed bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("locate home directory: %w", err)
	}
	dir := filepath.Join(home, ".ssh")
	path = filepath.Join(dir, "config")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return path, false, fmt.Errorf("create %s: %w", dir, err)
	}
	writePath, err := resolvedWritePath(path)
	if err != nil {
		return path, false, err
	}

	existing, err := os.ReadFile(writePath)
	if err != nil && !os.IsNotExist(err) {
		return path, false, fmt.Errorf("read %s: %w", path, err)
	}

	start, end := sshMarkers(destination)
	stripped, err := stripBlock(string(existing), start, end)
	if err != nil {
		return path, false, fmt.Errorf("%s: %w", path, err)
	}
	// Also drop a block from before the markers carried a destination. There
	// can only be one, it names some host this run may not be for, and leaving
	// it would forward the same socket twice — which fails at connect time with
	// "remote port forwarding failed" and no clue as to the cause.
	stripped, err = stripBlock(stripped, blockStart, blockEnd)
	if err != nil {
		return path, false, fmt.Errorf("%s: %w", path, err)
	}

	updated := stripped
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	if updated != "" {
		updated += "\n"
	}
	updated += block

	if string(existing) == updated {
		return path, false, nil
	}
	if err := verifySSHConfig(ctx, updated, destination); err != nil {
		return path, false, err
	}
	// Retain the first pre-clipd backup across repeated setup runs.
	backup := path + ".clipd-backup"
	if len(existing) > 0 {
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(backup, existing, 0o600); err != nil {
				return path, false, fmt.Errorf("back up %s: %w", path, err)
			}
		}
	}
	if err := writeFileAtomic(writePath, []byte(updated), 0o600); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// resolvedWritePath preserves a config symlink while retaining atomic writes.
//
// Replacing ~/.ssh/config itself would replace the symlink, which is a common
// way to manage dotfiles. Resolve an existing link once and atomically replace
// its regular-file target instead. A dangling link is refused: guessing where
// to create its target could write outside the user's intended location.
func resolvedWritePath(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("refusing to replace %s: it is not a regular file", path)
		}
		return path, nil
	}

	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("inspect target of %s: %w", path, err)
	}
	if !targetInfo.Mode().IsRegular() {
		return "", fmt.Errorf("refusing to replace %s: its target %s is not a regular file", path, target)
	}
	return target, nil
}

// verifySSHConfig checks client-side parsing with ssh -G. It cannot verify
// forwarding or server policy. If validation cannot be started, setup proceeds.
func verifySSHConfig(ctx context.Context, content, destination string) error {
	f, err := os.CreateTemp("", "clipd-sshcheck-*")
	if err != nil {
		return nil
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return nil
	}
	if err := f.Close(); err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-G", "-F", f.Name(), destination)
	stderr := &capWriter{max: maxProbeOutput}
	cmd.Stderr = stderr
	cmd.Stdout = &capWriter{max: maxProbeOutput}
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("the generated SSH configuration would not parse: %s", msg)
	}
	return nil
}

// writeFileAtomic replaces a file using a temporary in the same directory.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".clipd-*")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmp := f.Name()
	// A no-op once the rename below has succeeded.
	defer os.Remove(tmp)

	if err := f.Chmod(perm); err != nil {
		f.Close()
		return fmt.Errorf("restrict %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// stripBlock removes a previously managed block, markers included.
//
// Exactly zero or one complete block is accepted. Reversed, nested, duplicate,
// or unmatched markers mean the file was edited by hand or corrupted; refusing
// it is safer than guessing which surrounding lines belong to the user.
func stripBlock(s, start, end string) (string, error) {
	var out []string
	inside := false
	seen := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == start {
			if inside || seen {
				return "", fmt.Errorf("found duplicate or nested %q; fix the clipd block by hand", start)
			}
			inside = true
			continue
		}
		if trimmed == end {
			if !inside {
				return "", fmt.Errorf("found %q before a matching %q; fix the clipd block by hand", end, start)
			}
			inside = false
			seen = true
			continue
		}
		if !inside {
			out = append(out, line)
		}
	}
	if inside {
		return "", fmt.Errorf("found %q with no matching %q; fix the clipd block by hand", start, end)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n"), nil
}
