package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/server"
)

// localTransport is the generated transport for this machine's own netcat.
func localTransport(t *testing.T) transport {
	t.Helper()
	facts := remoteFacts{os: "Linux", hasNC: true, ncUnix: true, ncShutdown: true}
	if runtime.GOOS == "darwin" {
		facts.os = "Darwin"
	}
	client, err := clientCommand(facts)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGeneratedClientStreamsToRealReceiver(t *testing.T) {
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc unavailable")
	}
	for _, shell := range []string{"sh", "dash", "bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			binary, err := exec.LookPath(shell)
			if err != nil {
				t.Skip("shell unavailable")
			}
			dir, err := os.MkdirTemp("", "clipd-e2e-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "socket")
			destination := filepath.Join(dir, "received")
			fake := &clipboard.Fake{}
			srv, err := server.New(server.Options{Clipboard: fake, DropDir: destination, MaxPayload: 1024, MaxDropBytes: 1 << 20, MaxTransfer: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			ln, err := server.Listen(socket)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ctx, ln) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			book := filepath.Join(dir, "author", "book title")
			if err := os.MkdirAll(filepath.Join(book, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"audio.m4b": "audio", "nested/quote\".cue": "cue", "--use-compress-program=bad": "safe"} {
				if err := os.WriteFile(filepath.Join(book, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("/etc/passwd", filepath.Join(book, "link")); err != nil {
				t.Fatal(err)
			}
			client := localTransport(t)
			block := shellFunction(client, socket, true)
			script := "set -u\n" + block + "\nclipd drop 'author/book title'\nclipd drop 'author/book title/audio.m4b'\nclipd drop 'author/book title'\n"
			command := exec.CommandContext(ctx, binary, "-c", script)
			command.Dir = dir
			out, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("transfer failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "skipped 1 symlink or other special file") {
				t.Fatalf("symlink skip not reported: %s", out)
			}
			if _, err := os.Lstat(filepath.Join(destination, "book title", "link")); !os.IsNotExist(err) {
				t.Fatalf("symlink was recreated: %v", err)
			}
			for _, name := range []string{"book title/audio.m4b", "book title/nested/quote\".cue", "book title/--use-compress-program=bad", "audio.m4b", "book title-1/audio.m4b"} {
				if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
					t.Fatalf("missing %s: %v", name, err)
				}
			}
			failed := exec.CommandContext(ctx, binary, "-c", block+"\nclipd drop 'author/book title/audio.m4b' missing\n")
			failed.Dir = dir
			output, err := failed.CombinedOutput()
			if err == nil {
				t.Fatalf("failed tar reported success: %s", output)
			}
			if _, err := os.Stat(filepath.Join(destination, "audio-1.m4b")); !os.IsNotExist(err) {
				t.Fatal("partial transfer was published")
			}
			if fake.WriteCount() != 0 {
				t.Fatal("structured transfer touched clipboard")
			}
			if strings.Contains(string(out), "clipd: progress:") {
				t.Fatal("noninteractive transfer printed progress")
			}
			if runtime.GOOS == "darwin" {
				if scriptPath, err := exec.LookPath("script"); err == nil {
					ttyCtx, stop := context.WithTimeout(ctx, 10*time.Second)
					tty := exec.CommandContext(ttyCtx, scriptPath, "-q", "/dev/null", binary, "-c", block+"\nclipd drop 'author/book title/audio.m4b'\n")
					tty.Dir = dir
					ttyOutput, ttyErr := tty.CombinedOutput()
					stop()
					if ttyErr != nil || !strings.Contains(string(ttyOutput), "clipd: progress:") || !strings.Contains(string(ttyOutput), "clipd: ok:") {
						t.Fatalf("interactive progress: %s, %v", ttyOutput, ttyErr)
					}
				}
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "drop.") {
					t.Fatal("client created a payload spool")
				}
			}
		})
	}
}

func TestRemoteHelpWorksWithoutForward(t *testing.T) {
	block := shellFunction(fakeTransport(`nc -U "$_clipd_sock"`), "/nonexistent/socket", true)
	out, err := exec.Command("sh", "-c", block+"\nclipd -h\n").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "clipd restart") {
		t.Fatalf("help = %s, %v", out, err)
	}
}

func TestNamedInputDoesNotRequireTar(t *testing.T) {
	dir, err := os.MkdirTemp("", "clipd-no-tar-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := server.Listen(filepath.Join(dir, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	block := shellFunction(fakeTransport(`printf 'clipd: ok: dropped\n'`), filepath.Join(dir, "socket"), false)
	out, err := exec.Command("sh", "-c", block+"\nprintf payload | clipd drop --name report.txt\n").CombinedOutput()
	if err != nil {
		t.Fatalf("named input required tar: %s, %v", out, err)
	}
}

func TestClientRejectsAmbiguousReplies(t *testing.T) {
	dir, err := os.MkdirTemp("", "clipd-reply-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "socket")
	ln, err := server.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, reply := range []string{"clipd: ok: first\nclipd: error: second\n", "clipd: ok: first\nclipd: progress: late\n", "unexpected\n", ""} {
		client := filepath.Join(dir, "client")
		if err := os.WriteFile(client, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' "+shellQuote(reply)+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		block := shellFunction(fakeTransport(shellQuote(client)), socket, true)
		out, err := exec.Command("sh", "-c", block+"\nprintf payload | clipd\n").CombinedOutput()
		if err == nil {
			t.Fatalf("accepted ambiguous reply %q: %s", reply, out)
		}
	}
}

// staleSocket leaves a socket file with nothing bound to it, as sshd does when
// a session ends.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientRemovesStaleSocket(t *testing.T) {
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc unavailable")
	}
	client := localTransport(t)
	for _, shell := range []string{"sh", "dash", "bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			binary, err := exec.LookPath(shell)
			if err != nil {
				t.Skip("shell unavailable")
			}
			dir, err := os.MkdirTemp("", "clipd-stale-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "socket")
			if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("body"), 0600); err != nil {
				t.Fatal(err)
			}
			block := shellFunction(client, socket, true)
			for _, use := range []string{"printf hi | clipd", "clipd drop file.txt", "printf hi | clipd drop --name x.txt"} {
				staleSocket(t, socket)
				command := exec.Command(binary, "-c", "set -u\n"+block+"\n"+use+"\n")
				command.Dir = dir
				out, err := command.CombinedOutput()
				if err == nil {
					t.Fatalf("%s: succeeded against a stale socket: %s", use, out)
				}
				if !strings.Contains(string(out), "left behind by an earlier SSH session") {
					t.Fatalf("%s: stale socket not explained: %s", use, out)
				}
				if strings.Contains(string(out), "tar failed") || strings.Contains(string(out), "did not complete") {
					t.Fatalf("%s: misleading failure message: %s", use, out)
				}
				if _, err := os.Lstat(socket); !os.IsNotExist(err) {
					t.Fatalf("%s: stale socket was not removed: %v", use, err)
				}
			}
		})
	}
}

