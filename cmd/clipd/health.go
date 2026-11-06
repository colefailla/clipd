package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/colefailla/clipd/internal/config"
	"github.com/colefailla/clipd/internal/launchagent"
	"github.com/colefailla/clipd/internal/server"
)

const healthTimeout = 5 * time.Second

var (
	readInstalledAgent = launchagent.ReadInstalled
	restartAgent       = launchagent.Restart
	checkReceiver      = waitForReceiver
)

func waitForReceiver(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	for {
		if err := server.PingContext(ctx, "unix", socket); err == nil {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Resolve in the job's environment, never the invoking shell's CLIPD_CONFIG.
func serviceSocket(spec launchagent.Spec) (string, error) {
	var g globalOptions
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerGlobalFlags(fs, &g)
	if len(spec.ProgramArguments) < 2 {
		return "", errors.New("installed service has no serve command")
	}
	if err := fs.Parse(spec.ProgramArguments[1:]); err != nil {
		return "", err
	}
	args := fs.Args()
	if len(args) == 0 || args[0] != "serve" {
		return "", errors.New("installed service does not run clipd serve")
	}
	address := fs.String("address", "", "")
	fs.String("drop-dir", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return "", err
	}
	working := spec.WorkingDirectory
	if working == "" {
		working = "/"
	}
	if !filepath.IsAbs(working) {
		return "", errors.New("service working directory is not absolute")
	}
	resolve := func(path string) string {
		if !filepath.IsAbs(path) {
			return filepath.Join(working, path)
		}
		return path
	}
	path := g.configPath
	if path == "" {
		path = spec.EnvironmentVariables["CLIPD_CONFIG"]
	}
	if path == "" {
		base := spec.EnvironmentVariables["XDG_CONFIG_HOME"]
		if base == "" {
			home := spec.EnvironmentVariables["HOME"]
			if home == "" {
				u, err := user.Current()
				if err != nil {
					return "", err
				}
				home = u.HomeDir
			}
			base = filepath.Join(home, ".config")
		}
		path = filepath.Join(base, "clipd", "config.json")
	}
	cfg, err := config.Load(resolve(path))
	if err != nil {
		return "", err
	}
	if *address != "" {
		cfg.Address = *address
	}
	if !config.IsSocketPath(cfg.Address) {
		return "", fmt.Errorf("service address %q is not a UNIX socket path", cfg.Address)
	}
	if strings.HasPrefix(cfg.Address, "~") {
		return server.ExpandPath(cfg.Address)
	}
	return resolve(cfg.Address), nil
}

func healthFailure(e *env, ctx context.Context, action, log string, err error) int {
	reason := "the receiver did not respond within 5 seconds"
	if ctx.Err() != nil {
		reason = "the receiver check was canceled: " + ctx.Err().Error()
	} else if !errors.Is(err, context.DeadlineExceeded) {
		reason = "the receiver check failed: " + err.Error()
	}
	note := "Check the log:"
	if action == "service installed" {
		note = "The service remains installed. Check the log:"
	}
	return failf(e, exitFailure, "%s, but %s\n\n%s\n  %s", action, reason, note, log)
}
