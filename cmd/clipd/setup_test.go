package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/protocol"
)

// fakeTransport uses one command for both transfers and the stale-socket check.
func fakeTransport(command string) transport {
	return transport{send: command, probe: command}
}

func TestCapWriterReportsDiscardedBytesAsAccepted(t *testing.T) {
	t.Parallel()

	w := &capWriter{max: 3}
	if n, err := w.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("first Write = %d, %v; want 6, nil", n, err)
	}
	if n, err := w.Write([]byte("gh")); err != nil || n != 2 {
		t.Fatalf("second Write = %d, %v; want 2, nil", n, err)
	}
	if got := w.String(); got != "abc" {
		t.Fatalf("buffered output = %q, want %q", got, "abc")
	}
}

func TestClientCommandPicksWhatTheHostHas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		facts  remoteFacts
		want   string
		probe  string
		silent bool
	}{
		{"linux, modern netcat", remoteFacts{os: "Linux", hasNC: true, ncUnix: true, ncShutdown: true}, "nc -N -U -w 86400", "nc -N -U -w 5", false},
		{"socat fallback", remoteFacts{os: "Linux", hasSocat: true}, "socat -T 86400 -t 86400", "socat -T 5 -t 5", false},
		{"traditional netcat with socat", remoteFacts{os: "Linux", hasNC: true, hasSocat: true}, "socat -T 86400", "socat -T 5", false},
		// macOS netcat has -N, but there it takes a probe count for a write
		// timeout rather than half-closing. Passing it OpenBSD-style fails
		// outright, and this netcat needs no flag to close on stdin EOF.
		{"macos remote", remoteFacts{os: "Darwin", hasNC: true, ncUnix: true, ncShutdown: true}, "nc -U -w 86400", "nc -U -w 5", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := clientCommand(tc.facts)
			if err != nil {
				t.Fatalf("clientCommand: %v", err)
			}
			if !strings.HasPrefix(got.send, tc.want) || !strings.HasPrefix(got.probe, tc.probe) || got.silentRefusal != tc.silent {
				t.Errorf("clientCommand = %+v, want send %q, probe %q, silentRefusal %v", got, tc.want, tc.probe, tc.silent)
			}
		})
	}
}

// TestClientCommandNamesTheFix is the whole point of probing: when the host
// cannot do it, the message has to say which package to install rather than
// leaving the user to decode "invalid option -- 'U'".
func TestClientCommandNamesTheFix(t *testing.T) {
	t.Parallel()

	tests := map[string]remoteFacts{
		"netcat without -U":             {os: "Linux", hasNC: true},
		"nothing at all":                {os: "Linux"},
		"netcat that cannot half-close": {os: "Linux", hasNC: true, ncUnix: true},
	}
	for name, facts := range tests {
		_, err := clientCommand(facts)
		if err == nil {
			t.Errorf("%s: clientCommand succeeded, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "netcat-openbsd") && !strings.Contains(err.Error(), "socat") {
			t.Errorf("%s: err = %v, want it to name something that fixes it", name, err)
		}
	}
}

func TestRCFileForShell(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"/bin/zsh":      "$HOME/.zshrc",
		"/usr/bin/bash": "$HOME/.bashrc",
		"/bin/sh":       "$HOME/.profile",
		"/usr/bin/fish": "$HOME/.profile",
		"":              "$HOME/.profile",
	}
	for shell, want := range tests {
		if got := rcFileFor(remoteFacts{shell: shell}); got != want {
			t.Errorf("rcFileFor(%q) = %q, want %q", shell, got, want)
		}
	}
}

// TestShellFunctionIsValidShell runs the generated function through `sh -n`.
//
// It is generated text that will be pasted into someone's .bashrc, where a
// syntax error does not just break clipd — it breaks every new shell on that
// host, which is a genuinely bad afternoon for whoever hits it.
func TestShellFunctionIsValidShell(t *testing.T) {
	t.Parallel()

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}

	for _, facts := range []remoteFacts{
		{os: "Linux", hasNC: true, ncUnix: true, ncShutdown: true},
		{os: "Darwin", hasNC: true, ncUnix: true},
		{os: "Linux", hasSocat: true},
	} {
		client, err := clientCommand(facts)
		if err != nil {
			t.Fatal(err)
		}
		for _, hasTar := range []bool{true, false} {
			block := shellFunction(client, "/home/cole/.clipd/socket", hasTar)

			cmd := exec.Command(sh, "-n")
			cmd.Stdin = strings.NewReader(block)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("generated shell is invalid (client=%+v tar=%v): %v\n%s\n%s",
					client, hasTar, err, out, block)
			}
		}
	}
}

func TestShellFunctionQuotesSocketAsLiteralShellData(t *testing.T) {
	t.Parallel()

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}

	socket := "/home/$USER/`printf injected`/it's a \"socket\""
	block := shellFunction(fakeTransport(`nc -N -U "$_clipd_sock"`), socket, true)
	var assignment string
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "_clipd_sock=") {
			assignment = strings.TrimSpace(line)
			break
		}
	}
	if assignment == "" {
		t.Fatal("generated function has no socket assignment")
	}

	cmd := exec.Command(sh, "-c", assignment+`; printf '%s' "$_clipd_sock"`)
	cmd.Env = append(os.Environ(), "USER=expanded-by-shell")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("evaluate socket assignment: %v", err)
	}
	if got := string(out); got != socket {
		t.Fatalf("socket assignment expanded shell syntax: got %q, want %q\n%s", got, socket, assignment)
	}
}

