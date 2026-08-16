package main

import (
	"context"
	"fmt"
	"runtime"
)

// cmdVersion prints build information.
//
// version, commit and date are injected with -ldflags at build time; a `go
// build` with no flags reports the "dev" placeholders, which is itself useful
// information when someone is running a binary they built by hand.
func cmdVersion(_ context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "version", "Usage: clipd version")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	fmt.Fprintf(e.stdout, "clipd %s\n", version)
	fmt.Fprintf(e.stdout, "  commit   %s\n", commit)
	fmt.Fprintf(e.stdout, "  built    %s\n", date)
	fmt.Fprintf(e.stdout, "  go       %s\n", runtime.Version())
	fmt.Fprintf(e.stdout, "  platform %s/%s\n", runtime.GOOS, runtime.GOARCH)
	return exitOK
}