// A forward whose Mac daemon is down accepts the connection and then closes
// it. That socket belongs to a live session and must survive.
func TestClientPreservesLiveSocketWithoutReply(t *testing.T) {
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc unavailable")
	}
	client := localTransport(t)
	dir, err := os.MkdirTemp("", "clipd-live-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "socket")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	out, err := exec.Command("sh", "-c", shellFunction(client, socket, true)+"\nprintf hi | clipd\n").CombinedOutput()
	if err == nil {
		t.Fatalf("succeeded without a reply: %s", out)
	}
	if !strings.Contains(string(out), "did not complete") || strings.Contains(string(out), "left behind") {
		t.Fatalf("live socket failure misreported: %s", out)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("live socket was removed: %v", err)
	}
}

// The stale-socket rule decides from the probe's exit status and output alone,
// so each outcome is driven here by a fake probe against a real stale socket.
// Only exit 1 without a reply may delete, and only with a refusal diagnostic
// unless the client is one that refuses silently.
func TestStaleSocketRule(t *testing.T) {
	tests := []struct {
		name   string
		code   int
		output string // the client's stderr
		reply  string // the client's stdout
		silent bool
		remove bool
	}{
		{"refused with diagnostic", 1, "nc: socket: Connection refused", "", false, true},
		{"socat refusal", 1, "2026/10/06 E connect(5, AF=1 \"socket\", 9): Connection refused", "", false, true},
		{"silent exit 1, macOS nc", 1, "", "", true, true},
		{"silent exit 1, other client", 1, "", "", false, false},
		{"refused in a path, other error", 1, "nc: /home/refused/.clipd/socket: Permission denied", "", false, false},
		{"refused in a path, macOS nc", 1, "nc: /home/refused/.clipd/socket: Permission denied", "", true, false},
		{"reply with a refusal diagnostic", 1, "nc: socket: Connection refused", "clipd: ok: pong", false, false},
		{"reply, silent exit 1", 1, "", "clipd: ok: pong", true, false},
		{"timeout or peer close", 0, "", "", true, false},
		{"reply", 0, "", "clipd: ok: pong", true, false},
		{"missing path", 1, "nc: socket: No such file or directory", "", false, false},
		{"other status", 2, "", "", true, false},
		{"refusal with other status", 2, "nc: socket: Connection refused", "", false, false},
		{"killed by a signal", 143, "", "", true, false},
	}
	for _, shell := range []string{"sh", "dash", "bash", "zsh"} {
		binary, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		for _, tc := range tests {
			t.Run(shell+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				testStaleSocketRule(t, binary, tc.code, tc.output, tc.reply, tc.silent, tc.remove)
			})
		}
	}
}

