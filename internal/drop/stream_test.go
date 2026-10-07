package drop

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func streamOf(t *testing.T, entries ...entry) io.Reader {
	return io.MultiReader(archiveOf(t, entries...), strings.NewReader(Completion))
}

func TestStreamPreservesDirectoriesAndRenamesWholeTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "book"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "book", "old"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := Extract(streamOf(t, entry{name: "./book/chapter/a.mp3", body: "audio"}, entry{name: "./book/info.cue", body: "cue"}), Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Names) != 1 || res.Names[0] != "book-1" || res.Bytes != 8 {
		t.Fatalf("result %+v", res)
	}
	for name, want := range map[string]string{"book/old": "old", "book-1/chapter/a.mp3": "audio", "book-1/info.cue": "cue"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
}

func TestStreamRefusesIncompleteArchivesBeforePublication(t *testing.T) {
	complete, err := io.ReadAll(archiveOf(t, entry{name: "a.txt", body: "first"}, entry{name: "b.txt", body: "second"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, length := range []int{0, 512, 1024, len(complete) - 1024, len(complete)} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			dir := t.TempDir()
			if _, err := Extract(bytes.NewReader(complete[:length]), Options{Dir: dir}); err == nil {
				t.Fatal("accepted missing trailer")
			}
			assertDirEmpty(t, dir)
		})
	}
	for _, suffix := range []string{Completion + "extra", "clipd:complete:v", "not-complete\n"} {
		dir := t.TempDir()
		if _, err := Extract(io.MultiReader(bytes.NewReader(complete), strings.NewReader(suffix)), Options{Dir: dir}); err == nil {
			t.Fatal("accepted invalid trailer")
		}
		assertDirEmpty(t, dir)
	}
}

func TestStreamRejectsHostileAndConflictingPaths(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", "a/../outside", `a\outside`, "a/./b", "a\n/b", strings.Repeat("x/", 33) + "b", stageDirPrefix + "user/file"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := Extract(streamOf(t, entry{name: name, body: "x"}), Options{Dir: dir}); err == nil {
				t.Fatal("accepted unsafe path")
			}
			assertDirEmpty(t, dir)
		})
	}
	for _, entries := range [][]entry{{{name: "a", body: "x"}, {name: "a", body: "y"}}, {{name: "a/b", body: "x"}, {name: "a", body: "y"}}, {{name: "a", body: "x"}, {name: "a/b", body: "y"}}} {
		dir := t.TempDir()
		if _, err := Extract(streamOf(t, entries...), Options{Dir: dir}); err == nil {
			t.Fatal("accepted conflicting paths")
		}
		assertDirEmpty(t, dir)
	}
}

func TestStreamSizeLimitsAndCancelledPublication(t *testing.T) {
	dir := t.TempDir()
	if _, err := Extract(streamOf(t, entry{name: "book/a", body: "123"}, entry{name: "book/b", body: "456"}), Options{Dir: dir, MaxBytes: 5}); err == nil {
		t.Fatal("accepted oversized stream")
	}
	assertDirEmpty(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Extract(streamOf(t, entry{name: "book/a", body: "123"}), Options{Dir: dir, Context: ctx}); err == nil {
		t.Fatal("accepted cancelled stream")
	}
	assertDirEmpty(t, dir)
}

func TestLongTransfersAreNotCleanedAsStale(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	txn, err := beginTransaction(root)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * time.Minute)
	if err := os.Chtimes(filepath.Join(dir, txn.dir), old, old); err != nil {
		t.Fatal(err)
	}
	if n := CleanStale(dir, 2*time.Hour); n != 0 {
		t.Fatal("removed staging within configured lifetime")
	}
	if n := CleanStale(dir, time.Hour); n != 1 {
		t.Fatal("did not remove genuinely stale staging")
	}
}

func TestStreamPublicationFailureRollsBackFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxCollisionAttempts; i++ {
		name := "taken.txt"
		if i > 0 {
			name = fmt.Sprintf("taken-%d.txt", i)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("existing"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Extract(streamOf(t, entry{name: "book/a", body: "first"}, entry{name: "taken.txt", body: "second"}), Options{Dir: dir})
	if err == nil {
		t.Fatal("accepted exhausted collision names")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != maxCollisionAttempts {
		t.Fatalf("publication failure left %d entries", len(entries))
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || string(data) != "existing" {
			t.Fatal("changed preexisting file")
		}
	}
}

type replaceAtEOF struct {
	r       io.Reader
	replace func()
	done    bool
}

func (r *replaceAtEOF) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err == io.EOF && !r.done {
		r.done = true
		r.replace()
	}
	return n, err
}

func TestStreamRefusesReplacedRootBeforePathPublication(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "drop")
	moved := filepath.Join(parent, "old-drop")
	foreign := filepath.Join(parent, "foreign")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	reader := &replaceAtEOF{r: streamOf(t, entry{name: "book/a", body: "payload"}), replace: func() {
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(foreign, dir); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := Extract(reader, Options{Dir: dir}); err == nil {
		t.Fatal("published after drop root replacement")
	}
	assertDirEmpty(t, foreign)
	assertDirEmpty(t, moved)
}

type zeroStream struct{}

func (zeroStream) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestLargeDropStreamsWithBoundedAllocations(t *testing.T) {
	const size = int64(300 << 20) // Larger than the unchanged default drop limit.
	dir := t.TempDir()
	reader, writer := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(writer)
		err := tw.WriteHeader(&tar.Header{Name: "media.mp4", Mode: 0600, Size: size})
		if err == nil {
			_, err = io.CopyN(tw, zeroStream{}, size)
		}
		if err == nil {
			err = tw.Close()
		}
		if err == nil {
			_, err = io.WriteString(writer, Completion)
		}
		_ = writer.CloseWithError(err)
		produced <- err
	}()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res, err := Extract(reader, Options{Dir: dir, MaxBytes: 512 << 20})
	_ = reader.Close()
	producerErr := <-produced
	if err != nil || producerErr != nil {
		t.Fatalf("large transfer: %v, producer %v", err, producerErr)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("300 MiB drop allocated %d Go heap bytes", allocated)
	}
	info, err := os.Stat(filepath.Join(dir, "media.mp4"))
	if err != nil || info.Size() != size || res.Bytes != size {
		t.Fatalf("large file: %v, %v, result %+v", info, err, res)
	}
}

// Special entries are skipped and counted, as rsync does without -l, rather
// than refusing a folder that happens to contain a symlink. None of them may
// create anything on the Mac.
func TestStreamSkipsSpecialEntries(t *testing.T) {
	dir := t.TempDir()
	res, err := Extract(streamOf(t,
		entry{name: "./project/", typeflag: tar.TypeDir},
		entry{name: "./project/main.go", body: "package main"},
		entry{name: "./project/escape", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		entry{name: "./project/up", typeflag: tar.TypeSymlink, linkname: "../../outside"},
		entry{name: "./project/copy.go", typeflag: tar.TypeLink, linkname: "./project/main.go"},
		entry{name: "./project/pipe", typeflag: tar.TypeFifo},
		entry{name: "./project/tty", typeflag: tar.TypeChar},
	), Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 5 || len(res.Names) != 1 || res.Names[0] != "project" {
		t.Fatalf("result %+v", res)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "project", "main.go")); err != nil || string(got) != "package main" {
		t.Fatalf("main.go = %q, %v", got, err)
	}
	for _, name := range []string{"escape", "up", "copy.go", "pipe", "tty"} {
		if _, err := os.Lstat(filepath.Join(dir, "project", name)); !os.IsNotExist(err) {
			t.Fatalf("special entry %s was created: %v", name, err)
		}
	}
}

// A skipped entry's body would be discarded without counting against
// MaxBytes, so one that declares a body is refused.
func TestStreamRefusesSpecialEntryWithPayload(t *testing.T) {
	// archive/tar will not write a body for a symlink, so write a regular
	// file and patch its header into one.
	raw, err := io.ReadAll(archiveOf(t, entry{name: "link", body: "data"}))
	if err != nil {
		t.Fatal(err)
	}
	raw[156] = tar.TypeSymlink
	copy(raw[148:156], "        ")
	sum := 0
	for _, b := range raw[:512] {
		sum += int(b)
	}
	copy(raw[148:156], fmt.Sprintf("%06o\x00 ", sum))
	buf := bytes.NewReader(raw)
	dir := t.TempDir()
	_, err = Extract(io.MultiReader(buf, strings.NewReader(Completion)), Options{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("special entry with a payload: %v", err)
	}
	assertDirEmpty(t, dir)
}