// TestShellFunctionSendsTheRightFrame pins the two shapes the daemon parses:
// a bare stream for the clipboard, and magic plus envelope plus tar for a drop.
func TestShellFunctionSendsTheRightFrame(t *testing.T) {
	t.Parallel()

	block := shellFunction(fakeTransport(`nc -N -U "$_clipd_sock"`), "/home/cole/.clipd/socket", true)

	if !strings.Contains(block, `clipd:magic:v1\n{"type":"drop-stream-v2","progress":%s}\n`) {
		t.Errorf("the drop path does not emit the magic frame:\n%s", block)
	}
	// The -- is load-bearing: without it a file named
	// "--use-compress-program=curl" is read by tar as an option and executed.
	if !strings.Contains(block, `COPYFILE_DISABLE=1 command tar cf - "$@"`) {
		t.Errorf("the drop path does not pipe a tar stream with -- :\n%s", block)
	}
	// The socket check has to come first: without it, a session opened before
	// setup ran fails with a netcat error instead of an explanation.
	if !strings.Contains(block, `if [ ! -S "$_clipd_sock" ]`) {
		t.Errorf("the function does not check the socket exists:\n%s", block)
	}
	if !strings.HasPrefix(block, blockStart) || !strings.HasSuffix(strings.TrimSpace(block), blockEnd) {
		t.Errorf("the block is not bracketed by markers:\n%s", block)
	}
}

func TestShellFunctionRefusesDropWithoutTar(t *testing.T) {
	t.Parallel()

	block := shellFunction(fakeTransport(`nc -N -U "$_clipd_sock"`), "/home/cole/.clipd/socket", false)
	if strings.Contains(block, "tar cf") {
		t.Errorf("the function uses tar on a host that has none:\n%s", block)
	}
	if !strings.Contains(block, "no tar") {
		t.Errorf("the function does not explain why drop is unavailable:\n%s", block)
	}
}

// TestShellFunctionNeedsNoMktemp pins the dependency budget. clipd asks a remote
// host for a POSIX shell, tar, and one of nc or socat. mktemp is not in POSIX,
// so staging the archive through it made file drops unavailable on any host
// without it — for a file the shell can perfectly well create itself.
func TestShellFunctionNeedsNoMktemp(t *testing.T) {
	t.Parallel()

	block := shellFunction(fakeTransport(`nc -N -U "$_clipd_sock"`), "/home/cole/.clipd/socket", true)
	if strings.Contains(block, "mktemp") {
		t.Errorf("the function still depends on mktemp:\n%s", block)
	}
	// The three pieces that make a hand-made staging name safe: 0600 through
	// umask, noclobber so an existing file is refused rather than followed, and
	// clipd's own private directory rather than a shared /tmp.
	for _, want := range []string{`"./$_clipd_base"`, "clipd:complete:v2"} {
		if !strings.Contains(block, want) {
			t.Errorf("staging is missing %q:\n%s", want, block)
		}
	}
}

func TestShellFunctionValidatesAndEscapesNamedDrops(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}
	dir, err := os.MkdirTemp("", "clipd-name-")
	if err != nil {
		t.Fatalf("create short test directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("create test socket: %v", err)
	}
	defer listener.Close()

	called := filepath.Join(dir, "called")
	capture := filepath.Join(dir, "request")
	clientPath := filepath.Join(dir, "client.sh")
	clientScript := "#!/bin/sh\n" +
		`: > "$CLIPD_TEST_CALLED" || exit 1` + "\n" +
		`cat > "$CLIPD_TEST_CAPTURE" || exit 1` + "\n" +
		`printf 'clipd: ok: dropped\n'` + "\n"
	if err := os.WriteFile(clientPath, []byte(clientScript), 0o600); err != nil {
		t.Fatalf("write fake client: %v", err)
	}
	block := shellFunction(fakeTransport("sh "+shellQuote(clientPath)), socket, true)

	for _, tc := range []struct {
		name string
		args string
	}{
		{name: "missing filename", args: "--name"},
		{name: "extra argument", args: "--name safe.txt ignored.txt"},
		{name: "control character", args: "--name " + shellQuote("bad\nname.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(called)
			cmd := exec.Command(sh, "-c", block+"\nprintf payload | clipd drop "+tc.args)
			cmd.Env = append(os.Environ(), "CLIPD_TEST_CALLED="+called, "CLIPD_TEST_CAPTURE="+capture)
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitUsage {
				t.Fatalf("exit = %v, output %q; want %d", err, out, exitUsage)
			}
			if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("client ran for invalid arguments: %v", err)
			}
		})
	}

	name := `quote"and\backslash.txt`
	cmd := exec.Command(sh, "-c", block+"\nprintf payload | clipd drop --name "+shellQuote(name))
	cmd.Env = append(os.Environ(), "CLIPD_TEST_CALLED="+called, "CLIPD_TEST_CAPTURE="+capture)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("valid named drop failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}
	r := bufio.NewReader(bytes.NewReader(data))
	structured, err := protocol.Sniff(r)
	if err != nil || !structured {
		t.Fatalf("Sniff = %v, %v; want a structured request", structured, err)
	}
	req, err := protocol.ReadRequest(r)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Name != name {
		t.Fatalf("decoded name = %q, want %q", req.Name, name)
	}
	body, err := io.ReadAll(r)
	if err != nil || string(body) != "payload" {
		t.Fatalf("body = %q, %v; want payload", body, err)
	}
}

