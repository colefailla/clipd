package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/colefailla/clipd/internal/client"
	"github.com/colefailla/clipd/internal/config"
)

// cmdCopy sends stdin or a file to the remote clipboard.
//
// This is the command the whole utility exists for, and the one that runs
// implicitly when stdin is piped and no subcommand is given.
func cmdCopy(ctx context.Context, e *env, g *globalOptions, args []string) int {
	flags := newFlagSet(e, g, "copy", "Usage: clipd copy [options] [file]")
	maxPayload := flags.String("max-payload", "", "override the maximum payload for this copy, e.g. 20MB")
	if code, ok := flags.parse(args); !ok {
		return code
	}

	files := flags.Args()
	if len(files) > 1 {
		return failf(e, exitUsage, "copy accepts at most one file, got %d", len(files))
	}

	cfg, _, err := loadConfig(e, g)
	if err != nil {
		return fail(e, exitConfig, err)
	}
	if *maxPayload != "" {
		size, err := config.ParseSize(*maxPayload)
		if err != nil {
			return failf(e, exitUsage, "-max-payload: %v", err)
		}
		cfg.MaxPayloadBytes = size
	}
	// The input is resolved before the configuration is validated so that a
	// mistyped subcommand — which lands here as a filename — reports the
	// missing file rather than whatever else happens to be unconfigured.
	payload, err := readPayload(e, files, cfg.MaxPayloadBytes)
	if err != nil {
		return fail(e, exitCodeFor(err), err)
	}

	if err := cfg.ValidateClient(); err != nil {
		return fail(e, exitConfig, err)
	}

	tlsConfig, err := clientTLS(cfg)
	if err != nil {
		return fail(e, exitConfig, err)
	}

	res, err := client.Copy(ctx, client.Options{
		Address: cfg.DialAddress(),
		Token:   cfg.Token,
		TLS:     tlsConfig,
		Timeout: cfg.Timeout(),
	}, payload)
	if err != nil {
		return fail(e, exitCodeFor(err), err)
	}

	// Success is silent by default, like pbcopy. The verbose line goes to
	// stderr so clipd can sit anywhere in a pipeline without altering what
	// the next stage reads.
	if g.verbose {
		fmt.Fprintf(e.stderr, "copied %d bytes to %s\n", res.Bytes, cfg.DialAddress())
	}
	return exitOK
}

// readPayload reads the copy source: a named file, or stdin.
func readPayload(e *env, files []string, max int64) ([]byte, error) {
	if len(files) == 1 && files[0] != "-" {
		name := files[0]
		f, err := os.Open(name)
		if err != nil {
			// A mistyped subcommand lands here, because anything that is not
			// a known command is treated as a filename. Point at the command
			// list so the cause is obvious.
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s: no such file (run 'clipd help' for the list of commands)", name)
			}
			return nil, fmt.Errorf("open %s: %w", name, err)
		}
		defer f.Close()
		return client.ReadInput(f, max)
	}

	if len(files) == 0 && !e.stdinIsPipe {
		return nil, errors.New("no input: pipe data into clipd or pass a file")
	}
	return client.ReadInput(e.stdin, max)
}
