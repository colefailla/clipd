// Package config loads and saves the clipd daemon's settings.
//
// The file is small on purpose. Everything clipd used to store about identity
// — a shared token, a pinned server fingerprint, paths to a TLS keypair — is
// gone, because SSH now supplies authentication and encryption and the socket's
// filesystem permissions supply authorisation. What is left describes where to
// listen, where to put dropped files, and how large a message may be.
//
// A consequence worth stating: this file no longer holds a secret. It is still
// written 0600 inside a 0700 directory, because that costs nothing and because
// the settings describe how a daemon behaves, but losing it no longer means
// losing access to anything.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// File layout.
const (
	// AppName is the config subdirectory name under the user's config dir.
	AppName = "clipd"

	// FileName is the config file within that directory.
	FileName = "config.json"

	// DirPerm and FilePerm are applied explicitly with Chmod after creation:
	// the mode passed to MkdirAll/OpenFile is masked by umask, and while the
	// daemon sets a restrictive umask of its own, a config written by any other
	// invocation should not depend on that having happened.
	DirPerm  fs.FileMode = 0o700
	FilePerm fs.FileMode = 0o600
)

// EnvConfig selects the config file, the one environment variable clipd reads.
//
// The rest of the CLIPD_* overlay is gone. It existed to vary a client's
// server address and token per invocation; there is no client and no token
// now, and every remaining setting belongs to a daemon that launchd starts
// with no environment at all.
const EnvConfig = "CLIPD_CONFIG"

// Defaults.
const (
	// DefaultAddress is a socket in the user's home directory. Home is not
	// world-writable, which /tmp is: a socket there could be replaced by
	// another user between daemon restarts.
	DefaultAddress = "~/.clipd.sock"

	// DefaultDropDir is where `clipd drop` puts files. A dedicated directory,
	// never an existing one like ~/Downloads, so that nothing arriving over
	// the socket can be confused with something the user put there.
	DefaultDropDir = "~/Drop"

	// DefaultMaxPayloadBytes caps one clipboard message.
	DefaultMaxPayloadBytes int64 = 10 << 20

	// DefaultMaxDropBytes and DefaultMaxDropFiles cap one drop.
	DefaultMaxDropBytes int64 = 256 << 20
	DefaultMaxDropFiles       = 256

	// MaxAllowedPayloadBytes is the ceiling Validate enforces, well above any
	// plausible paste and well below anything that would trouble a Mac.
	MaxAllowedPayloadBytes int64 = 1 << 30

	// The remaining ceilings exist so that every limit the daemon derives from
	// this file stays in a range where the arithmetic is provably safe.
	//
	// The drop package multiplies MaxDropFiles to get an entry cap and adds a
	// per-entry framing budget on top of MaxDropBytes to get a wire cap. Left
	// unbounded, a large enough value in either field overflows int64 and the
	// resulting negative limit rejects every drop instead of bounding one — a
	// setting that reads like "allow more" that in fact allows nothing.
	MaxAllowedDropBytes int64 = 1 << 40
	MaxAllowedDropFiles       = 1 << 16

	// MaxAllowedConcurrent matches the daemon's own connection ceiling. Above
	// it, work slots outnumber the sockets that could occupy them.
	MaxAllowedConcurrent = 64
)

// Config is the on-disk configuration.
type Config struct {
	// Address is the daemon's UNIX socket path. It must start with /, ~ or .;
	// see IsSocketPath.
	Address string `json:"address"`

	// DropDir receives files sent with `clipd drop`.
	DropDir string `json:"drop_dir"`

	// MaxPayloadBytes caps a single clipboard message.
	MaxPayloadBytes int64 `json:"max_payload_bytes"`

	// MaxDropBytes and MaxDropFiles cap one drop, in total bytes and entries.
	MaxDropBytes int64 `json:"max_drop_bytes"`
	MaxDropFiles int   `json:"max_drop_files"`

	// MaxConcurrent bounds the messages handled at once. Zero means the
	// server's default. Together with MaxPayloadBytes it sets the daemon's
	// memory ceiling, since a clipboard payload is buffered whole.
	MaxConcurrent int `json:"max_concurrent"`

	// MaxTransferSeconds bounds one connection, including receipt and publication.
	MaxTransferSeconds int `json:"max_transfer_seconds"`
}

// Default returns the built-in configuration. Unlike the previous version
// there is nothing left to establish during setup: these values work as they
// stand, so a daemon with no config file at all is a working daemon.
func Default() Config {
	return Config{
		Address:            DefaultAddress,
		DropDir:            DefaultDropDir,
		MaxPayloadBytes:    DefaultMaxPayloadBytes,
		MaxDropBytes:       DefaultMaxDropBytes,
		MaxDropFiles:       DefaultMaxDropFiles,
		MaxConcurrent:      8,
		MaxTransferSeconds: 1800,
	}
}

