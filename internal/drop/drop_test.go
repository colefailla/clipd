package drop

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// entry describes one archive member for the test archive builder.
type entry struct {
	name     string
	body     string
	typeflag byte
	mode     int64
	linkname string
}

// archiveOf builds a tar stream in memory. It writes headers exactly as
// given, including ones a well-behaved tar would never produce, because those
// are the interesting cases.
func archiveOf(t *testing.T, entries ...entry) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Typeflag: flag,
			Linkname: e.linkname,
		}
		if flag != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return &buf
}

// TestExtractConfinesHostileNames is the test this package exists for.
//
// Every name here is a real archive-extraction attack. The property being
// pinned is not "these particular strings are handled" but the invariant that
// makes them all harmless at once: nothing is ever written outside Dir,
// because every entry is reduced to a basename before it becomes a path.
func TestExtractConfinesHostileNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		give string
		want string
	}{
		{"parent traversal", "../../.ssh/authorized_keys", "authorized_keys"},
		{"deep traversal", "../../../../../../etc/passwd", "passwd"},
		{"absolute path", "/etc/passwd", "passwd"},
		{"absolute traversal", "/../../etc/shadow", "shadow"},
		{"windows separators", `..\..\Windows\System32\drivers\etc\hosts`, "hosts"},
		{"embedded traversal", "a/b/../../../../c.txt", "c.txt"},
		{"trailing dot segments", "notes/./x.txt", "x.txt"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := filepath.Join(t.TempDir(), "drop")
			res, err := Extract(archiveOf(t, entry{name: tc.give, body: "payload"}), Options{Dir: dir})
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(res.Names) != 1 || res.Names[0] != tc.want {
				t.Fatalf("wrote %v, want exactly [%s]", res.Names, tc.want)
			}

			// The written file is inside Dir, and Dir holds nothing else.
			written := filepath.Join(dir, tc.want)
			if _, err := os.Stat(written); err != nil {
				t.Fatalf("stat %s: %v", written, err)
			}
			ents, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read dir: %v", err)
			}
			if len(ents) != 1 {
				t.Fatalf("drop directory holds %d entries, want 1", len(ents))
			}
		})
	}
}

// TestExtractNothingEscapesTheParent is the same property checked from the
// other side: after extracting an archive full of traversal attempts, the
// directory above Dir must be untouched.
func TestExtractNothingEscapesTheParent(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "drop")

	_, err := Extract(archiveOf(t,
		entry{name: "../escaped.txt", body: "no"},
		entry{name: "../../escaped2.txt", body: "no"},
		entry{name: "/tmp/escaped3.txt", body: "no"},
	), Options{Dir: dir})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	ents, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "drop" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("parent directory holds %v, want only [drop]", names)
	}
}

// TestExtractSkipsNonRegularEntries covers every tar type that is a way to
// write somewhere else or to create something a sender has no business
// creating: links of both kinds, directories, devices and FIFOs.
func TestExtractSkipsNonRegularEntries(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "drop")
	res, err := Extract(archiveOf(t,
		entry{name: "evil-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		entry{name: "evil-hard", typeflag: tar.TypeLink, linkname: "/etc/passwd"},
		entry{name: "subdir", typeflag: tar.TypeDir},
		entry{name: "evil-dev", typeflag: tar.TypeChar},
		entry{name: "evil-blk", typeflag: tar.TypeBlock},
		entry{name: "evil-fifo", typeflag: tar.TypeFifo},
		entry{name: "real.txt", body: "kept"},
	), Options{Dir: dir})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Names) != 1 || res.Names[0] != "real.txt" {
		t.Fatalf("wrote %v, want only [real.txt]", res.Names)
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("drop directory holds %d entries, want 1", len(ents))
	}
	// Specifically: no symlink was created, under any name.
	for _, e := range ents {
		if e.Type()&os.ModeSymlink != 0 {
			t.Errorf("%s is a symlink", e.Name())
		}
	}
}

