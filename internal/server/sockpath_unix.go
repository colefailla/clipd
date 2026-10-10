//go:build darwin

package server

// maxSocketPath is the longest UNIX socket path darwin accepts: sun_path is
// 104 bytes there, one of which is the terminating NUL.
const maxSocketPath = 103