// DefaultPath returns the config file path for the current user.
//
// ~/.config on every platform, rather than os.UserConfigDir's per-OS answer.
// That function returns ~/Library/Application Support on macOS, which is
// Apple's convention for application bundles; clipd is a command-line daemon,
// and the tools it sits beside — git, gh, btop, clipper — all live in
// ~/.config there. Using one path on both platforms also means one line of
// documentation instead of two, and no shell command that has to escape a
// space in the middle of a directory name.
//
// XDG_CONFIG_HOME still wins when it is set, which is the whole point of the
// variable.
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		// XDG_CONFIG_HOME is defined as an absolute path. A relative value is
		// especially unsafe here because an interactive install and launchd use
		// different working directories and would silently read different files.
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("XDG_CONFIG_HOME must be an absolute path, got %q", dir)
		}
		return filepath.Join(dir, AppName, FileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", AppName, FileName), nil
}

// LegacyPath is where releases before v3 kept the config on macOS, or "" if
// there is nothing there.
//
// Nothing reads it. Resolution does not fall back to it and startup does not
// fail over it, because a config written by v2 holds only settings v3 removed,
// and defaults are the right answer for an upgrader. It exists so status can
// mention the orphan, which is where someone would look for it.
func LegacyPath() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, "Library", "Application Support", AppName, FileName)
	if !Exists(path) {
		return ""
	}
	return path
}

// ResolvePath picks the config path from, in order: an explicit flag value,
// CLIPD_CONFIG, then the platform default.
func ResolvePath(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv(EnvConfig); env != "" {
		return env, nil
	}
	return DefaultPath()
}

// Exists reports whether a config file is present.
func Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Load reads the config file, filling unset fields from the defaults.
//
// A missing file is not an error: it yields defaults, which are a complete
// working configuration. That is what lets `clipd serve` come up on a machine
// where nothing has been configured at all.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	if err := checkV2(path, data); err != nil {
		return cfg, err
	}

	// Unknown keys are rejected rather than ignored, so a typo fails loudly
	// instead of silently leaving a default in place.
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var file Config
	if err := dec.Decode(&file); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	// A config file holds one object. Decoder.More is only meaningful while
	// traversing an array or object; at the top level it can overlook an
	// unmatched closing delimiter. A second decode must reach EOF.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return cfg, fmt.Errorf("parse %s: trailing data after the configuration object: %w", path, err)
		}
		return cfg, fmt.Errorf("parse %s: trailing data after the configuration object", path)
	}

	if file.Address != "" {
		cfg.Address = file.Address
	}
	if file.DropDir != "" {
		cfg.DropDir = file.DropDir
	}
	if file.MaxPayloadBytes != 0 {
		cfg.MaxPayloadBytes = file.MaxPayloadBytes
	}
	if file.MaxDropBytes != 0 {
		cfg.MaxDropBytes = file.MaxDropBytes
	}
	if file.MaxDropFiles != 0 {
		cfg.MaxDropFiles = file.MaxDropFiles
	}
	if file.MaxConcurrent != 0 {
		cfg.MaxConcurrent = file.MaxConcurrent
	}
	if file.MaxTransferSeconds != 0 {
		cfg.MaxTransferSeconds = file.MaxTransferSeconds
	}
	return cfg, cfg.Validate()
}

// v2Keys are settings that existed only before clipd moved to a socket. None
// of them has a v3 equivalent: the token and fingerprint authenticated a TLS
// connection that no longer exists, and the address is now one field rather
// than a host and a port.
var v2Keys = []string{
	"server_address", "server_fingerprint", "token",
	"tls_cert_path", "tls_key_path", "bind_address", "port", "timeout_ms",
}

// checkV2 turns the strict parser's complaint into an answer.
//
// Strict parsing is right — a typo should fail rather than silently leave a
// default in place — but "unknown field "server_fingerprint"" tells someone
// upgrading nothing about what happened or what to do. This is the one case
// worth naming, and it is a lookup rather than a migration: there is nothing in
// a v2 file worth carrying forward, since every v3 setting has a working
// default.
func checkV2(path string, data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// Not decodable at all; let the strict parser report why.
		return nil
	}
	var found []string
	for _, key := range v2Keys {
		if _, ok := raw[key]; ok {
			found = append(found, key)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s is a clipd v2 config (it still has %s).\n"+
			"       v3 sends over an SSH-forwarded socket, so the token, fingerprint and TLS\n"+
			"       settings no longer exist and every remaining setting has a working default.\n"+
			"       Move it aside and start fresh:\n\n"+
			"         mv %s %s.v2\n",
		path, strings.Join(found, ", "), path, path)
}

