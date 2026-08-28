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
	"io/fs"
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

// stagePrefix marks a drop that is still arriving.
//
// Bytes land under this prefix and take their real name only once they are
// complete, so a crash, a power cut, or a shutdown that outruns its grace
// period cannot leave a half-written file that looks exactly like a finished
// one. The leading dot also keeps a transfer in progress out of an ordinary
// listing of the drop directory.
const stagePrefix = ".clipd-part-"

// staleStageAge is how old a leftover staging file must be before CleanStale
// will remove it. Long enough that no transfer could still be using it.
const staleStageAge = time.Hour

// wirePerEntry is the wire budget each archive entry gets for its framing, on
// top of the bytes that reach disk: a 512-byte header, up to 511 bytes of
// padding, and room for the PAX records a long name or a large size needs.
const wirePerEntry = 4 << 10

// maxNameInError bounds how much of an archive's own name is quoted back into
// an error.
//
// A PAX header can carry a name approaching a megabyte, and every rejection
// interpolates one — into a response, and into a log file launchd never
// rotates.
const maxNameInError = 80

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
	// fail unwinds the files this call created and returns the error the caller
	// should see. Anything that goes wrong after the first file is written
	// undoes the whole drop: a partial archive on disk looks exactly like a
	// complete one, and there is no way for the user to tell them apart later.
	//
	// A removal that itself fails is reported rather than swallowed, because
	// files left behind by a rejected drop are precisely the state this unwind
	// exists to prevent.
	fail := func(err error) (Result, error) {
		stuck := 0
		for _, path := range written {
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				stuck++
			}
		}
		if stuck > 0 {
			return Result{}, fmt.Errorf("%w (and %d file(s) could not be removed from %s)",
				err, stuck, opts.Dir)
		}
		return Result{}, err
	}

	// The wire budget is the file bytes the caller allows, plus framing for the
	// most entries the archive may hold. Both terms are bounded by config
	// validation, so the sum cannot overflow.
	budget := maxBytes + int64(maxEntries)*wirePerEntry
	wire := &wireReader{r: r, remaining: budget}

	tr := tar.NewReader(wire)
	entries := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(readError(err, budget))
		}
		// Counted after the header arrives rather than before it, so an archive
		// holding exactly maxEntries is accepted rather than refused one short.
		entries++
		if entries > maxEntries {
			return fail(fmt.Errorf("drop: archive holds more than %d entries", maxEntries))
		}

		// Only regular files. Everything else a tar can describe — symlinks,
		// hard links, directories, character and block devices, FIFOs — is
		// either a way to write outside Dir or a way to create something the
		// sender has no business creating on this machine.
		if header.Typeflag != tar.TypeReg {
			// Skipping is not the same as ignoring. archive/tar discards a
			// skipped entry's declared body from the wire on the next call, so
			// an entry of an unsupported type that claims a huge size is a way
			// to send unbounded data that never counts against MaxBytes,
			// because none of it reaches disk. The wire budget above stops it
			// eventually; refusing it here says why.
			if header.Size > 0 {
				return fail(fmt.Errorf("drop: archive entry %q is type %q and declares a %d byte body",
					shortName(header.Name), rune(header.Typeflag), header.Size))
			}
			continue
		}

		name, err := safeName(header.Name)
		if err != nil {
			return fail(err)
		}

		if len(res.Names) >= maxFiles {
			return fail(fmt.Errorf("drop: archive holds more than %d files", maxFiles))
		}

		staged, n, err := stage(tr, opts.Dir, maxBytes-res.Bytes)
		if staged != "" {
			written = append(written, staged)
		}
		if err != nil {
			return fail(readError(err, budget))
		}

		// Marked before it is published, so the attribute is already on the
		// inode by the time the file has a name a user could open.
		quarantine(staged)
		path, err := publish(staged, opts.Dir, name)
		if err != nil {
			return fail(err)
		}
		// The bytes now live under the published name; track that instead, so
		// an unwind removes the file rather than a staging name that is gone.
		written[len(written)-1] = path

		res.Names = append(res.Names, filepath.Base(path))
		res.Bytes += n
	}

	if len(res.Names) == 0 {
		return fail(ErrNoFiles)
	}
	return res, nil
}

// wireReader bounds the total bytes read from a peer for one archive.
//
// The other limits here count what is written, which a hostile archive can
// decouple from what is sent. A run of PAX extended headers is consumed inside
// archive/tar and never surfaces as an entry at all, so the entry counter never
// advances and Next never returns; an entry whose type this package skips can
// still declare a body the reader discards from the wire. Neither writes a
// byte, so neither is caught by MaxBytes or MaxFiles, and every read refreshes
// the connection's idle deadline — so both stream for as long as the sender
// cares to keep going. This is the one bound that does not depend on the
// archive's account of itself.
type wireReader struct {
	r         io.Reader
	remaining int64
}

// errWireLimit is internal: readError turns it into something a user can act
// on, since "wire limit" surfacing out of archive/tar would say nothing.
var errWireLimit = errors.New("drop: wire budget exhausted")

func (w *wireReader) Read(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, errWireLimit
	}
	if int64(len(p)) > w.remaining {
		p = p[:w.remaining]
	}
	n, err := w.r.Read(p)
	w.remaining -= int64(n)
	return n, err
}

// readError explains a failure that came back through archive/tar.
func readError(err error, budget int64) error {
	if errors.Is(err, errWireLimit) {
		return fmt.Errorf("drop: sender exceeded the %d byte transfer budget for one archive", budget)
	}
	return fmt.Errorf("drop: read archive: %w", err)
}

