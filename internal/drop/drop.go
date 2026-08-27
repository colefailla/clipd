// Package drop receives files sent over the clipd socket and writes them into
// a single directory.
//
// The transport is a tar stream, because tar is on every Unix machine and so
// the sending side needs nothing installed: `tar cf - notes.txt | nc ...` is
// the whole client. tar also carries the one thing a raw byte stream loses,
// which is what the file was called.
//
// Everything here exists because that stream is attacker-controlled. A tar
// archive can name a file "../../.ssh/authorized_keys", declare it a symlink
// pointing anywhere on the filesystem, claim to be a device node, or expand
// to more bytes than the disk holds. `tar -x` has to cope with all of that
// because it is a general-purpose restore tool. This is not: the entire
// requirement is "the files land in one directory under their own names", and
// narrowing the requirement is what makes it safe to implement.
package drop

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// filePerm is the mode every extracted file gets.
//
// The archive's own mode is discarded rather than honoured. A sender can set
// any bits it likes, including the executable ones, and a file that arrived
// over a socket has no business being executable by default. 0600 also keeps
// the drop directory readable only by its owner on a shared Mac.
const filePerm os.FileMode = 0o600

// dirPerm matches: the directory holds files that arrived from another
// machine, so it is nobody else's business.
const dirPerm os.FileMode = 0o700

// maxNameBytes bounds a single filename.
//
// Well below any filesystem's limit, because the point is not to discover the
// limit — it is to keep a sender from choosing how much of the path budget it
// consumes.
const maxNameBytes = 200

// maxCollisionAttempts bounds the search for an unused name when a file of
// that name is already present.
//
// Bounded rather than open-ended so that a sender cannot make the daemon
// stat its way through an unbounded sequence by repeatedly dropping the same
// name.
const maxCollisionAttempts = 100

// Options configures an extraction. Dir is required; the limits fall back to
// conservative defaults when unset.
type Options struct {
	// Dir is the directory files are written into. It is created if missing.
	Dir string

	// MaxBytes caps the total written across the whole archive, not per file.
	// A thousand small files are as capable of filling a disk as one large one.
	MaxBytes int64

	// MaxFiles caps how many entries may be written.
	MaxFiles int
}

// Result describes what a successful extraction produced.
type Result struct {
	// Names are the basenames actually written, after any collision renaming,
	// in the order they arrived.
	Names []string

	// Bytes is the total written.
	Bytes int64
}

// Default limits, used when Options leaves them at zero.
const (
	DefaultMaxBytes int64 = 256 << 20
	DefaultMaxFiles       = 256
)

// ErrNoFiles reports that the archive parsed correctly but contained nothing
// this package is willing to write.
var ErrNoFiles = errors.New("drop: the archive contained no regular files")