// Save atomically writes the config, creating its directory if needed.
func (c Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	created, err := ensureDir(dir)
	if err != nil {
		return err
	}
	// Saving through -config must not change an arbitrary parent directory's
	// mode merely because its basename happens to be "clipd". We own a
	// directory we just created and the resolved default application directory;
	// nothing else.
	if created || isDefaultDir(dir) {
		// The mode passed to MkdirAll is masked by umask, so the intended mode
		// is set explicitly.
		if err := os.Chmod(dir, DirPerm); err != nil {
			return fmt.Errorf("restrict %s: %w", dir, err)
		}
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	writePath, err := resolveWritePath(path)
	if err != nil {
		return err
	}
	if err := writeAtomic(writePath, data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ensureDir creates only the final directory itself after making any missing
// parents. Using Mkdir for the last component tells the caller whether this
// invocation actually created the directory, so a creation race cannot make
// Save chmod a directory owned by somebody else.
func ensureDir(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("create %s: path exists and is not a directory", dir)
		}
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("inspect %s: %w", dir, err)
	}

	parent := filepath.Dir(dir)
	if parent != dir {
		if err := os.MkdirAll(parent, DirPerm); err != nil {
			return false, fmt.Errorf("create %s: %w", parent, err)
		}
	}
	if err := os.Mkdir(dir, DirPerm); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrExist) {
		return false, fmt.Errorf("create %s: %w", dir, err)
	}

	// Another process won the race. Verify that it created a directory, but do
	// not claim ownership of it or change its mode.
	info, err = os.Stat(dir)
	if err != nil {
		return false, fmt.Errorf("inspect %s after concurrent creation: %w", dir, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("create %s: path exists and is not a directory", dir)
	}
	return false, nil
}

// isDefaultDir compares directory identity rather than spelling so an XDG
// path reached through a symlink is still recognised, while an unrelated
// custom directory named "clipd" is not.
func isDefaultDir(dir string) bool {
	defaultPath, err := DefaultPath()
	if err != nil {
		return false
	}
	got, err := os.Stat(dir)
	if err != nil {
		return false
	}
	want, err := os.Stat(filepath.Dir(defaultPath))
	return err == nil && os.SameFile(got, want)
}

// resolveWritePath preserves an existing config symlink by atomically
// replacing its target. Replacing the link itself would quietly break a
// dotfiles-managed setup. A dangling link has no safe target and is refused.
func resolveWritePath(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("write %s: path is not a regular file", path)
		}
		return path, nil
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve config symlink %s: %w", path, err)
	}
	target, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect config symlink target %s: %w", resolved, err)
	}
	if !target.Mode().IsRegular() {
		return "", fmt.Errorf("write %s: config symlink target is not a regular file", path)
	}
	return resolved, nil
}

// writeAtomic builds a complete, restricted file beside its destination and
// renames it into place. Syncing both the file and its directory means a crash
// cannot expose a truncated config or lose a successful rename on filesystems
// that honour directory fsync.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		_ = os.Remove(tmp)
	}()

	if err := f.Chmod(FilePerm); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// IsSocketPath reports whether an address names a socket path. A leading /, ~
// or . is required, so a host:port from the loopback TCP mode clipd used to
// offer is refused rather than taken as a relative file name.
func IsSocketPath(address string) bool {
	return strings.HasPrefix(address, "/") ||
		strings.HasPrefix(address, "~") ||
		strings.HasPrefix(address, ".")
}

// Validate rejects a configuration the daemon could not honour.
func (c Config) Validate() error {
	if c.MaxTransferSeconds < 1 || c.MaxTransferSeconds > 86400 {
		return errors.New("max_transfer_seconds: must be between 1 and 86400 (24 hours)")
	}
	if c.Address == "" {
		return errors.New("address: must not be empty")
	}
	if !IsSocketPath(c.Address) {
		return fmt.Errorf("address: %q is not a socket path; clipd listens only on a UNIX socket, such as %s", c.Address, DefaultAddress)
	}
	if c.MaxPayloadBytes < 1 {
		return fmt.Errorf("max_payload_bytes: %d must be positive", c.MaxPayloadBytes)
	}
	if c.MaxPayloadBytes > MaxAllowedPayloadBytes {
		return fmt.Errorf("max_payload_bytes: %d exceeds the %s ceiling",
			c.MaxPayloadBytes, FormatSize(MaxAllowedPayloadBytes))
	}
	if c.MaxDropBytes < 0 {
		return fmt.Errorf("max_drop_bytes: %d must not be negative", c.MaxDropBytes)
	}
	if c.MaxDropBytes > MaxAllowedDropBytes {
		return fmt.Errorf("max_drop_bytes: %d exceeds the %s ceiling",
			c.MaxDropBytes, FormatSize(MaxAllowedDropBytes))
	}
	if c.MaxDropFiles < 0 {
		return fmt.Errorf("max_drop_files: %d must not be negative", c.MaxDropFiles)
	}
	if c.MaxDropFiles > MaxAllowedDropFiles {
		return fmt.Errorf("max_drop_files: %d exceeds the %d ceiling",
			c.MaxDropFiles, MaxAllowedDropFiles)
	}
	if c.MaxConcurrent < 0 {
		return fmt.Errorf("max_concurrent: %d must not be negative", c.MaxConcurrent)
	}
	if c.MaxConcurrent > MaxAllowedConcurrent {
		return fmt.Errorf("max_concurrent: %d exceeds the %d ceiling",
			c.MaxConcurrent, MaxAllowedConcurrent)
	}
	return nil
}

// FormatDuration renders a time limit as people write it: "30m" or "1h"
// rather than Go's "30m0s" or "1h0m0s".
func FormatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// FormatSize renders a byte count for human consumption.
func FormatSize(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.3g TiB", float64(n)/float64(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.3g GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.3g MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.3g KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
