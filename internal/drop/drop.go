// Package drop receives legacy flat drops and versioned streaming directory
// drops. Payloads are privately staged, bounded and validated before publication.
// Files never overwrite existing names or inherit executable permissions.
package drop

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"unicode/utf8"
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

// Each in-flight request gets a private directory under stageDirPrefix. A
// directory, rather than a distinctive filename in the drop directory, gives
// cleanup a structurally separate namespace: a user may legitimately receive
// a file called ".clipd-part-notes", and cleanup must never mistake it for
// daemon state merely because its name shares a prefix.
const (
	stageDirPrefix = ".clipd-stage-"
	stageMarker    = ".clipd-owned"
	stageMarkerV1  = "clipd staging directory v1\n"
)

// StaleCleanupInterval is both the minimum age CleanStale will remove and how
// often the daemon revisits leftovers. It is longer than the server's absolute
// connection lifetime, so cleanup cannot race a live daemon transfer.
const StaleCleanupInterval = time.Hour

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
	Dir             string
	Context         context.Context
	Stream          bool
	Progress        func(string, int64, int64)
	PublishProgress func(int, int)

	// MaxBytes caps the total written across the whole archive, not per file.
	// A thousand small files are as capable of filling a disk as one large one.
	MaxBytes int64

	// MaxFiles caps how many entries may be written.
	MaxFiles int
}

// Result describes what a successful extraction produced.
type Result struct {
	// Names are the published top-level names after collision renaming.
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

// A transaction owns one private staging directory and the files accumulated
// there. Paths are relative to the opened drop root, so os.Root keeps every
// operation confined even if a directory entry is replaced with a symlink.
type transaction struct {
	dir   string
	files []stagedFile
}

type stagedFile struct {
	path string
	name string
}

type publishedFile struct {
	staged string
	name   string
}

// beginTransaction creates a name that cannot collide predictably with a
// sender-controlled filename. The marker is what lets CleanStale distinguish
// clipd state from a directory a user happened to name similarly.
func beginTransaction(root *os.Root) (transaction, error) {
	for range 10 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return transaction{}, fmt.Errorf("drop: generate staging name: %w", err)
		}
		dir := stageDirPrefix + hex.EncodeToString(random[:])
		if err := root.Mkdir(dir, dirPerm); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return transaction{}, fmt.Errorf("drop: create staging directory: %w", err)
		}
		if err := writeStageMarker(root, dir); err != nil {
			_ = root.Remove(dir)
			return transaction{}, fmt.Errorf("drop: mark staging directory: %w", err)
		}
		return transaction{dir: dir}, nil
	}
	return transaction{}, errors.New("drop: could not allocate a unique staging directory")
}

func writeStageMarker(root *os.Root, dir string) error {
	f, err := root.OpenFile(filepath.Join(dir, stageMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, stageMarkerV1); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func hasStageMarker(root *os.Root, dir string) bool {
	f, err := root.Open(filepath.Join(dir, stageMarker))
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(len(stageMarkerV1)+1)))
	return err == nil && string(data) == stageMarkerV1
}

// removeTransaction removes only the exact flat shape beginTransaction and
// stage create. Refusing unexpected entries is intentional: even with a valid
// marker, cleanup must not recursively delete a directory somebody repurposed.
func removeTransaction(root *os.Root, dir string) error {
	f, err := root.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, readErr := f.ReadDir(-1)
	closeErr := f.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != stageMarker {
			index, err := strconv.Atoi(name)
			if err != nil || index < 0 || strconv.Itoa(index) != name {
				return fmt.Errorf("unexpected entry %q", name)
			}
		}
		info, err := root.Lstat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected non-regular entry %q", name)
		}
	}
	for _, entry := range entries {
		if err := root.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return root.Remove(dir)
}

// rollback removes final names before staging state. A cleanup failure is
// included in the returned error: silently leaving a file after reporting a
// rejected drop would break the all-or-nothing guarantee this path exists for.
func rollback(root *os.Root, stageDir string, published []publishedFile, cause error) error {
	var cleanup []error
	for _, file := range published {
		// A watcher may have moved the received file and put something else at
		// its name before a later publish failed. Compare inode identity with the
		// still-staged hard link so rollback never deletes that replacement.
		stagedInfo, stagedErr := root.Stat(file.staged)
		finalInfo, finalErr := root.Lstat(file.name)
		if errors.Is(finalErr, os.ErrNotExist) {
			continue
		}
		if stagedErr != nil || finalErr != nil {
			cleanup = append(cleanup, fmt.Errorf("verify published %s before removal: %v", file.name, errors.Join(stagedErr, finalErr)))
			continue
		}
		if !os.SameFile(stagedInfo, finalInfo) {
			cleanup = append(cleanup, fmt.Errorf("published %s was replaced before rollback; left the replacement untouched", file.name))
			continue
		}
		if err := root.Remove(file.name); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanup = append(cleanup, fmt.Errorf("remove published %s: %w", file.name, err))
		}
	}
	if err := removeTransaction(root, stageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		cleanup = append(cleanup, fmt.Errorf("remove staging directory: %w", err))
	}
	if len(cleanup) > 0 {
		return fmt.Errorf("%w (cleanup also failed: %v)", cause, errors.Join(cleanup...))
	}
	return cause
}

