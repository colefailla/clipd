package main

import (
	"context"
	"fmt"
	"runtime"

	"github.com/colefailla/clipd/internal/launchagent"
)

// cmdRestart stops and starts the LaunchAgent.
//
// It exists because the config is read once, at startup. Editing the file does
// nothing until the daemon reloads, and the incantation for that —
// `launchctl kickstart -k gui/$(id -u)/com.clipd.agent` — is not something
// anyone would arrive at unaided. Without a command for it, "change a limit"
// is a two-step operation whose second step is undiscoverable.
func cmdRestart(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "restart", "Usage: clipd restart")
	if code, ok := flags.parse(args); !ok {
		return code
	}
	if runtime.GOOS != "darwin" {
		return fail(e, exitFailure, launchagent.ErrUnsupported)
	}

	if err := launchagent.Restart(ctx); err != nil {
		return fail(e, exitFailure, err)
	}
	fmt.Fprintf(e.stdout, "Restarted the clipd daemon.\n")
	return exitOK
}