// TestExtractWillNotFollowAPlantedSymlink pins the reason publish uses
// os.Link rather than os.Rename. With a symlink already sitting at the target
// name, a rename would replace the link and an ordinary open would write
// straight through it to whatever it points at; link refuses outright.
func TestExtractWillNotFollowAPlantedSymlink(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	dir := filepath.Join(tmp, "drop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(tmp, "victim.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res, err := Extract(archiveOf(t, entry{name: "notes.txt", body: "overwritten"}), Options{Dir: dir})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("the symlink was followed: victim now holds %q", got)
	}
	if len(res.Names) != 1 || res.Names[0] == "notes.txt" {
		t.Fatalf("wrote %v, want a renamed file rather than notes.txt", res.Names)
	}
}

func TestExtractNeverOverwrites(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "drop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("first"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := Extract(archiveOf(t, entry{name: "notes.txt", body: "second"}), Options{Dir: dir})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Names[0] != "notes-1.txt" {
		t.Errorf("wrote %q, want notes-1.txt", res.Names[0])
	}
	original, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(original) != "first" {
		t.Errorf("the original was modified: %q", original)
	}
}

func TestExtractStripsTheExecutableBit(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "drop")
	if _, err := Extract(archiveOf(t,
		entry{name: "payload.sh", body: "#!/bin/sh\n", mode: 0o777},
	), Options{Dir: dir}); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "payload.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("mode = %04o, want %04o", perm, filePerm)
	}
}

func TestExtractEnforcesLimits(t *testing.T) {
	t.Parallel()

	t.Run("total bytes", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "drop")
		_, err := Extract(archiveOf(t,
			entry{name: "a.bin", body: strings.Repeat("x", 600)},
			entry{name: "b.bin", body: strings.Repeat("x", 600)},
		), Options{Dir: dir, MaxBytes: 1000})
		if err == nil {
			t.Fatal("Extract succeeded, want a size-limit error")
		}
		assertDirEmpty(t, dir)
	})

	t.Run("a single oversized file", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "drop")
		_, err := Extract(archiveOf(t,
			entry{name: "big.bin", body: strings.Repeat("x", 5000)},
		), Options{Dir: dir, MaxBytes: 1000})
		if err == nil {
			t.Fatal("Extract succeeded, want a size-limit error")
		}
		assertDirEmpty(t, dir)
	})

	t.Run("file count", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "drop")
		var entries []entry
		for i := range 10 {
			entries = append(entries, entry{name: string(rune('a'+i)) + ".txt", body: "x"})
		}
		_, err := Extract(archiveOf(t, entries...), Options{Dir: dir, MaxFiles: 3})
		if err == nil {
			t.Fatal("Extract succeeded, want a file-count error")
		}
		assertDirEmpty(t, dir)
	})
}

// TestExtractCleansUpAfterAFailure pins the all-or-nothing property: a
// rejected drop must not leave files behind that look like a complete one.
func TestExtractCleansUpAfterAFailure(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "drop")
	_, err := Extract(archiveOf(t,
		entry{name: "ok-1.txt", body: "fine"},
		entry{name: "ok-2.txt", body: "fine"},
		entry{name: "toobig.bin", body: strings.Repeat("x", 5000)},
	), Options{Dir: dir, MaxBytes: 1000})
	if err == nil {
		t.Fatal("Extract succeeded, want a size-limit error")
	}
	assertDirEmpty(t, dir)
}