// Extract reads a tar stream from r and writes its regular files into
// opts.Dir.
//
// It is deliberately lossy. Directory structure, ownership, modes, symlinks
// and every other archive feature are discarded; what survives is the file's
// basename and its contents. On any error, files already written by this call
// are removed, so a rejected drop leaves nothing behind to be mistaken for a
// complete one.
func Extract(r io.Reader, opts Options) (Result, error) {
	if opts.Stream {
		return extractStream(r, opts)
	}
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
	root, err := os.OpenRoot(opts.Dir)
	if err != nil {
		return Result{}, fmt.Errorf("drop: open destination %s: %w", opts.Dir, err)
	}
	defer root.Close()

	txn, err := beginTransaction(root)
	if err != nil {
		return Result{}, err
	}
	var published []publishedFile
	// No final name is created until the complete archive has parsed. If
	// publishing or cleanup then fails, remove every final name this request
	// created as well as its private staging directory.
	fail := func(cause error) (Result, error) {
		return Result{}, rollback(root, txn.dir, published, cause)
	}

	var res Result

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

		if len(txn.files) >= maxFiles {
			return fail(fmt.Errorf("drop: archive holds more than %d files", maxFiles))
		}

		staged, n, err := stage(tr, root, txn.dir, len(txn.files), maxBytes-res.Bytes)
		if err != nil {
			return fail(readError(err, budget))
		}
		txn.files = append(txn.files, stagedFile{path: staged, name: name})
		res.Bytes += n
	}

	if len(txn.files) == 0 {
		return fail(ErrNoFiles)
	}

	// Publication begins only after Next returned EOF for the whole archive.
	// A corrupt or truncated later entry therefore cannot expose the valid
	// prefix as though it were the complete drop.
	for _, file := range txn.files {
		// Marked before it is published, so the attribute is already on the
		// inode by the time the file has a name a user could open.
		if err := requestContext(opts).Err(); err != nil {
			return fail(err)
		}
		if err := verifyRootPath(root); err != nil {
			return fail(err)
		}
		quarantine(requestContext(opts), filepath.Join(opts.Dir, file.path))
		if err := requestContext(opts).Err(); err != nil {
			return fail(err)
		}
		name, err := publish(root, file.path, file.name)
		if err != nil {
			return fail(err)
		}
		published = append(published, publishedFile{staged: file.path, name: name})
		res.Names = append(res.Names, name)
	}
	if err := removeTransaction(root, txn.dir); err != nil {
		return fail(fmt.Errorf("drop: remove completed staging directory: %w", err))
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

// CleanStale removes transaction directories a daemon that did not shut down
// cleanly left behind, and reports how many went.
//
// A matching name is not sufficient. The entry must be a directory, contain
// clipd's exact ownership marker, and be older than any server connection can
// remain alive. This deliberately leaves legacy .clipd-part-* files alone:
// those names were not reserved, so an old one may be a user's real file.
func CleanStale(dir string, minimumAge ...time.Duration) int {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0
	}
	defer root.Close()
	dirFile, err := root.Open(".")
	if err != nil {
		return 0
	}
	entries, err := dirFile.ReadDir(-1)
	_ = dirFile.Close()
	if err != nil {
		return 0
	}

	age := StaleCleanupInterval
	if len(minimumAge) > 0 && minimumAge[0] > age {
		age = minimumAge[0]
	}
	cutoff := time.Now().Add(-age)
	removed := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), stageDirPrefix) {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err != nil || !info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		if !hasStageMarker(root, entry.Name()) {
			continue
		}
		if removeTransaction(root, entry.Name()) == nil {
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
	root, err := os.OpenRoot(opts.Dir)
	if err != nil {
		return Result{}, fmt.Errorf("drop: open destination %s: %w", opts.Dir, err)
	}
	defer root.Close()
	txn, err := beginTransaction(root)
	if err != nil {
		return Result{}, err
	}

	source := r
	if opts.Progress != nil {
		source = &fileProgress{r: r, name: safe, size: -1, report: opts.Progress}
	}
	staged, n, err := stage(source, root, txn.dir, 0, maxBytes)
	if err != nil {
		return Result{}, rollback(root, txn.dir, nil, err)
	}
	if err := requestContext(opts).Err(); err != nil {
		return Result{}, rollback(root, txn.dir, nil, err)
	}
	if opts.PublishProgress != nil {
		opts.PublishProgress(0, 1)
	}
	if err := verifyRootPath(root); err != nil {
		return Result{}, rollback(root, txn.dir, nil, err)
	}
	quarantine(requestContext(opts), filepath.Join(opts.Dir, staged))
	if err := requestContext(opts).Err(); err != nil {
		return Result{}, rollback(root, txn.dir, nil, err)
	}
	finalName, err := publish(root, staged, safe)
	if err != nil {
		return Result{}, rollback(root, txn.dir, nil, err)
	}
	if err := removeTransaction(root, txn.dir); err != nil {
		return Result{}, rollback(root, txn.dir, []publishedFile{{staged: staged, name: finalName}},
			fmt.Errorf("drop: remove completed staging directory: %w", err))
	}
	return Result{Names: []string{finalName}, Bytes: n}, nil
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
	// IndexFunc below validates Unicode control characters rune by rune. Invalid
	// UTF-8 would be decoded as RuneError instead, allowing a raw C1 byte such as
	// 0x9b to reach the terminal acknowledgement unchecked. Reject malformed
	// names before that conversion rather than letting the filesystem and
	// terminal disagree about what arrived.
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("drop: archive entry %q has a filename that is not valid UTF-8", shortName(raw))
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

// stage writes at most remaining bytes of r into one transaction file.
//
// The bytes land in a private directory rather than under the caller's chosen
// name. os.Root confines the create beneath the already-open drop directory,
// including if a directory entry is replaced with a symlink on a shared path.
func stage(r io.Reader, root *os.Root, txnDir string, index int, remaining int64) (path string, n int64, err error) {
	if remaining < 0 {
		return "", 0, errors.New("drop: archive exceeds the size limit")
	}

	path = filepath.Join(txnDir, strconv.Itoa(index))
	f, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return "", 0, fmt.Errorf("drop: create staging file: %w", err)
	}
	defer f.Close()

	// OpenFile's mode is still subject to umask. Chmod explicitly so this does
	// not depend on the daemon having installed its restrictive one first.
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
	// The file must be durable before a final name can point at its inode.
	if err := f.Sync(); err != nil {
		return path, n, fmt.Errorf("drop: sync staging file: %w", err)
	}
	if err := f.Close(); err != nil {
		return path, n, fmt.Errorf("drop: write staging file: %w", err)
	}
	return path, n, nil
}

// verifyRootPath catches root replacement before APIs that cannot use os.Root.
// This is a check, not an atomic guarantee against concurrent pathname changes.
func verifyRootPath(root *os.Root) error {
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	named, err := os.Stat(root.Name())
	if err != nil {
		return err
	}
	if !os.SameFile(opened, named) {
		return errors.New("drop: destination root changed during transfer")
	}
	return nil
}

// publish gives a completed transaction file its real name, adding a numeric
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
func publish(root *os.Root, staged, name string) (string, error) {
	if err := verifyRootPath(root); err != nil {
		return "", err
	}
	stem, ext := splitName(name)
	for attempt := 0; attempt < maxCollisionAttempts; attempt++ {
		candidate := name
		if attempt > 0 {
			candidate = stem + "-" + strconv.Itoa(attempt) + ext
		}
		// Go 1.24's os.Root does not yet expose Link. Both names are still
		// derived inside the already-open root: staged is generated by this
		// package and candidate is a validated basename.
		err := os.Link(filepath.Join(root.Name(), staged), filepath.Join(root.Name(), candidate))
		if err == nil {
			// The staging link remains until every file in the transaction has
			// a final name. Removing the transaction directory then commits the
			// whole validated archive; rollback can remove the final links first.
			return candidate, nil
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
func requestContext(opts Options) context.Context {
	if opts.Context != nil {
		return opts.Context
	}
	return context.Background()
}

func quarantine(ctx context.Context, path string) {
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, xattr, "-w", "com.apple.quarantine", value, path)
	cmd.WaitDelay = time.Second
	_ = cmd.Run()
}