func TestShellFunctionDoesNotChangeInteractiveVariables(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}
	dir, err := os.MkdirTemp("", "clipd-vars-")
	if err != nil {
		t.Fatalf("create short test directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("create test socket: %v", err)
	}
	defer listener.Close()

	block := shellFunction(fakeTransport(`printf 'clipd: ok: copied\n'`), socket, true)
	script := `_clipd_sock=mine; _clipd_reply=mine; _clipd_client=73
` + block + `
clipd </dev/null >/dev/null || exit
printf '%s|%s|%s' "$_clipd_sock" "$_clipd_reply" "$_clipd_client"
`
	out, err := exec.Command(sh, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("run generated function: %v\n%s", err, out)
	}
	if got := string(out); got != "mine|mine|73" {
		t.Fatalf("interactive variables changed to %q", got)
	}
}

// TestShellFunctionIgnoresAliasesAndFunctions sources and executes the complete
// generated block in each supported shell shape. An alias named clipd used to
// turn the following `clipd() (` into a syntax error, while aliases or functions
// named after data-path utilities could silently change bytes or bypass cleanup.
func TestShellFunctionIgnoresAliasesAndFunctions(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("", "clipd-aliases-")
	if err != nil {
		t.Fatalf("create short test directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("create test socket: %v", err)
	}
	defer listener.Close()

	transportPath := filepath.Join(dir, "clipd_test_transport")
	transport := `#!/bin/sh
cat > "$CLIPD_TEST_CAPTURE" || exit 1
printf 'clipd: ok: received\n'
`
	if err := os.WriteFile(transportPath, []byte(transport), 0o700); err != nil {
		t.Fatalf("write fake transport: %v", err)
	}
	blockPath := filepath.Join(dir, "clipd-block.sh")
	block := shellFunction(fakeTransport("clipd_test_transport"), socket, true)
	if err := os.WriteFile(blockPath, []byte(block), 0o600); err != nil {
		t.Fatalf("write generated block: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("archive contents"), 0o600); err != nil {
		t.Fatalf("write archive source: %v", err)
	}

	shells := []struct {
		name    string
		command string
		prelude string
	}{
		{name: "POSIX sh", command: "sh"},
		{name: "Dash", command: "dash"},
		{name: "Bash", command: "bash", prelude: "shopt -s expand_aliases\n"},
		{name: "Zsh", command: "zsh"},
	}
	for _, shell := range shells {
		shell := shell
		t.Run(shell.name, func(t *testing.T) {
			shellPath, err := exec.LookPath(shell.command)
			if err != nil {
				t.Skipf("%s is unavailable", shell.command)
			}

			namedCapture := filepath.Join(dir, shell.command+"-named")
			archiveCapture := filepath.Join(dir, shell.command+"-archive")
			script := shell.prelude + `printf() { return 91; }
grep() { return 91; }
sed() { return 91; }
cat() { return 91; }
rm() { return 91; }
tar() { return 91; }
clipd_test_transport() { return 91; }
alias clipd='false'
alias printf='false'
alias grep='false'
alias sed='false'
alias cat='false'
alias rm='false'
alias tar='false'
alias clipd_test_transport='false'
. "$CLIPD_TEST_BLOCK" || exit 92
CLIPD_TEST_CAPTURE="$CLIPD_TEST_NAMED"; export CLIPD_TEST_CAPTURE
command printf payload | clipd drop --name 'quote"and\backslash.txt' || exit 93
CLIPD_TEST_CAPTURE="$CLIPD_TEST_ARCHIVE"; export CLIPD_TEST_CAPTURE
clipd drop source.txt || exit 94
`
			cmd := exec.Command(shellPath, "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CLIPD_TEST_BLOCK="+blockPath,
				"CLIPD_TEST_NAMED="+namedCapture,
				"CLIPD_TEST_ARCHIVE="+archiveCapture,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("source and execute generated block: %v\n%s", err, out)
			}

			named, err := os.ReadFile(namedCapture)
			if err != nil {
				t.Fatalf("read named request: %v", err)
			}
			r := bufio.NewReader(bytes.NewReader(named))
			structured, err := protocol.Sniff(r)
			if err != nil || !structured {
				t.Fatalf("Sniff named request = %v, %v; want structured", structured, err)
			}
			req, err := protocol.ReadRequest(r)
			if err != nil {
				t.Fatalf("read named request: %v", err)
			}
			if req.Name != `quote"and\backslash.txt` {
				t.Fatalf("named request = %q, want exact unaliased name", req.Name)
			}
			if body, err := io.ReadAll(r); err != nil || string(body) != "payload" {
				t.Fatalf("named body = %q, %v; want payload", body, err)
			}

			archive, err := os.ReadFile(archiveCapture)
			if err != nil {
				t.Fatalf("read archive request: %v", err)
			}
			frame := []byte("clipd:magic:v1\n{\"type\":\"drop-stream-v2\",\"progress\":false}\n")
			if !bytes.HasPrefix(archive, frame) {
				t.Fatalf("archive request has no frame: %q", archive)
			}
			tr := tar.NewReader(bytes.NewReader(archive[len(frame):]))
			hdr, err := tr.Next()
			if err != nil || strings.TrimPrefix(hdr.Name, "./") != "source.txt" {
				t.Fatalf("archive entry = %v, %v; want source.txt", hdr, err)
			}
			if body, err := io.ReadAll(tr); err != nil || string(body) != "archive contents" {
				t.Fatalf("archive body = %q, %v", body, err)
			}
			if leftovers, err := filepath.Glob(filepath.Join(dir, "drop.*")); err != nil || len(leftovers) != 0 {
				t.Fatalf("staging cleanup = %v, %v; want no files", leftovers, err)
			}
		})
	}
}

// TestShellFunctionOnlySendsCompleteArchives covers the failure shape that is
// easy to miss: tar can write a valid archive prefix and then exit non-zero.
// Failed producers may send a prefix but must never send the completion marker.
func TestShellFunctionOnlySendsCompleteArchives(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}

	tests := []struct {
		name       string
		arguments  string
		wantSent   bool
		wantStatus string
	}{
		{name: "complete archive", arguments: "source.txt", wantSent: true},
		{name: "tar fails after one file", arguments: "source.txt missing.txt", wantStatus: "not completed"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// macOS caps UNIX socket paths near 104 bytes, while t.TempDir's
			// test-derived name can exceed that before "/socket" is appended.
			dir, err := os.MkdirTemp("", "clipd-shell-")
			if err != nil {
				t.Fatalf("create short test directory: %v", err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			socket := filepath.Join(dir, "socket")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatalf("create test socket: %v", err)
			}
			defer listener.Close()

			if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("complete contents"), 0o600); err != nil {
				t.Fatalf("write source: %v", err)
			}
			clientPath := filepath.Join(dir, "client.sh")
			clientScript := "#!/bin/sh\n" +
				`: > "$CLIPD_TEST_CALLED" || exit 1` + "\n" +
				`cat > "$CLIPD_TEST_CAPTURE" || exit 1` + "\n" +
				`if [ "$(tail -c 18 "$CLIPD_TEST_CAPTURE")" = "clipd:complete:v2" ]; then printf 'clipd: ok: dropped\n'; else printf 'clipd: error: incomplete archive\n'; exit 1; fi` + "\n"
			if err := os.WriteFile(clientPath, []byte(clientScript), 0o600); err != nil {
				t.Fatalf("write fake client: %v", err)
			}

			calledPath := filepath.Join(dir, "client-called")
			capturePath := filepath.Join(dir, "request")
			block := shellFunction(fakeTransport("sh "+shellQuote(clientPath)), socket, true)
			script := block + "\nclipd drop " + tc.arguments + "\n"
			cmd := exec.Command(sh, "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"CLIPD_TEST_CALLED="+calledPath,
				"CLIPD_TEST_CAPTURE="+capturePath,
			)
			out, runErr := cmd.CombinedOutput()

			// The staging file is named drop.<pid>.<n> in clipd's own
			// directory, and the function's EXIT trap removes it on every path
			// — including the one where tar failed and nothing was sent.
			leftover, err := filepath.Glob(filepath.Join(dir, "drop.*"))
			if err != nil {
				t.Fatalf("glob staging files: %v", err)
			}
			if len(leftover) != 0 {
				t.Fatalf("staging files survived function exit: %v", leftover)
			}
			if !tc.wantSent {
				if runErr == nil {
					t.Fatalf("failed tar reported success:\n%s", out)
				}
				if !strings.Contains(string(out), tc.wantStatus) {
					t.Fatalf("failure output = %q, want %q", out, tc.wantStatus)
				}
				payload, err := os.ReadFile(capturePath)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.HasSuffix(payload, []byte("clipd:complete:v2\n")) {
					t.Fatal("failed producer sent a completion marker")
				}
				return
			}

			if runErr != nil {
				t.Fatalf("complete archive failed: %v\n%s", runErr, out)
			}
			payload, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatalf("read captured request: %v", err)
			}
			frame := []byte("clipd:magic:v1\n{\"type\":\"drop-stream-v2\",\"progress\":false}\n")
			if !bytes.HasPrefix(payload, frame) {
				t.Fatalf("request has no complete protocol frame: %q", payload)
			}
			tr := tar.NewReader(bytes.NewReader(payload[len(frame):]))
			hdr, err := tr.Next()
			if err != nil {
				t.Fatalf("read captured tar header: %v", err)
			}
			if strings.TrimPrefix(hdr.Name, "./") != "source.txt" {
				t.Fatalf("tar entry = %q, want source.txt", hdr.Name)
			}
			contents, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read captured tar entry: %v", err)
			}
			if string(contents) != "complete contents" {
				t.Fatalf("tar contents = %q", contents)
			}
		})
	}
}

