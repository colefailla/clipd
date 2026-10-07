package drop

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Completion is sent only after tar exits successfully. Old archive requests
// retain their original meaning; streaming requests require this trailer.
const Completion = "clipd:complete:v2\n"

// completedArchive accepts tar's zero padding followed by exactly one trailer
// and EOF. Draining through wireReader keeps padding and trailers bounded too.
func completedArchive(r io.Reader) error {
	marker := 0
	var buf [4096]byte
	for {
		n, err := r.Read(buf[:])
		for _, b := range buf[:n] {
			if marker == 0 && b == 0 {
				continue
			}
			if marker >= len(Completion) || b != Completion[marker] {
				return errors.New("drop: missing or invalid completion marker; nothing was published")
			}
			marker++
		}
		if errors.Is(err, io.EOF) {
			if marker != len(Completion) {
				return errors.New("drop: transfer ended without a completion marker; nothing was published")
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// archivePath deliberately rejects traversal rather than cleaning it away.
// Only ordinary relative paths are accepted, with bounded depth and length.
func archivePath(raw string) (string, error) {
	for strings.HasPrefix(raw, "./") {
		raw = strings.TrimPrefix(raw, "./")
	}
	raw = strings.TrimSuffix(raw, "/")
	if raw == "" || strings.HasPrefix(raw, "/") || strings.Contains(raw, `\`) || len(raw) > 2048 {
		return "", errors.New("drop: invalid archive path")
	}
	parts := strings.Split(raw, "/")
	if len(parts) > 32 || strings.HasPrefix(parts[0], stageDirPrefix) {
		return "", errors.New("drop: archive path is too deep or uses a reserved staging name")
	}
	for _, part := range parts {
		name, err := safeName(part)
		if err != nil || name != part {
			return "", errors.New("drop: unsafe archive path component")
		}
	}
	return filepath.Join(parts...), nil
}

type ownedDir struct {
	name string
	info os.FileInfo
}

// Extract reads a tar stream from r, followed by the sender's completion
// marker, and publishes its regular files and directories beneath opts.Dir.
//
// Payloads are staged in a private, marker-owned directory and nothing gets a
// final name until the whole stream has validated, so a rejected or truncated
// drop leaves nothing behind. Existing destination trees are never merged into
// or overwritten. Symlinks, hard links and devices are skipped and counted.
func Extract(r io.Reader, opts Options) (Result, error) {
	maxBytes, maxFiles := opts.MaxBytes, opts.MaxFiles
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	if err := os.MkdirAll(opts.Dir, dirPerm); err != nil {
		return Result{}, err
	}
	root, err := os.OpenRoot(opts.Dir)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	txn, err := beginTransaction(root)
	if err != nil {
		return Result{}, err
	}
	var published []publishedFile
	var dirs []ownedDir
	fail := func(cause error) (Result, error) {
		cause = rollback(root, txn.dir, published, cause)
		for i := len(dirs) - 1; i >= 0; i-- {
			info, err := root.Lstat(dirs[i].name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !os.SameFile(info, dirs[i].info) {
				cause = fmt.Errorf("%w (directory replaced during cleanup; left untouched)", cause)
				continue
			}
			if err := root.Remove(dirs[i].name); err != nil {
				cause = fmt.Errorf("%w (remove directory: %v)", cause, err)
			}
		}
		return Result{}, cause
	}
	// The wire budget is the file bytes allowed plus framing for the most
	// entries the archive may hold. Both terms are bounded by config
	// validation, so the sum cannot overflow.
	budget := maxBytes + int64(maxFiles*16)*wirePerEntry
	wire := &wireReader{r: r, remaining: budget}
	tr := tar.NewReader(wire)
	paths := make(map[string]bool) // true means directory
	var res Result
	for entries := 0; ; entries++ {
		if err := requestContext(opts).Err(); err != nil {
			return fail(err)
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(readError(err, budget))
		}
		if entries >= maxFiles*16 {
			return fail(errors.New("drop: archive exceeds the entry limit"))
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// Never written: a symlink or device has no meaning on the Mac and
			// could point outside the drop directory. A body here would be
			// discarded unseen by MaxBytes, so one is refused.
			if h.Size != 0 {
				return fail(errors.New("drop: special archive entry has a payload"))
			}
			res.Skipped++
			continue
		default:
			return fail(errors.New("drop: streaming archives accept only files, directories and links"))
		}
		if h.Typeflag == tar.TypeDir && h.Size != 0 {
			return fail(errors.New("drop: directory has a payload"))
		}
		name, err := archivePath(h.Name)
		if err != nil {
			return fail(err)
		}
		isDir := h.Typeflag == tar.TypeDir
		if old, exists := paths[name]; exists && (!old || !isDir) {
			return fail(errors.New("drop: duplicate or conflicting archive path"))
		}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			if old, exists := paths[parent]; exists && !old {
				return fail(errors.New("drop: file used as an archive directory"))
			}
			paths[parent] = true
		}
		paths[name] = isDir
		if len(paths) > maxFiles*16 {
			return fail(errors.New("drop: too many archive paths"))
		}
		if isDir {
			continue
		}
		if len(txn.files) >= maxFiles {
			return fail(ErrTooManyFiles)
		}
		source := io.Reader(tr)
		if opts.Progress != nil {
			source = &fileProgress{r: tr, name: name, size: h.Size, report: opts.Progress}
		}
		staged, n, err := stage(source, root, txn.dir, len(txn.files), maxBytes-res.Bytes)
		if errors.Is(err, errWireLimit) {
			err = readError(err, budget)
		}
		if err != nil {
			return fail(err)
		}
		txn.files = append(txn.files, stagedFile{path: staged, name: name})
		res.Bytes += n
	}
	if err := completedArchive(wire); err != nil {
		return fail(err)
	}
	if len(paths) == 0 {
		return fail(ErrNoFiles)
	}
	if err := requestContext(opts).Err(); err != nil {
		return fail(err)
	}
	var directoryNames []string
	for name, isDir := range paths {
		if isDir {
			directoryNames = append(directoryNames, name)
		}
	}
	sort.Strings(directoryNames) // parents precede children
	renamed := make(map[string]string)
	identities := make(map[string]os.FileInfo)
	verifyParents := func(name string) error {
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			info, err := root.Lstat(parent)
			expected := identities[parent]
			if err != nil || expected == nil || !info.IsDir() || !os.SameFile(info, expected) {
				return errors.New("drop: destination directory changed during publication")
			}
		}
		return nil
	}
	makeDir := func(name string) error {
		if err := requestContext(opts).Err(); err != nil {
			return err
		}
		if opts.PublishProgress != nil {
			opts.PublishProgress(0, len(txn.files))
		}
		if err := verifyParents(name); err != nil {
			return err
		}
		if err := root.Mkdir(name, dirPerm); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("drop: reserved directory was replaced")
		}
		dirs = append(dirs, ownedDir{name: name, info: info})
		identities[name] = info
		return nil
	}
	for _, name := range directoryNames {
		top, rest, _ := strings.Cut(name, string(filepath.Separator))
		if rest == "" {
			for attempt := 0; ; attempt++ {
				if attempt >= maxCollisionAttempts {
					return fail(errors.New("drop: too many directory name collisions"))
				}
				candidate := top
				if attempt > 0 {
					candidate = fmt.Sprintf("%s-%d", top, attempt)
				}
				err := makeDir(candidate)
				if errors.Is(err, os.ErrExist) {
					continue
				}
				if err != nil {
					return fail(err)
				}
				renamed[top] = candidate
				res.Names = append(res.Names, candidate)
				break
			}
		} else {
			if err := makeDir(filepath.Join(renamed[top], rest)); err != nil {
				return fail(err)
			}
		}
	}
	for index, file := range txn.files {
		if opts.PublishProgress != nil {
			opts.PublishProgress(index, len(txn.files))
		}
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
		target := file.name
		top, rest, nested := strings.Cut(target, string(filepath.Separator))
		if nested {
			target = filepath.Join(renamed[top], rest)
		}
		if err := verifyParents(target); err != nil {
			return fail(err)
		}
		final, err := publish(root, file.path, target)
		if err != nil {
			return fail(err)
		}
		published = append(published, publishedFile{staged: file.path, name: final})
		if !nested {
			res.Names = append(res.Names, final)
		}
	}
	if err := removeTransaction(root, txn.dir); err != nil {
		return fail(err)
	}
	return res, nil
}

type fileProgress struct {
	r          io.Reader
	name       string
	size, read int64
	report     func(string, int64, int64)
}

func (p *fileProgress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if n > 0 {
		p.report(p.name, p.read, p.size)
	}
	return n, err
}