func testStaleSocketRule(t *testing.T, shell string, code int, output, reply string, silent, remove bool) {
	t.Helper()
	dir, err := os.MkdirTemp("", "clipd-rule-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "socket")
	staleSocket(t, socket)
	probe := transport{
		probe:         `sh -c 'cat >/dev/null; [ -z "$PROBE_OUTPUT" ] || printf "%s\n" "$PROBE_OUTPUT" >&2; [ -z "$PROBE_REPLY" ] || printf "%s\n" "$PROBE_REPLY"; exit "$PROBE_CODE"'`,
		silentRefusal: silent,
	}
	script := "_clipd_sock=" + shellQuote(socket) + "\n" + staleSocketScript(probe) + `printf 'stale=%s\n' "$_clipd_stale"`
	command := exec.Command(shell, "-c", script)
	command.Env = append(os.Environ(), "PROBE_OUTPUT="+output, "PROBE_REPLY="+reply, fmt.Sprintf("PROBE_CODE=%d", code))
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	_, statErr := os.Lstat(socket)
	if removed := os.IsNotExist(statErr); removed != remove {
		t.Fatalf("removed = %v, want %v: %s", removed, remove, out)
	}
	if want := map[bool]string{true: "stale=1", false: "stale=0"}[remove]; !strings.Contains(string(out), want) {
		t.Fatalf("output %q, want %s", out, want)
	}
}

// listenUnix serves socket until the test ends. With silent set, accepted
// connections are held open and never answered, as a forward whose far end is
// unreachable would be; otherwise each is read and closed without a reply, as
// a forward to a stopped Mac daemon is.
func listenUnix(t *testing.T, socket string, silent bool) {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if silent {
				t.Cleanup(func() { conn.Close() })
				continue
			}
			_, _ = conn.Read(make([]byte, 64))
			conn.Close()
		}
	}()
}

// The probe must give up on a socket that accepts and stays silent, rather
// than waiting out the transfer's day-long timeout, and must keep every socket
// that accepted.
func TestStaleSocketProbeWithRealTransports(t *testing.T) {
	t.Parallel()
	transports := map[string]transport{"nc": localTransport(t)}
	if _, err := exec.LookPath("socat"); err == nil {
		socat, err := clientCommand(remoteFacts{os: "Linux", hasSocat: true})
		if err != nil {
			t.Fatal(err)
		}
		transports["socat"] = socat
	}
	for name, client := range transports {
		if _, err := exec.LookPath(strings.Fields(client.probe)[0]); err != nil {
			continue
		}
		for _, state := range []string{"refused", "closes", "silent"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				t.Parallel()
				dir, err := os.MkdirTemp("", "clipd-probe-")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(dir)
				socket := filepath.Join(dir, "socket")
				switch state {
				case "refused":
					staleSocket(t, socket)
				default:
					listenUnix(t, socket, state == "silent")
				}
				script := "_clipd_sock=" + shellQuote(socket) + "\n" + staleSocketScript(client) + `printf 'stale=%s\n' "$_clipd_stale"`
				ctx, cancel := context.WithTimeout(context.Background(), (probeSeconds+5)*time.Second)
				defer cancel()
				start := time.Now()
				out, err := exec.CommandContext(ctx, "sh", "-c", script).CombinedOutput()
				if err != nil {
					t.Fatalf("probe did not finish within %v: %v\n%s", time.Since(start), err, out)
				}
				_, statErr := os.Lstat(socket)
				removed := os.IsNotExist(statErr)
				if removed != (state == "refused") || !strings.Contains(string(out), fmt.Sprintf("stale=%d", map[bool]int{true: 1, false: 0}[removed])) {
					t.Fatalf("%s socket: removed = %v\n%s", state, removed, out)
				}
			})
		}
	}
}