func TestStripBlockRemovesAManagedSection(t *testing.T) {
	t.Parallel()

	start, end := sshMarkers("debian")
	give := "Host other\n  User someone\n\n" +
		start + "\nHost debian\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + end + "\n\nHost third\n  User x\n"
	got, err := stripBlock(give, start, end)
	if err != nil {
		t.Fatalf("stripBlock: %v", err)
	}

	if strings.Contains(got, "RemoteForward") || strings.Contains(got, start) {
		t.Errorf("the managed block survived:\n%s", got)
	}
	for _, want := range []string{"Host other", "Host third"} {
		if !strings.Contains(got, want) {
			t.Errorf("stripBlock removed unrelated content %q:\n%s", want, got)
		}
	}
}

func TestStripBlockLeavesUnmanagedFilesAlone(t *testing.T) {
	t.Parallel()

	start, end := sshMarkers("debian")
	give := "Host debian\n  User cole\n  RemoteForward mine:yours"
	got, err := stripBlock(give, start, end)
	if err != nil {
		t.Fatalf("stripBlock: %v", err)
	}
	if got != give {
		t.Errorf("stripBlock changed an unmanaged file:\n%s", got)
	}
}

func TestStripBlockRefusesMalformedMarkerOrder(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"unmatched end":       blockEnd + "\nkeep",
		"reversed":            blockEnd + "\ninside\n" + blockStart,
		"nested":              blockStart + "\n" + blockStart + "\n" + blockEnd + "\n" + blockEnd,
		"duplicate blocks":    blockStart + "\n" + blockEnd + "\n" + blockStart + "\n" + blockEnd,
		"duplicate end":       blockStart + "\n" + blockEnd + "\n" + blockEnd,
		"duplicate start":     blockStart + "\n" + blockStart + "\n" + blockEnd,
		"start without close": "keep\n" + blockStart + "\ninside",
	}
	for name, content := range tests {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := stripBlock(content, blockStart, blockEnd); err == nil {
				t.Fatalf("stripBlock accepted malformed markers:\n%s", content)
			}
		})
	}
}

