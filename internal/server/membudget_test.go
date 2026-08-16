package server

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// waitForWaiters blocks until n handlers are queued, so a test can establish
// arrival order before releasing anything.
func waitForWaiters(t *testing.T, b *memBudget, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.waiting() == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d queued waiters, have %d", n, b.waiting())
}

func TestMemBudgetAccounting(t *testing.T) {
	t.Parallel()

	b := newMemBudget(100)
	if got := b.inUse(); got != 0 {
		t.Fatalf("fresh budget in use = %d, want 0", got)
	}

	if err := b.acquire(context.Background(), 40); err != nil {
		t.Fatalf("acquire 40: %v", err)
	}
	if got := b.inUse(); got != 40 {
		t.Errorf("in use = %d, want 40", got)
	}

	// Zero-sized copies are legal — they clear the clipboard — and must not
	// consume budget or queue behind anything.
	if err := b.acquire(context.Background(), 0); err != nil {
		t.Errorf("acquire 0: %v", err)
	}
	if got := b.inUse(); got != 40 {
		t.Errorf("after a zero acquire, in use = %d, want 40", got)
	}

	b.release(40)
	if got := b.inUse(); got != 0 {
		t.Errorf("after release, in use = %d, want 0", got)
	}
}

func TestMemBudgetRefusesWhatItCanNeverFit(t *testing.T) {
	t.Parallel()

	b := newMemBudget(100)
	// Queuing this would block until the deadline and then report a timeout,
	// pointing at load rather than at the configuration that is actually wrong.
	err := b.acquire(context.Background(), 101)
	if err == nil {
		t.Fatal("acquiring more than the whole budget succeeded")
	}
	if b.waiting() != 0 {
		t.Errorf("an impossible request was queued: %d waiters", b.waiting())
	}
	if got := b.inUse(); got != 0 {
		t.Errorf("in use = %d after a refused acquire, want 0", got)
	}
}

// TestMemBudgetServesInArrivalOrder is the starvation guard: a large waiter
// must hold the queue rather than being passed over by later small ones that
// happen to fit.
func TestMemBudgetServesInArrivalOrder(t *testing.T) {
	t.Parallel()

	b := newMemBudget(100)
	if err := b.acquire(context.Background(), 80); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	// Wants 50, only 20 free: queues.
	bigDone := make(chan error, 1)
	go func() { bigDone <- b.acquire(context.Background(), 50) }()
	waitForWaiters(t, b, 1)

	// Wants 10, which would fit right now — but it arrived second, so it must
	// wait behind the larger request instead of overtaking it.
	smallDone := make(chan error, 1)
	go func() { smallDone <- b.acquire(context.Background(), 10) }()
	waitForWaiters(t, b, 2)

	select {
	case <-smallDone:
		t.Fatal("a later small request overtook an earlier large one")
	case <-time.After(100 * time.Millisecond):
	}

	b.release(80)

	for _, c := range []struct {
		name string
		ch   chan error
	}{{"large", bigDone}, {"small", smallDone}} {
		select {
		case err := <-c.ch:
			if err != nil {
				t.Errorf("%s waiter: %v", c.name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s waiter was never granted", c.name)
		}
	}

	if got := b.inUse(); got != 60 {
		t.Errorf("in use = %d, want 60 (50 + 10)", got)
	}
}

func TestMemBudgetCancellationReleasesTheQueue(t *testing.T) {
	t.Parallel()

	b := newMemBudget(100)
	if err := b.acquire(context.Background(), 100); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.acquire(ctx, 50) }()
	waitForWaiters(t, b, 1)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled acquire never returned")
	}

	// The abandoned waiter must leave no trace: still queued, it would be
	// handed bytes on the next release that nobody would ever give back.
	if got := b.waiting(); got != 0 {
		t.Errorf("%d waiters remain after cancellation, want 0", got)
	}
	b.release(100)
	if got := b.inUse(); got != 0 {
		t.Errorf("in use = %d after everything drained, want 0", got)
	}
}

// TestMemBudgetDoesNotLeakOnGrantCancelRace covers the window where a release
// grants a reservation at the same moment its waiter gives up: the bytes have
// already been charged, so the loser of that race must hand them back rather
// than walk away from them.
func TestMemBudgetDoesNotLeakOnGrantCancelRace(t *testing.T) {
	t.Parallel()

	for i := range 400 {
		b := newMemBudget(100)
		if err := b.acquire(context.Background(), 100); err != nil {
			t.Fatalf("initial acquire: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- b.acquire(ctx, 100) }()
		waitForWaiters(t, b, 1)

		// Fire the release and the cancellation together and let the race
		// resolve whichever way it does; both outcomes are legal, and both
		// must balance the books.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); b.release(100) }()
		go func() { defer wg.Done(); cancel() }()
		wg.Wait()

		if err := <-done; err == nil {
			// Granted despite the cancellation: this handler owns the bytes.
			b.release(100)
		}
		cancel()

		if got := b.inUse(); got != 0 {
			t.Fatalf("iteration %d leaked %d bytes", i, got)
		}
		if got := b.waiting(); got != 0 {
			t.Fatalf("iteration %d left %d waiters queued", i, got)
		}
	}
}

// TestMemBudgetUnderConcurrentLoad is the property that matters in production:
// however the requests interleave, the budget is never overdrawn and always
// returns to zero.
func TestMemBudgetUnderConcurrentLoad(t *testing.T) {
	t.Parallel()

	const capacity = 1000
	b := newMemBudget(capacity)

	var (
		mu      sync.Mutex
		held    int64
		peak    int64
		workers = 64
	)

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(i)))
			for range 50 {
				n := int64(rng.Intn(capacity)) + 1
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := b.acquire(ctx, n)
				cancel()
				if err != nil {
					continue
				}

				mu.Lock()
				held += n
				if held > peak {
					peak = held
				}
				over := held > capacity
				mu.Unlock()
				if over {
					t.Errorf("budget overdrawn: %d bytes held against a capacity of %d", held, capacity)
				}

				mu.Lock()
				held -= n
				mu.Unlock()
				b.release(n)
			}
		}()
	}
	wg.Wait()

	if got := b.inUse(); got != 0 {
		t.Errorf("in use = %d after all work finished, want 0", got)
	}
	if got := b.waiting(); got != 0 {
		t.Errorf("%d waiters left queued", got)
	}
	if peak == 0 {
		t.Error("no reservation was ever held; the test did not exercise the budget")
	}
}
