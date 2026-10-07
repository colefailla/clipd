// Command clipd is a terminal-independent remote clipboard.
//
// The daemon runs on the Mac and listens on a UNIX domain socket. Forwarding
// that socket over SSH puts it on a remote machine, where anything able to
// write bytes — `nc`, an editor, a shell function — can put text on the Mac's
// clipboard or send it a file. Nothing is installed on the remote side, and
// nothing is asked of the terminal emulator, so the workflow behaves
// identically in Terminal.app, iTerm2, Ghostty, tmux or a VS Code panel.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// Build information, injected with -ldflags. See the Makefile.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Exit codes. Distinct codes let a script branch on $? instead of parsing
// stderr. 64 is sysexits.h's EX_USAGE, kept apart from the operational codes
// so "I typed it wrong" never looks like "the daemon is not running".
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 4
	exitUsage   = 64
)

// env is the process environment the commands run against, injected so the
// dispatcher and commands are testable without touching the real one.
type env struct {
	stdout io.Writer
	stderr io.Writer

	getenv func(string) string
}

// globalOptions are accepted before or after the subcommand.
type globalOptions struct {
	configPath string
	verbose    bool
}

// commandFunc is the signature every subcommand implements.
type commandFunc func(ctx context.Context, e *env, g *globalOptions, args []string) int

// commands is the authoritative list of subcommand names.
var commands = map[string]commandFunc{
	"serve":     cmdServe,
	"setup":     cmdSetup,
	"status":    cmdStatus,
	"install":   cmdInstall,
	"restart":   cmdRestart,
	"uninstall": cmdUninstall,
	"version":   cmdVersion,
	"help":      cmdHelp,
}

func main() {
	// Every file, directory and socket this process creates is owner-only,
	// set once here rather than repaired afterwards with Chmod. A socket in
	// particular has no second chance: between bind and chmod there is a
	// window where a permissive umask has already published it.
	//
	// Taken from wincent/clipper, which opens main the same way.
	restrictUmask()

	e := &env{
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
	}
	os.Exit(run(context.Background(), os.Args[1:], e))
}

// run dispatches a command line and returns a process exit code.
func run(ctx context.Context, args []string, e *env) int {
	var g globalOptions

	fs := flag.NewFlagSet("clipd", flag.ContinueOnError)
	// The flag package prints its own error and calls Usage on every parse
	// failure, including -h. Both are suppressed so help lands once on stdout
	// and errors land once on stderr, instead of usage appearing twice on a
	// terminal where the two streams interleave.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	registerGlobalFlags(fs, &g)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(e.stdout)
			return exitOK
		}
		fmt.Fprintf(e.stderr, "clipd: %v\n\n", err)
		printUsage(e.stderr)
		return exitUsage
	}

	rest := fs.Args()
	if len(rest) == 0 {
		printUsage(e.stderr)
		return exitUsage
	}
	cmd, ok := commands[rest[0]]
	if !ok {
		fmt.Fprintf(e.stderr, "clipd: unknown command %q\n\n", rest[0])
		printUsage(e.stderr)
		return exitUsage
	}
	return cmd(ctx, e, &g, rest[1:])
}

// registerGlobalFlags attaches the global flags to fs.
//
// Each flag's default is g's current value, so registering the same flags on
// a subcommand's flag set preserves anything already parsed from before the
// subcommand. That is what makes `clipd -v serve` and `clipd serve -v`
// equivalent.
func registerGlobalFlags(fs *flag.FlagSet, g *globalOptions) {
	fs.StringVar(&g.configPath, "config", g.configPath, "path to the clipd config file")
	fs.BoolVar(&g.verbose, "verbose", g.verbose, "report progress on stderr")
	fs.BoolVar(&g.verbose, "v", g.verbose, "shorthand for -verbose")
}

// cmdFlags is a subcommand's flag set together with its usage text.
//
// The flag package prints usage to one fixed destination, but the same text
// serves two purposes: an answer when the user typed -h, and a complaint when
// they typed something wrong. Keeping the text here lets parse route it to
// stdout or stderr accordingly.
type cmdFlags struct {
	*flag.FlagSet
	usage string
	e     *env
}

// newFlagSet builds a subcommand flag set that also accepts the global flags.
func newFlagSet(e *env, g *globalOptions, name, usage string) *cmdFlags {
	fs := flag.NewFlagSet("clipd "+name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	registerGlobalFlags(fs, g)
	fs.Usage = func() {}
	return &cmdFlags{FlagSet: fs, usage: usage, e: e}
}

// parse handles a subcommand's arguments, translating flag package outcomes
// into exit codes. ok is false when the caller should return code.
func (c *cmdFlags) parse(args []string) (code int, ok bool) {
	switch err := c.FlagSet.Parse(args); {
	case err == nil:
		return exitOK, true
	case errors.Is(err, flag.ErrHelp):
		// -h is a request, and its output is what the user asked for, so it
		// goes to stdout where it can be piped into a pager.
		c.printUsage(c.e.stdout)
		return exitOK, false
	default:
		c.printUsage(c.e.stderr)
		return exitUsage, false
	}
}

func (c *cmdFlags) printUsage(w io.Writer) {
	fmt.Fprintf(w, "%s\n\nOptions:\n", c.usage)
	c.FlagSet.SetOutput(w)
	defer c.FlagSet.SetOutput(c.e.stderr)
	c.FlagSet.PrintDefaults()
}

// fail prints an error to stderr in the conventional `clipd: message` form
// and returns the exit code.
func fail(e *env, code int, err error) int {
	fmt.Fprintf(e.stderr, "clipd: %v\n", err)
	return code
}

// failf is fail for messages built on the spot.
func failf(e *env, code int, format string, args ...any) int {
	return fail(e, code, fmt.Errorf(format, args...))
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `clipd — send text to your Mac's clipboard and files to its Drop directory over SSH

The daemon runs on the Mac. 'clipd setup <host>' edits your SSH config and
remote shell startup file; it installs no remote binary or service:

  ls -l | clipd            copy output to the Mac's clipboard
  clipd notes.txt          copy a file's contents
  clipd drop report.pdf    send files to the Mac's ~/Drop

Commands:
  serve       run the daemon in the foreground (macOS)
  setup       configure a remote host to talk to this daemon
  status      show the daemon's configuration and state
  install     macOS: install and start the LaunchAgent
  restart     macOS: reload the daemon after editing the config
  uninstall   macOS: stop and remove the LaunchAgent
  version     print build information
  help        show help for a command

Global options:
  -config <path>     use an alternate config file
  -verbose, -v       enable daemon diagnostic logs

Environment:
  CLIPD_CONFIG       path to the config file

Config: ~/.config/clipd/config.json (or $XDG_CONFIG_HOME/clipd/config.json).
After editing settings, run 'clipd restart'. 'clipd install' writes all defaults.
Run 'clipd help setup' for files setup edits, 'clipd help config' for limits, or
'clipd help security' for what protects the socket.
`)
}

// cmdHelp prints general or per-command help.
func cmdHelp(_ context.Context, e *env, _ *globalOptions, args []string) int {
	if len(args) == 0 {
		printUsage(e.stdout)
		return exitOK
	}
	text, ok := helpTopics[args[0]]
	if !ok {
		fmt.Fprintf(e.stderr, "clipd: no help for %q\n", args[0])
		return exitUsage
	}
	fmt.Fprintln(e.stdout, text)
	return exitOK
}
