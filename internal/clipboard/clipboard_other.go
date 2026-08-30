//go:build !darwin && !linux

package clipboard

// New reports that this platform has no clipboard backend.
//
// macOS and Linux can both receive; everything else is a client only. Failing
// here — rather than silently accepting and discarding a copy — makes a
// misconfigured `clipd serve` obvious.
func New() (Clipboard, error) {
	return nil, ErrUnsupported
}