// A valid prefix is not a complete archive. In particular, tar can emit one
// good file and then fail while opening the next input. Nothing from that
// prefix may become visible under a final name while Extract is still waiting
// to learn whether the rest of the archive is valid.
func TestExtractPublishesOnlyAfterTheWholeArchiveArrives(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "drop")
	pr, pw := io.Pipe()
	firstWritten := make(chan struct{})
	finish := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(pw)
		if err := tw.WriteHeader(&tar.Header{Name: "first.txt", Mode: 0o600, Size: 5, Typeflag: tar.TypeReg}); err != nil {
			writerDone <- err
			return
		}
		if _, err := tw.Write([]byte("first")); err != nil {
			writerDone <- err
			return
		}
		if err := tw.Flush(); err != nil {
			writerDone <- err
			return
		}
		close(firstWritten)
		<-finish
		if err := tw.Close(); err != nil {
			writerDone <- err
			return
		}
		writerDone <- pw.Close()
	}()

	type outcome struct {
		res Result
		err error
	}
	extracted := make(chan outcome, 1)
	go func() {
		res, err := Extract(pr, Options{Dir: dir})
		extracted <- outcome{res: res, err: err}
	}()
	<-firstWritten

	// Wait until the first entry has reached private staging; checking sooner
	// would prove only that the extractor had not read it yet.
	deadline := time.Now().Add(2 * time.Second)
	for !hasStagedData(t, dir) {
		if time.Now().After(deadline) {
			t.Fatal("first archive entry never reached staging")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "first.txt")); !os.IsNotExist(err) {
		t.Fatalf("first.txt became visible before archive EOF: %v", err)
	}

	close(finish)
	if err := <-writerDone; err != nil {
		t.Fatalf("write archive: %v", err)
	}
	got := <-extracted
	if got.err != nil {
		t.Fatalf("Extract: %v", got.err)
	}
	if len(got.res.Names) != 1 || got.res.Names[0] != "first.txt" {
		t.Fatalf("wrote %v, want [first.txt]", got.res.Names)
	}
}

func hasStagedData(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read drop dir: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), stageDirPrefix) {
			continue
		}
		staged, err := os.ReadDir(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read staging dir: %v", err)
		}
		for _, file := range staged {
			if file.Name() == "0" {
				return true
			}
		}
	}
	return false
}

func TestCleanStaleRemovesOnlyMarkedTransactionDirectories(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".clipd-part-real-file")
	if err := os.WriteFile(legacy, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write legacy-shaped file: %v", err)
	}
	lookalike := filepath.Join(dir, stageDirPrefix+"user-directory")
	if err := os.Mkdir(lookalike, 0o700); err != nil {
		t.Fatalf("make lookalike: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lookalike, "notes"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("write lookalike: %v", err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	stale, err := beginTransaction(root)
	if err != nil {
		root.Close()
		t.Fatalf("begin stale transaction: %v", err)
	}
	fresh, err := beginTransaction(root)
	if err != nil {
		root.Close()
		t.Fatalf("begin fresh transaction: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close root: %v", err)
	}

	old := time.Now().Add(-2 * StaleCleanupInterval)
	for _, path := range []string{legacy, lookalike, filepath.Join(dir, stale.dir)} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("age %s: %v", path, err)
		}
	}

	if got := CleanStale(dir); got != 1 {
		t.Fatalf("CleanStale removed %d transactions, want 1", got)
	}
	for _, path := range []string{legacy, lookalike, filepath.Join(dir, fresh.dir)} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("CleanStale removed %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, stale.dir)); !os.IsNotExist(err) {
		t.Errorf("stale transaction still exists: %v", err)
	}
}

func TestRollbackDoesNotDeleteAReplacedPublishedName(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()
	txn, err := beginTransaction(root)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	staged, _, err := stage(strings.NewReader("received"), root, txn.dir, 0, 64)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	name, err := publish(root, staged, "report.txt")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := root.Remove(name); err != nil {
		t.Fatalf("move published name out of the way: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("user replacement"), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	err = rollback(root, txn.dir, []publishedFile{{staged: staged, name: name}}, errors.New("later publish failed"))
	if err == nil || !strings.Contains(err.Error(), "left the replacement untouched") {
		t.Fatalf("rollback error = %v, want a replacement warning", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read replacement: %v", err)
	}
	if string(got) != "user replacement" {
		t.Fatalf("rollback changed replacement to %q", got)
	}
}

// TestSafeNameRejectsUnusableNames exercises safeName directly rather than
// through Extract, because Go's tar writer refuses to encode some of these —
// a trailing slash, for instance. A hostile archive is not produced by Go's
// writer, so the extractor still has to cope with them.
func TestSafeNameRejectsUnusableNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"", ".", "..", "/", "//", "../", "a/b/..",
		strings.Repeat("n", maxNameBytes+1),
	} {
		if got, err := safeName(name); err == nil {
			t.Errorf("safeName(%q) = %q, want an error", name, got)
		}
	}
}

// TestSafeNameKeepsUsableNames is the other half: legitimate filenames,
// including dotfiles and names with spaces, must survive intact.
func TestSafeNameKeepsUsableNames(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"notes.txt":            "notes.txt",
		"a/b/c/notes.txt":      "notes.txt",
		".bashrc":              ".bashrc",
		"my report (final).md": "my report (final).md",
		"árvíztűrő.txt":        "árvíztűrő.txt",
		// Base strips trailing slashes, so this lands as a plain filename
		// inside Dir rather than being rejected. Safe, and worth pinning so
		// the behaviour is not mistaken for an oversight later.
		"some/dir/": "dir",
	}
	for give, want := range tests {
		got, err := safeName(give)
		if err != nil {
			t.Errorf("safeName(%q): %v", give, err)
			continue
		}
		if got != want {
			t.Errorf("safeName(%q) = %q, want %q", give, got, want)
		}
	}
}

