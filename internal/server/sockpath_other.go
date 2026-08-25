//go:build !darwin

package server

// maxSocketPath is the longest UNIX socket path Linux and the BSDs accept:
// sun_path is 108 bytes, one of which is the terminating NUL. The value is
// harmless on platforms with no UNIX sockets at all, where Listen fails for
// its own reasons before this is consulted.
const maxSocketPath = 107