// readmeTCPRecipe returns the shell commands of the README's manual
// loopback-TCP recipe, so tests run what the README tells users to run rather
// than a copy that could drift from it.
func readmeTCPRecipe(t *testing.T) string {
	t.Helper()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	at := strings.Index(text, "clipd setup -print oldbox")
	if at < 0 {
		t.Fatal("README no longer contains the manual TCP recipe")
	}
	start := strings.LastIndex(text[:at], "```bash\n")
	end := strings.Index(text[at:], "```")
	if start < 0 || end < 0 {
		t.Fatal("the README's manual TCP recipe is not in a bash code block")
	}
	return text[start+len("```bash\n") : at+end]
}

// runTCPRecipe runs the README's recipe with shell in dir, with a stub clipd whose
// "setup -print" prints printed, or fails when printed is empty. It returns
// the recipe's output and the generated function, nil if none was kept.
func runTCPRecipe(t *testing.T, shell, dir, printed string) (string, []byte) {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "printed"), []byte(printed), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\n[ -s " + shellQuote(filepath.Join(dir, "printed")) + " ] || { echo 'clipd: ssh: connect to host oldbox: Connection timed out' >&2; exit 1; }\ncat " + shellQuote(filepath.Join(dir, "printed")) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "clipd"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(shell, "-c", readmeTCPRecipe(t))
	command.Dir = dir
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("recipe exited %v:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "clipd-print.txt")); !os.IsNotExist(err) {
		t.Fatal("recipe left clipd-print.txt behind")
	}
	generated, err := os.ReadFile(filepath.Join(dir, "clipd-tcp.sh"))
	if os.IsNotExist(err) {
		return string(out), nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(out), generated
}

// printedSetup is what `clipd setup -print oldbox` prints around a function.
func printedSetup(block string) string {
	return "\n  remote shell   /bin/bash\n\n--- would append to $HOME/.bashrc on oldbox ---\n" + block +
		"\n--- would append to ~/.ssh/config ---\n# >>> clipd: oldbox >>>\nHost \"oldbox\"\n# <<< clipd: oldbox <<<\n"
}

// TestManualTCPRecipeStopsOnFailure: a setup that fails, or prints no
// function, must not leave a clipd-tcp.sh that looks usable.
func TestManualTCPRecipeStopsOnFailure(t *testing.T) {
	for name, printed := range map[string]string{
		"setup fails": "",
		"no function": "\n  note: oldbox has no tar\n",
		"truncated":   printedSetup(strings.SplitN(shellFunction(localTransport(t), "/home/cole/.clipd/socket", true), "_clipd_progress=false", 2)[0]),
	} {
		for _, shell := range []string{"sh", "dash", "bash", "zsh"} {
			binary, err := exec.LookPath(shell)
			if err != nil {
				continue
			}
			t.Run(shell+"/"+name, func(t *testing.T) {
				dir, err := os.MkdirTemp("", "clipd-recipe-")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(dir)
				out, generated := runTCPRecipe(t, binary, dir, printed)
				if generated != nil || strings.Contains(out, "is ready") || !strings.Contains(out, "Could not generate clipd-tcp.sh") {
					t.Fatalf("failed generation looked usable:\n%s", out)
				}
			})
		}
	}
}