// TestExtractRejectsAnOverlongName goes through Extract for the one unusable
// case the tar writer will happily encode.
func TestExtractRejectsAnOverlongName(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "drop")
	_, err := Extract(archiveOf(t,
		entry{name: strings.Repeat("n", maxNameBytes+1), body: "x"},
	), Options{Dir: dir})
	if err == nil {
		t.Fatal("Extract accepted an overlong name")
	}
	assertDirEmpty(t, dir)
}

func TestExtractRejectsAnEmptyArchive(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "drop")
	_, err := Extract(archiveOf(t), Options{Dir: dir})
	if !errors.Is(err, ErrNoFiles) {
		t.Fatalf("err = %v, want ErrNoFiles", err)
	}
}

func TestExtractRejectsGarbage(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "drop")
	_, err := Extract(strings.NewReader("this is not a tar archive at all"), Options{Dir: dir})
	if err == nil {
		t.Fatal("Extract accepted non-archive input")
	}
}

func TestExtractPreservesContentAndOrder(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "drop")
	res, err := Extract(archiveOf(t,
		entry{name: "one.txt", body: "first"},
		entry{name: "two.txt", body: "second"},
	), Options{Dir: dir})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := strings.Join(res.Names, ","); got != "one.txt,two.txt" {
		t.Errorf("names = %s, want one.txt,two.txt", got)
	}
	if res.Bytes != int64(len("first")+len("second")) {
		t.Errorf("bytes = %d, want %d", res.Bytes, len("first")+len("second"))
	}
	for name, want := range map[string]string{"one.txt": "first", "two.txt": "second"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestSplitName(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, stem, ext string }{
		{"notes.txt", "notes", ".txt"},
		{"archive.tar.gz", "archive.tar", ".gz"},
		{"README", "README", ""},
		{".bashrc", ".bashrc", ""},
	}
	for _, tc := range tests {
		stem, ext := splitName(tc.name)
		if stem != tc.stem || ext != tc.ext {
			t.Errorf("splitName(%q) = %q, %q; want %q, %q", tc.name, stem, ext, tc.stem, tc.ext)
		}
	}
}

// assertDirEmpty reports any file left in dir, which after a failed extraction
// should be none.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("a failed drop left %v behind", names)
	}
}

// TestExtractBoundsSkippedEntries pins the second budget.
//
// maxFiles only counts what is written, so an archive made entirely of entries
// this package skips would otherwise stream forever: the reader keeps making
// progress, the connection's idle deadline never fires, and the handler holds
// a concurrency slot for as long as the sender keeps writing.
func TestExtractBoundsSkippedEntries(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for {
			if err := tw.WriteHeader(&tar.Header{
				Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
			}); err != nil {
				return
			}
			if err := tw.Flush(); err != nil {
				return
			}
		}
	}()
	defer pw.Close()

	dir := filepath.Join(t.TempDir(), "drop")
	done := make(chan error, 1)
	go func() {
		_, err := Extract(pr, Options{Dir: dir, MaxFiles: 8})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Extract accepted an endless archive")
		}
		if !strings.Contains(err.Error(), "entries") {
			t.Errorf("err = %v, want it to name the entry limit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Extract did not stop: the entry loop is unbounded")
	}
	assertDirEmpty(t, dir)
}

