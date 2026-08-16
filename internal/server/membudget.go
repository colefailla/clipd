package server

import (
	"context"
	"fmt"
	"sync"
)

// memBudget rations the payload bytes handlers may hold in memory at once.
//
// The connection limit does not bound memory on its own. A payload is buffered
// whole before it reaches the clipboard — it has to be, because a copy that
// fails partway must leave the clipboard untouched rather than truncated — so
// the daemon's ceiling is the connection limit times the payload limit. That is
// a product of two numbers chosen for unrelated reasons, and nothing was
// checking it: raising concurrency for connection-handling reasons silently
// multiplied the memory ceiling by the same factor.
//
// Bounding the product directly lets each limit be set for its own reasons.
//
// Waiters are served strictly in arrival order, and one that does not fit
// blocks those behind it instead of being passed over. Passing over would let a
// stream of small copies starve a large one for as long as the stream lasted.
type memBudget struct {
	mu        sync.Mutex
	capacity  int64
	available int64
	waiters   []*memWaiter
}

// memWaiter is one handler blocked on capacity. ready is closed by whoever
// grants the reservation, which is what transfers ownership of n bytes to this
// waiter: once it is closed, those bytes are charged and must be released.
type memWaiter struct {
	n     int64
	ready chan struct{}
}

func newMemBudget(capacity int64) *memBudget {
	return &memBudget{capacity: capacity, available: capacity}
}

// acquire reserves n bytes, waiting until they are free or ctx ends.
//
// A request larger than the whole budget is refused rather than queued: no
// amount of waiting would satisfy it, and blocking until the deadline would
// report a timeout for what is really a misconfiguration.
func (b *memBudget) acquire(ctx context.Context, n int64) error {
	if n <= 0 {
		return nil
	}
	if n > b.capacity {
		return fmt.Errorf("payload of %d bytes exceeds the server's %d byte memory budget", n, b.capacity)
	}

	b.mu.Lock()
	// Queue behind anyone already waiting even when the bytes are free, or a
	// steady trickle of small requests would overtake a large one forever.
	if len(b.waiters) == 0 && b.available >= n {
		b.available -= n
		b.mu.Unlock()
		return nil
	}
	w := &memWaiter{n: n, ready: make(chan struct{})}
	b.waiters = append(b.waiters, w)
	b.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		b.mu.Lock()
		// A grant racing this cancellation has already charged the bytes to
		// this waiter, so they have to be handed back rather than abandoned.
		select {
		case <-w.ready:
			b.mu.Unlock()
			b.release(n)
			return ctx.Err()
		default:
		}
		b.removeLocked(w)
		b.mu.Unlock()
		return ctx.Err()
	}
}

// release returns n bytes and hands them to whoever is waiting.
func (b *memBudget) release(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.available += n
	if b.available > b.capacity {
		// Unreachable if every acquire is paired with one release, which is
		// why it is worth clamping rather than trusting: a double release
		// would otherwise inflate the budget permanently and silently undo it.
		b.available = b.capacity
	}
	b.grantLocked()
}

// grantLocked wakes waiters from the front for as long as they fit.
func (b *memBudget) grantLocked() {
	for len(b.waiters) > 0 {
		w := b.waiters[0]
		if b.available < w.n {
			return
		}
		b.available -= w.n
		b.waiters = b.waiters[1:]
		close(w.ready)
	}
}

func (b *memBudget) removeLocked(target *memWaiter) {
	for i, w := range b.waiters {
		if w == target {
			b.waiters = append(b.waiters[:i], b.waiters[i+1:]...)
			return
		}
	}
}

// inUse reports the bytes currently reserved, for tests and diagnostics.
func (b *memBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capacity - b.available
}

// waiting reports how many handlers are queued for capacity. Tests use it to
// establish arrival order, which is otherwise unobservable.
func (b *memBudget) waiting() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.waiters)
}