// TestManualTCPRecipe applies the README's recipe to the generated function
// and uses the result against a daemon on a loopback port. A socket left over
// from the UNIX-socket setup must be ignored, not probed or removed.
func TestManualTCPRecipe(t *testing.T) {
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skip("sed unavailable")
	}
	clients := map[string]transport{"nc": localTransport(t)}
	if _, err := exec.LookPath("socat"); err == nil {
		socat, err := clientCommand(remoteFacts{os: "Linux", hasSocat: true})
		if err != nil {
			t.Fatal(err)
		}
		clients["socat"] = socat
	}
	for name, client := range clients {
		if _, err := exec.LookPath(strings.Fields(client.send)[0]); err != nil {
			continue
		}
		for _, shell := range []string{"sh", "dash", "bash", "zsh"} {
			for _, leftover := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/leftover=%v", name, shell, leftover), func(t *testing.T) {
					testManualTCPRecipe(t, client, shell, leftover)
				})
			}
		}
	}
}

func testManualTCPRecipe(t *testing.T, client transport, shell string, withLeftover bool) {
	binary, err := exec.LookPath(shell)
	if err != nil {
		t.Skip("shell unavailable")
	}
	dir, err := os.MkdirTemp("", "clipd-tcp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fake := &clipboard.Fake{}
	srv, err := server.New(server.Options{Clipboard: fake, DropDir: filepath.Join(dir, "received"), MaxPayload: 1024, MaxDropBytes: 1 << 20, MaxTransfer: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := server.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	defer func() {
		cancel()
		<-done
	}()

	leftover := filepath.Join(dir, "socket")
	if withLeftover {
		staleSocket(t, leftover)
	}
	recipeOut, generated := runTCPRecipe(t, binary, dir, printedSetup(shellFunction(client, leftover, true)))
	if generated == nil || !strings.Contains(recipeOut, "clipd-tcp.sh is ready.") {
		t.Fatalf("recipe did not generate a function:\n%s", recipeOut)
	}
	block := strings.ReplaceAll(string(generated), "8199", port)
	if !strings.HasPrefix(block, blockStart) || strings.Contains(block, "_clipd_sock") || strings.Contains(block, "socket check") || strings.Contains(block, "oldbox") {
		t.Fatalf("recipe left socket logic or SSH config behind:\n%s", block)
	}

	if err := os.MkdirAll(filepath.Join(dir, "folder", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"folder/a.txt": "a", "folder/sub/b.txt": "b", "big.bin": strings.Repeat("x", 2<<20)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := "set -u\n" + block + `
printf hello | clipd || exit 10
printf named | clipd drop --name named.txt || exit 11
clipd drop folder || exit 12
if clipd drop big.bin; then exit 13; fi
`
	command := exec.Command(binary, "-c", script)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("TCP function failed: %v\n%s", err, out)
	}
	if got := string(fake.Data()); got != "hello" {
		t.Fatalf("clipboard = %q\n%s", got, out)
	}
	for _, name := range []string{"named.txt", "folder/a.txt", "folder/sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, "received", name)); err != nil {
			t.Fatalf("missing %s: %v\n%s", name, err, out)
		}
	}
	if !strings.Contains(string(out), "max_drop_bytes") {
		t.Fatalf("oversized drop was not rejected with its limit:\n%s", out)
	}
	if _, err := os.Lstat(leftover); withLeftover && err != nil {
		t.Fatalf("leftover socket was touched: %v", err)
	}
}

// TestManualTCPRecipeRewritesEveryClient checks the README's edits against all
// three generated transports, including ones this machine cannot run.
func TestManualTCPRecipeRewritesEveryClient(t *testing.T) {
	for want, facts := range map[string]remoteFacts{
		"command nc -N -w 86400 127.0.0.1 8199 |":                 {os: "Linux", hasNC: true, ncUnix: true, ncShutdown: true},
		"command nc -w 86400 127.0.0.1 8199 |":                    {os: "Darwin", hasNC: true, ncUnix: true},
		"command socat -T 86400 -t 86400 - TCP4:127.0.0.1:8199 |": {os: "Linux", hasSocat: true},
	} {
		client, err := clientCommand(facts)
		if err != nil {
			t.Fatal(err)
		}
		dir, err := os.MkdirTemp("", "clipd-recipe-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		out, generated := runTCPRecipe(t, "sh", dir, printedSetup(shellFunction(client, "/home/cole/.clipd/socket", true)))
		if generated == nil {
			t.Fatalf("%s: recipe failed:\n%s", want, out)
		}
		if got := strings.Count(string(generated), want); got != 3 || strings.Contains(string(generated), "_clipd_sock") {
			t.Fatalf("want 3 transfers as %q and no socket left, got %d:\n%s", want, got, generated)
		}
	}
}