// TestSafeNameRejectsControlCharacters: names are echoed into the response the
// sender's terminal prints, so a newline forges an extra line and an ANSI
// escape drives the terminal.
func TestSafeNameRejectsControlCharacters(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"evil\nclipd: copied 999 bytes",
		"wipe\x1b[2J.txt",
		"bell\x07.txt",
		"nul\x00byte.txt",
		"carriage\r.txt",
		"tab\t.txt",
		"del\x7f.txt",
	} {
		if got, err := safeName(name); err == nil {
			t.Errorf("safeName(%q) = %q, want an error", name, got)
		}
	}
}

// TestSafeNameKeepsAwkwardButLegitimateNames: refusing control characters must
// not also refuse the merely unusual. A leading dash matters because these
// names reach os.OpenFile, never a shell.
func TestSafeNameKeepsAwkwardButLegitimateNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"-rf", "--help", "file with spaces.txt", "日本語.txt",
		"emoji 🎉.txt", "quote'and\"quote.txt", "semi;colon.txt",
		"$(whoami).txt", "back`tick`.txt",
	} {
		got, err := safeName(name)
		if err != nil {
			t.Errorf("safeName(%q): %v", name, err)
			continue
		}
		if got != name {
			t.Errorf("safeName(%q) = %q, want it unchanged", name, got)
		}
	}
}

// TestExtractWritesAwkwardNames carries the same cases through a real
// extraction, so the guarantee covers the filesystem call and not just the
// validator.
func TestExtractWritesAwkwardNames(t *testing.T) {
	t.Parallel()

	names := []string{"-rf", "file with spaces.txt", "日本語.txt", "$(whoami).txt"}
	var entries []entry
	for _, n := range names {
		entries = append(entries, entry{name: n, body: "ok"})
	}

	dir := filepath.Join(t.TempDir(), "drop")
	res, err := Extract(archiveOf(t, entries...), Options{Dir: dir})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Names) != len(names) {
		t.Fatalf("wrote %v, want %d files", res.Names, len(names))
	}
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("stat %q: %v", n, err)
		}
	}
}

// TestSaveWritesOneNamedStream covers the pipeline case, where the bytes never
// existed as a file and so carry no name of their own.
func TestSaveWritesOneNamedStream(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "drop")
	res, err := Save(strings.NewReader("dumped rows"), "dump.sql", Options{Dir: dir})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(res.Names) != 1 || res.Names[0] != "dump.sql" {
		t.Fatalf("wrote %v, want [dump.sql]", res.Names)
	}
	got, err := os.ReadFile(filepath.Join(dir, "dump.sql"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "dumped rows" {
		t.Errorf("content = %q, want the streamed bytes", got)
	}
}

// TestSaveAppliesTheSameGuaranteesAsExtract: a name arriving over the wire is
// no more trustworthy than one inside an archive, so it goes through exactly
// the same reduction.
func TestSaveAppliesTheSameGuaranteesAsExtract(t *testing.T) {
	t.Parallel()

	t.Run("traversal is flattened", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		dir := filepath.Join(parent, "drop")
		res, err := Save(strings.NewReader("x"), "../../escaped.txt", Options{Dir: dir})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if res.Names[0] != "escaped.txt" {
			t.Errorf("wrote %q, want escaped.txt", res.Names[0])
		}
		if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); err == nil {
			t.Error("Save wrote outside the drop directory")
		}
	})

	t.Run("control characters are refused", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "drop")
		if _, err := Save(strings.NewReader("x"), "evil\nname.txt", Options{Dir: dir}); err == nil {
			t.Error("Save accepted a control character in the name")
		}
	})

	t.Run("size is capped and rolled back", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "drop")
		_, err := Save(strings.NewReader(strings.Repeat("x", 5000)), "big.bin", Options{Dir: dir, MaxBytes: 100})
		if err == nil {
			t.Fatal("Save accepted a stream past the limit")
		}
		assertDirEmpty(t, dir)
	})

	t.Run("existing files are not overwritten", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "drop")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("first"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		res, err := Save(strings.NewReader("second"), "notes.txt", Options{Dir: dir})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if res.Names[0] != "notes-1.txt" {
			t.Errorf("wrote %q, want notes-1.txt", res.Names[0])
		}
	})
}
