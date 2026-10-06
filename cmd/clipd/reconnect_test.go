package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/server"
)

func TestReconnectSocketRecovery(t *testing.T) {
	for _, shell := range []string{"sh", "dash", "bash", "zsh"} {
		sh, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		for _, state := range []string{"missing", "stale", "active", "alternate-listener", "transport-error", "no-nc", "file", "symlink", "changed", "public-dir"} {
			t.Run(shell+"/"+state, func(t *testing.T) {
				dir, err := os.MkdirTemp("/tmp", "clipd-reconnect-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(dir) })
				private := filepath.Join(dir, "private ' ü")
				if err := os.Mkdir(private, 0o700); err != nil {
					t.Fatal(err)
				}
				socket := filepath.Join(private, "socket")
				switch state {
				case "file":
					if err := os.WriteFile(socket, []byte("preserve"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(filepath.Join(dir, "missing"), socket); err != nil {
						t.Fatal(err)
					}
				case "missing":
				default:
					ln, err := net.Listen("unix", socket)
					if err != nil {
						t.Fatal(err)
					}
					ln.(*net.UnixListener).SetUnlinkOnClose(false)
					if state == "active" {
						defer ln.Close()
					} else {
						ln.Close()
					}
				}
				if state == "public-dir" {
					if err := os.Chmod(private, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				cmd := exec.Command(sh, "-c", "set -u\n"+reconnectScript(socket))
				nc := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' " + shellQuote("nc: "+socket+": Connection refused") + " >&2\nexit 1\n"
				if state == "alternate-listener" || state == "active" {
					nc = "#!/bin/sh\ncat >/dev/null\nexit 0\n"
				}
				if state == "transport-error" {
					nc = "#!/bin/sh\ncat >/dev/null\nprintf 'Permission denied\\n' >&2\nexit 1\n"
				}
				if state == "changed" {
					nc = "#!/bin/sh\nrm -- " + shellQuote(socket) + "\nprintf preserve > " + shellQuote(socket) + "\nprintf '%s\\n' " + shellQuote("nc: "+socket+": Connection refused") + " >&2\nexit 1\n"
				}
				if state != "no-nc" {
					if err := os.WriteFile(filepath.Join(dir, "nc"), []byte(nc), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
				if state == "no-nc" {
					ls, err := exec.LookPath("ls")
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(ls, filepath.Join(dir, "ls")); err != nil {
						t.Fatal(err)
					}
					cmd.Env = append(os.Environ(), "PATH="+dir)
				}
				out, err := cmd.CombinedOutput()
				wantOK := state == "missing" || state == "stale" || state == "active" || state == "alternate-listener"
				if (err == nil) != wantOK {
					t.Fatalf("success=%v, want %v: %s", err == nil, wantOK, out)
				}
				_, statErr := os.Lstat(socket)
				if state == "active" || state == "alternate-listener" {
					if statErr != nil || strings.TrimSpace(string(out)) != "active" {
						t.Fatalf("active listener was not preserved: %v, %s", statErr, out)
					}
				} else if wantOK {
					if !os.IsNotExist(statErr) {
						t.Fatalf("socket remains: %v", statErr)
					}
					if strings.TrimSpace(string(out)) != map[bool]string{true: "removed", false: "missing"}[state == "stale"] {
						t.Fatalf("unexpected reply: %s", out)
					}
				} else if statErr != nil {
					t.Fatalf("protected path removed: %v", statErr)
				}
			})
		}
	}
}

func TestReconnectUsage(t *testing.T) {
	for _, args := range [][]string{{"reconnect"}, {"reconnect", "one", "two"}, {"reconnect", "--", "-evil"}, {"reconnect", "*"}} {
		if got := runCLI(t, args...); got.code != exitUsage {
			t.Errorf("%v: %+v", args, got)
		}
	}
	if got := runCLI(t, "reconnect", "-h"); got.code != exitOK || !strings.Contains(got.stdout, "reconnect") {
		t.Fatalf("help: %+v", got)
	}
}

func TestSetupRejectsInvalidDestinationBeforeSSH(t *testing.T) {
	for _, host := range []string{"*", "-oProxyCommand=id", "host:22", "ssh://host"} {
		got := runCLI(t, "setup", "--", host)
		if got.code != exitUsage || strings.Contains(got.stdout, "Probing") {
			t.Fatalf("invalid destination reached SSH: %+v", got)
		}
	}
}

func TestReconnectProbeWithNativeNC(t *testing.T) {
	nc, err := exec.LookPath("nc")
	if err != nil {
		t.Skip("nc unavailable")
	}
	help, _ := exec.Command(nc, "-h").CombinedOutput()
	if !strings.Contains(string(help), "-U") {
		t.Skip("nc has no UNIX-socket support")
	}
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "clipd-probe-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "socket")
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			ln.(*net.UnixListener).SetUnlinkOnClose(false)
			fake := &clipboard.Fake{}
			if active {
				srv, err := server.New(server.Options{Clipboard: fake, DropDir: filepath.Join(dir, "drops"), MaxPayload: 1024, MaxTransfer: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- srv.Serve(ctx, ln) }()
				defer func() { cancel(); <-done }()
			} else {
				ln.Close()
			}
			cmd := exec.Command("sh", "-c", reconnectScript(socket))
			out, err := cmd.CombinedOutput()
			if !active && runtime.GOOS == "darwin" {
				// Apple's nc reports only failure for a stale UNIX socket, with
				// no errno text. Recovery must preserve this ambiguous failure.
				if err == nil {
					t.Fatal("ambiguous native nc failure accepted")
				}
				if _, err := os.Lstat(socket); err != nil {
					t.Fatalf("ambiguous socket removed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("native probe: %v %s", err, out)
			}
			want := "removed"
			if active {
				want = "active"
			}
			if strings.TrimSpace(string(out)) != want {
				t.Fatalf("reply=%q, want %q", out, want)
			}
			if fake.WriteCount() != 0 {
				t.Fatal("probe changed clipboard")
			}
			_, statErr := os.Lstat(socket)
			if active && statErr != nil {
				t.Fatalf("live socket removed: %v", statErr)
			}
			if !active && !os.IsNotExist(statErr) {
				t.Fatalf("stale socket not removed: %v", statErr)
			}
		})
	}
}

func TestReconnectSSHFlow(t *testing.T) {
	for _, state := range []string{"success", "existing-master", "start-failure", "stop-failure", "config-missing", "config-failure", "probe-failure", "daemon-missing"} {
		t.Run(state, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "clipd-reconnect-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			if err := os.Mkdir(filepath.Join(dir, ".clipd"), 0o700); err != nil {
				t.Fatal(err)
			}
			local := filepath.Join(dir, "daemon")
			cfg := filepath.Join(dir, "config.json")
			if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"address":%q}`, local)), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if state != "daemon-missing" {
				srv, err := server.New(server.Options{Clipboard: &clipboard.Fake{}, DropDir: filepath.Join(dir, "drops"), MaxPayload: 1024, MaxTransfer: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				ln, err := server.Listen(local)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- srv.Serve(ctx, ln) }()
				defer func() { cancel(); <-done }()
			}
			log := filepath.Join(dir, "ssh.log")
			fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(log) + "\n" + `
case "$*" in
  *'sh -s')
    script=$(cat)
    case $script in
      *'os=%s'*)
        [ "$CLIPD_TEST_STATE" != probe-failure ] || exit 1
        printf 'home=%s\nos=Linux\nshell=/bin/sh\n' "$CLIPD_TEST_HOME"
        ;;
      *) printf '%s\n' "$script" | sh ;;
    esac ;;
  *'-G '*)
    [ "$CLIPD_TEST_STATE" != config-failure ] || exit 1
    if [ "$CLIPD_TEST_STATE" != config-missing ]; then
      printf 'remoteforward %s/.clipd/socket %s/daemon\n' "$CLIPD_TEST_HOME" "$CLIPD_TEST_HOME"
    fi
    printf 'controlmaster auto\ncontrolpersist yes\ncontrolpath %s/.ssh/clipd-0000000000000000000000000000000000000000\n' "$CLIPD_TEST_HOME"
    ;;
  *'-O check'*) [ "$CLIPD_TEST_STATE" = existing-master ] || [ "$CLIPD_TEST_STATE" = stop-failure ] ;;
  *'-O exit'*)
    case "$*" in *'/.ssh/clipd-'*) [ "$CLIPD_TEST_STATE" != stop-failure ] ;; *) exit 0 ;; esac ;;
  *'-f -N'*) [ "$CLIPD_TEST_STATE" != start-failure ] ;;
  *) printf 'unexpected interactive or forwarding command\n' >&2; exit 99 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fake), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("CLIPD_TEST_HOME", dir)
			t.Setenv("CLIPD_TEST_STATE", state)
			var out, errOut bytes.Buffer
			e := &env{stdout: &out, stderr: &errOut, getenv: func(string) string { return "" }}
			got := cmdReconnect(ctx, e, &globalOptions{configPath: cfg}, []string{"cole@debian"})
			if (got == exitOK) != (state == "success" || state == "existing-master") {
				t.Fatalf("code=%d, stdout=%s, stderr=%s", got, out.String(), errOut.String())
			}
			calls, _ := os.ReadFile(log)
			if state == "daemon-missing" {
				if len(calls) != 0 {
					t.Fatal("SSH ran despite unavailable daemon")
				}
				return
			}
			if !strings.Contains(string(calls), "-O exit") {
				t.Fatal("private master was not closed")
			}
			for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if !strings.HasSuffix(line, "sh -s") && !strings.HasPrefix(line, "-G ") && !strings.Contains(line, "-O exit") && !strings.Contains(line, "-O check") && !strings.HasPrefix(line, "-f -N ") {
					t.Fatalf("unexpected SSH command: %s", calls)
				}
			}
			if (state == "success" || state == "existing-master") && !strings.Contains(out.String(), "Forward restored. Connect normally: ssh cole@debian") {
				t.Fatalf("did not return repair instructions: %s", out.String())
			}
			if state == "config-missing" || state == "config-failure" {
				if strings.Count(string(calls), "sh -s") != 1 {
					t.Fatalf("mutated remote socket despite bad config: %s", calls)
				}
			}
		})
	}
}