// TestInstallScriptRejectsMalformedMarkersBeforeWriting exercises the script
// that actually runs remotely. Equal marker counts are not enough: reversed or
// nested pairs previously passed that check and made awk discard user content.
func TestInstallScriptRejectsMalformedMarkersBeforeWriting(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}

	tests := map[string]string{
		"unmatched start":  "keep\n" + blockStart + "\ninside\n",
		"unmatched end":    blockEnd + "\nkeep\n",
		"reversed":         blockEnd + "\ninside\n" + blockStart + "\n",
		"nested":           blockStart + "\n" + blockStart + "\ninside\n" + blockEnd + "\n" + blockEnd + "\n",
		"duplicate blocks": blockStart + "\nold one\n" + blockEnd + "\n" + blockStart + "\nold two\n" + blockEnd + "\n",
	}
	for name, original := range tests {
		name, original := name, original
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			rcPath := filepath.Join(home, ".profile")
			if err := os.WriteFile(rcPath, []byte(original), 0o600); err != nil {
				t.Fatalf("seed rc file: %v", err)
			}
			newBlock := blockStart + "\nclipd() { :; }\n" + blockEnd
			script := fmt.Sprintf(installScript, "$HOME/.profile", staleSocketScript(fakeTransport(`nc -U "$_clipd_sock"`)), newBlock)
			cmd := exec.Command(sh, "-c", script)
			cmd.Env = append(os.Environ(), "HOME="+home)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("install script accepted malformed markers:\n%s", original)
			}
			if !strings.Contains(string(out), "malformed clipd markers") {
				t.Fatalf("install error did not explain the malformed markers:\n%s", out)
			}

			got, err := os.ReadFile(rcPath)
			if err != nil {
				t.Fatalf("read rc file: %v", err)
			}
			if string(got) != original {
				t.Fatalf("install changed malformed rc file:\ngot:  %q\nwant: %q", got, original)
			}
			if _, err := os.Stat(rcPath + ".clipd-backup"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("install created a backup before validation: %v", err)
			}
		})
	}
}

// TestInstallScriptRemovesOnlyAStaleSocket covers re-running setup as the fix
// for a socket an earlier session left behind. A socket that accepts and never
// answers, as one held by an unreachable earlier session does, must neither be
// removed nor stall setup until its command timeout prevents the install.
func TestInstallScriptRemovesOnlyAStaleSocket(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc unavailable")
	}
	for _, shell := range []string{"sh", "dash"} {
		binary, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		for _, state := range []string{"stale", "closes", "silent"} {
			testInstallScriptSocket(t, shell, binary, state)
		}
	}
}

// testInstallScriptSocket runs setup's remote install script, under set -e, as
// shell against a socket in the given state.
func testInstallScriptSocket(t *testing.T, shell, binary, state string) {
	t.Run(shell+"/"+state, func(t *testing.T) {
		t.Parallel()
		home, err := os.MkdirTemp("", "cdh")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(home)
		if err := os.Mkdir(filepath.Join(home, ".clipd"), 0o700); err != nil {
			t.Fatal(err)
		}
		socket := filepath.Join(home, ".clipd", "socket")
		if state == "stale" {
			staleSocket(t, socket)
		} else {
			listenUnix(t, socket, state == "silent")
		}
		script := fmt.Sprintf(installScript, "$HOME/.profile", staleSocketScript(localTransport(t)), blockStart+"\nclipd() { :; }\n"+blockEnd)
		ctx, cancel := context.WithTimeout(context.Background(), (probeSeconds+5)*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-c", script)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("install failed or stalled: %v\n%s", err, out)
		}
		if rc, err := os.ReadFile(filepath.Join(home, ".profile")); err != nil || !strings.Contains(string(rc), blockStart) {
			t.Fatalf("function was not installed: %v", err)
		}
		_, statErr := os.Lstat(socket)
		removed := strings.Contains(string(out), "clipd-removed-stale-socket")
		if state != "stale" && (removed || statErr != nil) {
			t.Fatalf("install removed a live socket: %s", out)
		}
		if state == "stale" && (!removed || !os.IsNotExist(statErr)) {
			t.Fatalf("install kept a stale socket: %s, %v", out, statErr)
		}
	})
}

// TestInstallSSHConfigIsIdempotent cannot be parallel: it points HOME at a
// temp directory, which is process-wide state.
func TestInstallSSHConfigIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	start, end := sshMarkers("debian")
	block := start + "\nHost debian\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + end + "\n"

	path, changed, err := installSSHConfig(t.Context(), "debian", block)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !changed {
		t.Error("the first install reported no change")
	}

	_, changed, err = installSSHConfig(t.Context(), "debian", block)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if changed {
		t.Error("the second install rewrote an already-current file")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := strings.Count(string(data), start); n != 1 {
		t.Errorf("the config holds %d managed blocks, want 1:\n%s", n, data)
	}
}

// TestInstallSSHConfigPreservesAndBacksUp: this file is often hand-tuned, so
// existing content must survive and a backup must exist before the first edit.
func TestInstallSSHConfigPreservesAndBacksUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := "Host work\n  User me\n  IdentityFile ~/.ssh/work_ed25519\n"
	configPath := filepath.Join(sshDir, "config")
	if err := os.WriteFile(configPath, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	start, end := sshMarkers("debian")
	block := start + "\nHost debian\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + end + "\n"
	if _, _, err := installSSHConfig(t.Context(), "debian", block); err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "IdentityFile ~/.ssh/work_ed25519") {
		t.Errorf("the existing configuration was lost:\n%s", data)
	}
	if !strings.Contains(string(data), "RemoteForward \"/home/c/.clipd/socket\"") {
		t.Errorf("the forward was not added:\n%s", data)
	}

	backup, err := os.ReadFile(configPath + ".clipd-backup")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != existing {
		t.Errorf("backup = %q, want the original file", backup)
	}
}

func TestInstallSSHConfigPreservesConfigSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	dotfilesDir := filepath.Join(home, "dotfiles")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("create ssh directory: %v", err)
	}
	if err := os.MkdirAll(dotfilesDir, 0o700); err != nil {
		t.Fatalf("create dotfiles directory: %v", err)
	}
	existing := "Host work\n  User me\n"
	target := filepath.Join(dotfilesDir, "ssh-config")
	if err := os.WriteFile(target, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed symlink target: %v", err)
	}
	configPath := filepath.Join(sshDir, "config")
	relativeTarget := filepath.Join("..", "dotfiles", "ssh-config")
	if err := os.Symlink(relativeTarget, configPath); err != nil {
		t.Fatalf("create config symlink: %v", err)
	}

	start, end := sshMarkers("debian")
	block := start + "\nHost debian\n" + end + "\n"
	path, changed, err := installSSHConfig(t.Context(), "debian", block)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !changed {
		t.Fatal("install reported no change")
	}
	if path != configPath {
		t.Fatalf("reported path = %q, want logical config path %q", path, configPath)
	}

	info, err := os.Lstat(configPath)
	if err != nil {
		t.Fatalf("lstat config: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("install replaced ~/.ssh/config symlink with a regular file")
	}
	if got, err := os.Readlink(configPath); err != nil || got != relativeTarget {
		t.Fatalf("config symlink = %q, %v; want %q", got, err, relativeTarget)
	}
	updated, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read symlink target: %v", err)
	}
	if !strings.Contains(string(updated), "Host work") || !strings.Contains(string(updated), start) {
		t.Fatalf("target was not updated while preserving existing content:\n%s", updated)
	}
	backup, err := os.ReadFile(configPath + ".clipd-backup")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != existing {
		t.Fatalf("backup = %q, want original target contents %q", backup, existing)
	}
}

func TestInstallSSHConfigRefusesDanglingConfigSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("create ssh directory: %v", err)
	}
	configPath := filepath.Join(sshDir, "config")
	target := filepath.Join("..", "missing", "ssh-config")
	if err := os.Symlink(target, configPath); err != nil {
		t.Fatalf("create dangling symlink: %v", err)
	}

	start, end := sshMarkers("debian")
	if _, _, err := installSSHConfig(t.Context(), "debian", start+"\nHost debian\n"+end+"\n"); err == nil {
		t.Fatal("install accepted a dangling ~/.ssh/config symlink")
	}
	info, err := os.Lstat(configPath)
	if err != nil {
		t.Fatalf("lstat config: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("install replaced the dangling symlink")
	}
	if got, err := os.Readlink(configPath); err != nil || got != target {
		t.Fatalf("config symlink = %q, %v; want %q", got, err, target)
	}
}

func TestInstallSSHConfigWritesRestrictivePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	start, end := sshMarkers("debian")
	path, _, err := installSSHConfig(t.Context(), "debian", start+"\nHost debian\n"+end+"\n")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// ssh itself refuses to use a config other users can write.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config mode = %04o, want no group or other access", perm)
	}
}

// TestHostPatternStripsTheUser pins a silent failure: ssh removes the user
// before matching Host patterns, so a block written as "Host cole@debian"
// matches nothing and the forward never happens, with no error anywhere.
func TestHostPatternStripsTheUser(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ user, host string }{
		"debian":            {"", "debian"},
		"cole@debian":       {"cole", "debian"},
		"cole@debian.local": {"cole", "debian.local"},
		"root@10.0.0.5":     {"root", "10.0.0.5"},
	}
	for give, want := range tests {
		user, host := splitDestination(give)
		if user != want.user || host != want.host {
			t.Errorf("splitDestination(%q) = %q, %q; want %q, %q", give, user, host, want.user, want.host)
		}
	}
}

// TestSSHBlockScopesToTheUser: ssh strips the user before matching Host
// patterns, so `Host server` applies to every account on that machine. Setting
// up bob@server then replaced alice's block and pointed her connections at
// bob's home directory, where the forward fails with a permission error.
func TestSSHBlockScopesToTheUser(t *testing.T) {
	t.Parallel()

	withUser, err := sshBlockFor("alice@server", "/home/alice/.clipd/socket", "/Users/c/.clipd.sock")
	if err != nil {
		t.Fatalf("sshBlockFor: %v", err)
	}
	if !strings.Contains(withUser, `Match originalhost "server" user "alice"`) {
		t.Errorf("a user-qualified destination did not produce a Match block:\n%s", withUser)
	}

	bare, err := sshBlockFor("server", "/home/c/.clipd/socket", "/Users/c/.clipd.sock")
	if err != nil {
		t.Fatalf("sshBlockFor: %v", err)
	}
	if !strings.Contains(bare, `Host "server"`) {
		t.Errorf("a bare destination did not produce a Host block:\n%s", bare)
	}
	// Both halves of the socket's protection on a shared remote host.
	// StreamLocalBindUnlink is deliberately not emitted: for a remote forward the
	// socket is bound by the remote's sshd, so only its sshd_config governs. The
	// client-side option was measured against a real host and had no effect.
	if strings.Contains(bare, "StreamLocalBindUnlink") {
		t.Errorf("the block emits StreamLocalBindUnlink, which does nothing for a remote forward:\n%s", bare)
	}
	for _, want := range []string{"StreamLocalBindMask 0177"} {
		if !strings.Contains(bare, want) {
			t.Errorf("the block is missing %q:\n%s", want, bare)
		}
	}
}

