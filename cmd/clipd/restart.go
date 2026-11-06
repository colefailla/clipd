package main

import (
	"context"
	"fmt"
	"runtime"

	"github.com/colefailla/clipd/internal/launchagent"
)

// cmdRestart restarts the installed service and checks its configured socket.
func cmdRestart(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "restart", "Usage: clipd restart")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	if runtime.GOOS != "darwin" {
		return fail(e, exitFailure, launchagent.ErrUnsupported)
	}

	spec, err := readInstalledAgent(ctx)
	if err != nil {
		return fail(e, exitFailure, err)
	}
	socket, err := serviceSocket(spec)
	if err != nil {
		return failf(e, exitFailure, "cannot check the installed service: %v", err)
	}
	if err := restartAgent(ctx); err != nil {
		return fail(e, exitFailure, err)
	}
	if err := checkReceiver(ctx, socket); err != nil {
		return healthFailure(e, ctx, "restart requested", spec.StandardErrPath, err)
	}
	fmt.Fprintf(e.stdout, "Restarted the macOS login service. clipd is responding.\n")
	return exitOK
}
