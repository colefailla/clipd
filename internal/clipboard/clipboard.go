// Package clipboard abstracts the host clipboard.
//
// The server depends only on the Clipboard interface, which keeps it
// portable, makes it testable without touching the developer's real
// clipboard, and leaves room for a Linux or Windows receiver later without
// disturbing the network layer.
package clipboard

import (
	"context"
	"errors"
	"sync"
)

// ErrUnsupported is returned by New on platforms with no implementation.
var ErrUnsupported = errors.New("clipboard: no implementation for this platform")

// Clipboard writes bytes to the host clipboard.
//
// Write takes a context so the server can bound a clipboard helper that
// hangs (a wedged pbcopy would otherwise pin a connection until its deadline
// expires). Implementations must write the supplied bytes verbatim: no
// trimming, no newline fixups, no encoding conversion.
type Clipboard interface {
	Write(ctx context.Context, data []byte) error

	// Name identifies the backend for status output and logs.
	Name() string
}

// Fake is an in-memory Clipboard for tests.
//
// It lives in the production package rather than a _test file so that both
// the clipboard tests and the server tests can use it.
type Fake struct {
	mu   sync.Mutex
	data []byte
	// writes counts successful and failed calls alike. It is read through
	// WriteCount so tests on other goroutines get a synchronised view.
	writes int
	// Err, when set, is returned by Write instead of storing the data. It
	// lets tests exercise the server's internal-error path.
	Err error
}

// Write records data. It copies the input so a caller reusing its buffer
// cannot retroactively change what the test observes.
func (f *Fake) Write(ctx context.Context, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.Err != nil {
		return f.Err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.data = append([]byte(nil), data...)
	return nil
}

// Name implements Clipboard.
func (f *Fake) Name() string { return "fake" }

// Data returns a copy of the most recently written bytes.
func (f *Fake) Data() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data...)
}

// WriteCount reports how many times Write has been called. Server handlers
// call Write on their own goroutines, so tests must read the count through
// the same mutex those writes hold — a bare field read would be a data race.
func (f *Fake) WriteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}
