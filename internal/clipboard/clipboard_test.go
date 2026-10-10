package clipboard

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestFakeStoresBytesVerbatim(t *testing.T) {
	t.Parallel()

	var f Fake
	payload := []byte("line one\nline two\t\x00\xff\n")
	if err := f.Write(context.Background(), payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := f.Data(); !bytes.Equal(got, payload) {
		t.Errorf("Data = %q, want %q", got, payload)
	}
	if f.WriteCount() != 1 {
		t.Errorf("Writes = %d, want 1", f.WriteCount())
	}
}

// TestFakeCopiesInput guards the test double itself: a fake that aliased the
// caller's buffer could make a broken server look correct.
func TestFakeCopiesInput(t *testing.T) {
	t.Parallel()

	var f Fake
	payload := []byte("original")
	if err := f.Write(context.Background(), payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	copy(payload, "MANGLED!")

	if got := string(f.Data()); got != "original" {
		t.Errorf("Data = %q, want the bytes as they were at Write time", got)
	}
}

func TestFakeReturnsConfiguredError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("pasteboard unavailable")
	f := Fake{Err: sentinel}
	if err := f.Write(context.Background(), []byte("x")); !errors.Is(err, sentinel) {
		t.Errorf("Write = %v, want %v", err, sentinel)
	}
	if f.Data() != nil && len(f.Data()) != 0 {
		t.Error("a failed write stored data")
	}
}

func TestFakeHonoursCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var f Fake
	if err := f.Write(ctx, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Write with a cancelled context = %v, want context.Canceled", err)
	}
}

// TestFakeImplementsClipboard fails at compile time if the interface drifts.
var _ Clipboard = (*Fake)(nil)