// Extract reads a tar stream from r and writes its regular files into
// opts.Dir.
//
// It is deliberately lossy. Directory structure, ownership, modes, symlinks
// and every other archive feature are discarded; what survives is the file's
// basename and its contents. On any error, files already written by this call
// are removed, so a rejected drop leaves nothing behind to be mistaken for a
// complete one.
func Extract(r io.Reader, opts Options) (Result, error) {
	if opts.Dir == "" {
		return Result{}, errors.New("drop: no destination directory")
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	// maxFiles bounds what is written, which is not the same as what is read.
	// Entries this package skips — directories, symlinks, devices — are never
	// written and so never count against it, and an archive made entirely of
	// them would stream forever: the reader keeps making progress, so the
	// connection's idle deadline never fires, and the handler holds a
	// concurrency slot for as long as the sender cares to keep writing.
	//
	// Generous relative to maxFiles, because a legitimate archive of a
	// directory tree carries a directory header per level on top of its files.
	maxEntries := maxFiles * 16

	if err := os.MkdirAll(opts.Dir, dirPerm); err != nil {
		return Result{}, fmt.Errorf("drop: create %s: %w", opts.Dir, err)
	}

	var (
		res     Result
		written []string
	)
	// Anything that goes wrong after the first file is written unwinds the
	// whole drop: a partial archive on disk looks exactly like a complete one
	// and there is no way for the user to tell them apart later.
	cleanup := func() {
		for _, path := range written {
			_ = os.Remove(path)
		}
	}

	tr := tar.NewReader(r)
	for entries := 0; ; entries++ {
		if entries >= maxEntries {
			cleanup()
			return Result{}, fmt.Errorf("drop: archive holds more than %d entries", maxEntries)
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return Result{}, fmt.Errorf("drop: read archive: %w", err)
		}

		// Only regular files. Everything else a tar can describe — symlinks,
		// hard links, directories, character and block devices, FIFOs — is
		// either a way to write outside Dir or a way to create something the
		// sender has no business creating on this machine.
		if header.Typeflag != tar.TypeReg {
			continue
		}

		name, err := safeName(header.Name)
		if err != nil {
			cleanup()
			return Result{}, err
		}

		if len(res.Names) >= maxFiles {
			cleanup()
			return Result{}, fmt.Errorf("drop: archive holds more than %d files", maxFiles)
		}

		remaining := maxBytes - res.Bytes
		path, n, err := writeFile(tr, opts.Dir, name, remaining)
		if path != "" {
			written = append(written, path)
		}
		if err != nil {
			cleanup()
			return Result{}, err
		}

		res.Names = append(res.Names, filepath.Base(path))
		res.Bytes += n
	}

	if len(res.Names) == 0 {
		cleanup()
		return Result{}, ErrNoFiles
	}
	return res, nil
}

// Save writes a single stream into opts.Dir under name.
//
// This is the pipeline case — `pg_dump db | clipd drop dump.sql` — where there
// is no archive because the bytes never existed as a file. It shares every
// guarantee Extract has, and for the same reasons: the name is reduced to a
// bare filename, nothing is overwritten, nothing is made executable, the size
// is capped, and a failure leaves nothing behind.
func Save(r io.Reader, name string, opts Options) (Result, error) {
	if opts.Dir == "" {
		return Result{}, errors.New("drop: no destination directory")
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	safe, err := safeName(name)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(opts.Dir, dirPerm); err != nil {
		return Result{}, fmt.Errorf("drop: create %s: %w", opts.Dir, err)
	}

	path, n, err := writeFile(r, opts.Dir, safe, maxBytes)
	if err != nil {
		if path != "" {
			_ = os.Remove(path)
		}
		return Result{}, err
	}
	return Result{Names: []string{filepath.Base(path)}, Bytes: n}, nil
}

// safeName reduces an archive entry's name to a bare filename that cannot
// escape the destination directory.
//
// filepath.Base is what does the work, and it is why nothing here needs to
// reason about escaping: it discards every directory component, so
// "../../.ssh/authorized_keys" becomes "authorized_keys" and "/etc/passwd"
// becomes "passwd". The checks that follow only reject the handful of results
// that are not usable filenames at all.
func safeName(raw string) (string, error) {
	// Windows-style separators first: a tar written on another platform can
	// carry them, and Base on Unix would treat the whole thing as one name.
	name := filepath.Base(strings.ReplaceAll(raw, `\`, "/"))

	switch name {
	case "", ".", "..", string(filepath.Separator):
		return "", fmt.Errorf("drop: archive entry %q has no usable filename", raw)
	}
	if strings.ContainsRune(name, filepath.Separator) {
		// Unreachable after Base, and checked anyway: this is the invariant
		// the whole package rests on, and it costs one comparison to assert.
		return "", fmt.Errorf("drop: archive entry %q resolved to a path", raw)
	}
	// Control characters are refused rather than escaped on output. The name
	// is both written to disk and echoed back to the sender's terminal in the
	// acknowledgement, where an embedded newline forges a second response line
	// and an ANSI escape drives the terminal directly. Refusing them here fixes
	// both at once, and no filename worth having contains one.
	if i := strings.IndexFunc(name, isControl); i >= 0 {
		return "", fmt.Errorf("drop: archive entry %q contains a control character at byte %d", raw, i)
	}
	if len(name) > maxNameBytes {
		return "", fmt.Errorf("drop: archive entry name exceeds %d bytes", maxNameBytes)
	}
	return name, nil
}

// isControl reports whether r is a C0 control character or DEL.
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// writeFile writes one entry, returning the path created and the bytes
// written. The path is returned even on error so the caller can clean up a
// partial file.
func writeFile(r io.Reader, dir, name string, remaining int64) (string, int64, error) {
	if remaining <= 0 {
		return "", 0, errors.New("drop: archive exceeds the size limit")
	}

	f, path, err := createUnique(dir, name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	// One byte past the budget, so that exceeding it is detected here rather
	// than discovered after the disk has already taken the data. The header's
	// declared size is never trusted — only what actually arrives is counted.
	n, err := io.CopyN(f, r, remaining+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return path, n, fmt.Errorf("drop: write %s: %w", name, err)
	}
	if n > remaining {
		return path, n, errors.New("drop: archive exceeds the size limit")
	}
	if err := f.Close(); err != nil {
		return path, n, fmt.Errorf("drop: write %s: %w", name, err)
	}

	quarantine(path)
	return path, n, nil
}

// createUnique creates a new file under dir, adding a numeric suffix if the
// name is taken.
//
// O_EXCL is doing two jobs, and the second one is the load-bearing half.
//
// It refuses to overwrite a file already there, which is the difference
// between receiving a drop and losing one. And POSIX requires open() with
// O_CREAT|O_EXCL to fail on a symlink whatever the link points at — so it also
// refuses to follow a link an attacker planted in Dir, which is the difference
// between writing into Dir and writing anywhere on the filesystem. O_NOFOLLOW
// would restate that, and is not portable through the os package, so the
// guarantee is documented here instead.
func createUnique(dir, name string) (*os.File, string, error) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL

	stem, ext := splitName(name)
	for attempt := 0; attempt < maxCollisionAttempts; attempt++ {
		candidate := name
		if attempt > 0 {
			candidate = stem + "-" + strconv.Itoa(attempt) + ext
		}
		path := filepath.Join(dir, candidate)
		f, err := os.OpenFile(path, flags, filePerm)
		if err == nil {
			return f, path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", fmt.Errorf("drop: create %s: %w", candidate, err)
		}
	}
	return nil, "", fmt.Errorf("drop: %s already exists (and %d alternatives)", name, maxCollisionAttempts)
}

// splitName divides a filename into the part a suffix goes after and its
// extension, so "notes.txt" collides into "notes-1.txt" rather than
// "notes.txt-1".
//
// The leading dot of a dotfile is not an extension: ".bashrc" is all stem, or
// the alternatives would be "-1.bashrc".
func splitName(name string) (stem, ext string) {
	ext = filepath.Ext(name)
	if ext == name {
		return name, ""
	}
	return strings.TrimSuffix(name, ext), ext
}

// quarantine marks a file as having come from outside, so that macOS treats it
// the way it treats a browser download: Gatekeeper checks it, and opening it
// asks first.
//
// Best effort by design. The attribute is defence in depth on top of the
// extraction rules above, not a substitute for them, and a drop that landed
// safely should not be reported as failed because an optional label could not
// be applied.
func quarantine(path string) {
	if runtime.GOOS != "darwin" {
		return
	}
	xattr, err := exec.LookPath("xattr")
	if err != nil {
		return
	}
	// The format Gatekeeper expects: flags, timestamp, the agent that fetched
	// it, and a UUID slot this does not populate. 0081 means "quarantined,
	// not yet approved by the user".
	value := fmt.Sprintf("0081;%x;clipd;", time.Now().Unix())
	_ = exec.Command(xattr, "-w", "com.apple.quarantine", value, path).Run()
}