// CleanStale removes staging files a daemon that did not shut down cleanly left
// behind, and reports how many went.
//
// Only files old enough that no transfer could still be using them are touched,
// so this is safe to call at startup without having to establish whether
// anything else is writing to the directory.
func CleanStale(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-staleStageAge)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), stagePrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, entry.Name())) == nil {
			removed++
		}
	}
	return removed
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

	staged, n, err := stage(r, opts.Dir, maxBytes)
	if err != nil {
		if staged != "" {
			_ = os.Remove(staged)
		}
		return Result{}, err
	}
	quarantine(staged)
	path, err := publish(staged, opts.Dir, safe)
	if err != nil {
		_ = os.Remove(staged)
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
		return "", fmt.Errorf("drop: archive entry %q has no usable filename", shortName(raw))
	}
	if strings.ContainsRune(name, filepath.Separator) {
		// Unreachable after Base, and checked anyway: this is the invariant
		// the whole package rests on, and it costs one comparison to assert.
		return "", fmt.Errorf("drop: archive entry %q resolved to a path", shortName(raw))
	}
	// Control characters are refused rather than escaped on output. The name
	// is both written to disk and echoed back to the sender's terminal in the
	// acknowledgement, where an embedded newline forges a second response line
	// and an ANSI escape drives the terminal directly. Refusing them here fixes
	// both at once, and no filename worth having contains one.
	if i := strings.IndexFunc(name, isControl); i >= 0 {
		return "", fmt.Errorf("drop: archive entry %q contains a control character at byte %d",
			shortName(raw), i)
	}
	if len(name) > maxNameBytes {
		return "", fmt.Errorf("drop: archive entry name exceeds %d bytes", maxNameBytes)
	}
	return name, nil
}

// shortName bounds an archive's own name before it is quoted into an error.
//
// Truncation is on bytes and may split a rune, which is harmless: every use
// site formats with %q, and that escapes an invalid byte rather than emitting
// it.
func shortName(raw string) string {
	if len(raw) <= maxNameInError {
		return raw
	}
	return raw[:maxNameInError] + "..."
}

// isControl reports whether r is a character no filename should carry.
//
// C0 and DEL are refused because the name is echoed back to the sender's
// terminal in the acknowledgement, where an embedded newline forges a second
// response line and an ANSI escape drives the terminal directly. C1 is refused
// for the same reason, since some terminals still act on it. The bidirectional
// overrides are refused because they make a name render as something other than
// what was written to disk, which turns the acknowledgement into a lie.
func isControl(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f:
		return true
	case r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// stage writes at most remaining bytes of r into a staging file in dir.
//
// The bytes land under a staging name rather than the caller's chosen one. The
// previous version created the file under its final name and wrote into it, so
// an interrupted transfer left a truncated file that was indistinguishable from
// a completed drop — and Extract's unwind only covers errors it lives to see,
// not a SIGKILL or a power cut.
func stage(r io.Reader, dir string, remaining int64) (path string, n int64, err error) {
	if remaining <= 0 {
		return "", 0, errors.New("drop: archive exceeds the size limit")
	}

	f, err := os.CreateTemp(dir, stagePrefix+"*")
	if err != nil {
		return "", 0, fmt.Errorf("drop: create staging file in %s: %w", dir, err)
	}
	path = f.Name()
	defer f.Close()

	// CreateTemp already uses 0600; setting it explicitly means the mode does
	// not depend on that remaining true.
	if err := f.Chmod(filePerm); err != nil {
		return path, 0, fmt.Errorf("drop: restrict staging file: %w", err)
	}

	// One byte past the budget, so that exceeding it is detected here rather
	// than discovered after the disk has already taken the data. The header's
	// declared size is never trusted — only what actually arrives is counted.
	n, err = io.CopyN(f, r, remaining+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return path, n, err
	}
	if n > remaining {
		return path, n, errors.New("drop: archive exceeds the size limit")
	}
	if err := f.Close(); err != nil {
		return path, n, fmt.Errorf("drop: write staging file: %w", err)
	}
	return path, n, nil
}

// publish gives a completed staging file its real name, adding a numeric
// suffix if that name is taken.
//
// os.Link rather than os.Rename, and the distinction is the load-bearing part:
// rename silently replaces whatever is already at the destination, which is the
// difference between receiving a drop and losing one. Link fails with EEXIST
// instead — the same guarantee O_CREATE|O_EXCL gave the version this replaced —
// and it does not follow a symlink planted at the destination either, so a link
// an attacker left in Dir cannot redirect the publish out of it.
//
// Bounded rather than open-ended so that a sender cannot make the daemon walk
// an unbounded sequence by repeatedly dropping the same name.
func publish(staged, dir, name string) (string, error) {
	stem, ext := splitName(name)
	for attempt := 0; attempt < maxCollisionAttempts; attempt++ {
		candidate := name
		if attempt > 0 {
			candidate = stem + "-" + strconv.Itoa(attempt) + ext
		}
		path := filepath.Join(dir, candidate)
		err := os.Link(staged, path)
		if err == nil {
			// Only the staging name goes; the content lives on under the
			// published name, which is now the same inode.
			_ = os.Remove(staged)
			return path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("drop: publish %s: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("drop: %s already exists (and %d alternatives)", name, maxCollisionAttempts)
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