func TestSSHBlockResetsScopeBeforeFollowingDirectives(t *testing.T) {
	t.Parallel()

	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh is unavailable")
	}
	tests := []struct {
		name        string
		destination string
	}{
		{name: "bare Host block", destination: "server"},
		{name: "account Match block", destination: "alice@server"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			block, err := sshBlockFor(tc.destination, "/home/alice/.clipd/socket", "/Users/c/.clipd.sock")
			if err != nil {
				t.Fatalf("sshBlockFor: %v", err)
			}
			configPath := filepath.Join(t.TempDir(), "config")
			// This is a global directive the user appended after setup's marker.
			// Without a real Host/Match reset, it remains captured by clipd's stanza
			// and does not apply to an unrelated destination.
			if err := os.WriteFile(configPath, []byte(block+"Port 2222\n"), 0o600); err != nil {
				t.Fatalf("write candidate config: %v", err)
			}
			cmd := exec.Command(ssh, "-G", "-F", configPath, "unrelated.invalid")
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("ssh -G: %v", err)
			}
			var port string
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[0] == "port" {
					port = fields[1]
					break
				}
			}
			if port != "2222" {
				t.Fatalf("directive after managed block resolved to port %q, want 2222\n%s", port, block)
			}
		})
	}
}

// TestSSHBlockMatchesTheNameAsTyped guards a failure with no symptom.
//
// `Match host` is evaluated after HostName substitution, so against an alias
// like "Host myserver / HostName real.example.com" it compares the pattern to
// real.example.com and never fires. The generated block reads correctly, the
// forward silently never happens, and the first sign of trouble is clipd not
// working on that host. `Match originalhost` compares the name the user typed,
// which is the one setup was given.
func TestSSHBlockMatchesTheNameAsTyped(t *testing.T) {
	t.Parallel()

	block, err := sshBlockFor("alice@myserver", "/home/alice/.clipd/socket", "/Users/c/.clipd.sock")
	if err != nil {
		t.Fatalf("sshBlockFor: %v", err)
	}
	if strings.Contains(block, "Match host ") {
		t.Errorf("the block matches on the resolved hostname, which an alias defeats:\n%s", block)
	}
	if !strings.Contains(block, `Match originalhost "myserver"`) {
		t.Errorf("the block does not match on the name as typed:\n%s", block)
	}
}

// TestSSHBlockRefusesAWideningPattern: `clipd setup '*'` would write a stanza
// matching every host, so the forward is attempted on every connection.
func TestSSHBlockRefusesAWideningPattern(t *testing.T) {
	t.Parallel()

	for _, destination := range []string{"*", "web?", "!prod", "a b", "-oProxyCommand=id", "alice@-host"} {
		if _, err := sshBlockFor(destination, "/r/socket", "/l/socket"); err == nil {
			t.Errorf("sshBlockFor(%q) wrote a block for a pattern, not a host", destination)
		}
	}
}

// TestSSHBlockQuotesAndEscapes: a path with a space splits into the wrong
// number of arguments unquoted, and ssh expands %h and friends inside these
// values whatever the quoting.
func TestSSHBlockQuotesAndEscapes(t *testing.T) {
	t.Parallel()

	block, err := sshBlockFor("server", "/home/100%real/my socket", "/Users/c/.clipd.sock")
	if err != nil {
		t.Fatalf("sshBlockFor: %v", err)
	}
	if !strings.Contains(block, `"/home/100%%real/my socket"`) {
		t.Errorf("the remote path was not quoted and percent-escaped:\n%s", block)
	}
}

// TestStripBlockRefusesAnUnmatchedMarker: the previous version skipped from a
// start marker to the end of the file looking for a close that was not there,
// so a half-deleted block took the rest of ~/.ssh/config with it.
func TestStripBlockRefusesAnUnmatchedMarker(t *testing.T) {
	t.Parallel()

	content := "Host keep\n  User me\n" + blockStart + "\nHost gone\n"
	if _, err := stripBlock(content, blockStart, blockEnd); err == nil {
		t.Error("stripBlock accepted an unmatched start marker")
	}
}

// TestSeveralHostsCoexist is the property one shared marker broke: setup is run
// once per host, and each run must leave the others' forwards alone. With a
// single marker the second run stripped the first on its way to writing its
// own, so only the most recent host kept a forward.
func TestSeveralHostsCoexist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	hosts := []string{"debian", "debian.local", "10.0.0.5"}
	for _, h := range hosts {
		start, end := sshMarkers(h)
		block := start + "\nHost " + h + "\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + end + "\n"
		if _, _, err := installSSHConfig(t.Context(), h, block); err != nil {
			t.Fatalf("setup %s: %v", h, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, h := range hosts {
		if !strings.Contains(string(data), "Host "+h+"\n") {
			t.Errorf("%s lost its block after later setups:\n%s", h, data)
		}
	}
}

// TestLegacyUnhostedBlockIsReplaced covers the upgrade: a config written before
// the markers carried a host name has one unhosted block, which would otherwise
// survive and forward the same socket twice.
func TestLegacyUnhostedBlockIsReplaced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := blockStart + "\nHost debian\n  RemoteForward \"/home/c/.clipd/socket\" \"/Users/c/.clipd.sock\"\n" + blockEnd + "\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	start, end := sshMarkers("debian")
	block := start + "\nHost debian\n  RemoteForward \"/home/c/.clipd/new\" \"/Users/c/.clipd.sock\"\n" + end + "\n"
	if _, _, err := installSSHConfig(t.Context(), "debian", block); err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sshDir, "config"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "old:old") {
		t.Errorf("the legacy block survived, forwarding the socket twice:\n%s", data)
	}
	if !strings.Contains(string(data), "/home/c/.clipd/new") {
		t.Errorf("the new block is missing:\n%s", data)
	}
}

// TestShellFunctionHandlesBothDropForms pins the two shapes the daemon
// distinguishes: files on the command line become a tar, and --name sends this
// shell's stdin under a name of its own.
//
// The distinction used to be drawn from whether stdin was a terminal, which
// made `ssh host 'clipd drop report.pdf'` — and every drop from a script or a
// cron job — send a header with no body, creating an empty file under the right
// name and reporting success.
func TestShellFunctionHandlesBothDropForms(t *testing.T) {
	t.Parallel()

	block := shellFunction(fakeTransport(`nc -N -U "$_clipd_sock"`), "/home/cole/.clipd/socket", true)

	if strings.Contains(block, "[ -t 0 ]") {
		t.Errorf("the function still infers the drop form from the terminal:\n%s", block)
	}
	if !strings.Contains(block, `if [ "${1:-}" = "--name" ]`) {
		t.Errorf("the function does not branch on an explicit --name:\n%s", block)
	}
	if !strings.Contains(block, `COPYFILE_DISABLE=1 command tar cf - "$@"`) {
		t.Errorf("the file form does not tar its arguments:\n%s", block)
	}
	if !strings.Contains(block, `"name":"%s"`) {
		t.Errorf("the --name form does not send a name:\n%s", block)
	}
	// The name lands inside a JSON string, so those two characters must be
	// escaped or a filename with a quote produces a parse error on the daemon.
	if !strings.Contains(block, "sed 's/") {
		t.Errorf("the --name form does not escape the name for JSON:\n%s", block)
	}
}

// TestShellFunctionSurvivesNounset: a plain `printf x | clipd` passes no
// arguments, and expanding $1 unguarded aborted the whole shell under `set -u`.
func TestShellFunctionSurvivesNounset(t *testing.T) {
	t.Parallel()

	block := shellFunction(fakeTransport(`nc -N -U "$_clipd_sock"`), "/home/cole/.clipd/socket", true)
	if strings.Contains(block, "_clipd_name=\"$1\"") {
		t.Errorf("the function expands $1 unguarded:\n%s", block)
	}
	if !strings.Contains(block, `"${1:-}"`) {
		t.Errorf("the function does not guard its first argument:\n%s", block)
	}
}

// TestShellFunctionReportsFailureAsExitStatus: both the daemon and tar have to
// succeed before `clipd drop x && rm x` is allowed to delete the source.
func TestShellFunctionReportsFailureAsExitStatus(t *testing.T) {
	t.Parallel()

	block := shellFunction(fakeTransport(`nc -N -U "$_clipd_sock"`), "/home/cole/.clipd/socket", true)
	if !strings.Contains(block, "'clipd: ok: '*) return 0 ;;") {
		t.Errorf("the function does not turn the daemon's reply into an exit status:\n%s", block)
	}
	if !strings.Contains(block, "if COPYFILE_DISABLE=1 command tar") {
		t.Errorf("the function does not test tar's exit status:\n%s", block)
	}
	if strings.Contains(block, "_clipd_payload") || !strings.Contains(block, "clipd:complete:v2") {
		t.Error("archive must stream with a completion marker and no payload spool")
	}
	if strings.Contains(block, "_clipd_tar") || strings.Contains(block, "_clipd_rc") {
		t.Errorf("the function retains obsolete pipeline-status bookkeeping:\n%s", block)
	}
}

// TestShellFunctionDoesNotLeakVariables: this runs in the user's interactive
// shell, where a bare `name` or `sock` would overwrite one of theirs.
func TestShellFunctionDoesNotLeakVariables(t *testing.T) {
	t.Parallel()

	block := shellFunction(fakeTransport(`nc -N -U "$_clipd_sock"`), "/home/cole/.clipd/socket", true)
	for _, bare := range []string{"\n  sock=", "\n      name=", "\n      esc="} {
		if strings.Contains(block, bare) {
			t.Errorf("the function assigns an unprefixed variable %q:\n%s", bare, block)
		}
	}
}

// TestMacOSRemoteDoesNotGetTheOpenBSDFlag is the case the OS probe exists for.
//
// Both netcats advertise -N, so a check for the flag's presence cannot tell
// them apart. On macOS it takes a probe count for a write timeout, and passing
// it the way OpenBSD's is passed fails with "invalid tcp adaptive write
// timeout value" on every copy — after setup has already reported success.
func TestMacOSRemoteDoesNotGetTheOpenBSDFlag(t *testing.T) {
	t.Parallel()

	got, err := clientCommand(remoteFacts{
		os: "Darwin", hasNC: true, ncUnix: true, ncShutdown: true,
	})
	if err != nil {
		t.Fatalf("clientCommand: %v", err)
	}
	for _, command := range []string{got.send, got.probe} {
		if strings.Contains(command, "-N") {
			t.Errorf("command = %q, want no -N on a macOS remote", command)
		}
		if !strings.Contains(command, "-U") {
			t.Errorf("command = %q, want it to still use the socket", command)
		}
	}
}

// TestProbeAsksForTheOperatingSystem: the client choice depends on it, so it
// has to be collected rather than assumed.
func TestProbeAsksForTheOperatingSystem(t *testing.T) {
	t.Parallel()

	if !strings.Contains(probeScript, "uname -s") {
		t.Errorf("the probe does not ask the host what it is:\n%s", probeScript)
	}
}
