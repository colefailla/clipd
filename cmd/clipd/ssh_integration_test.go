package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colefailla/clipd/internal/clipboard"
	"github.com/colefailla/clipd/internal/server"
)

// Opt in on a disposable runner. Keys, config, sockets and received files are
// temporary. ssh -N opens no remote shell and neither daemon is installed.
func TestSSHForwardLifecycle(t *testing.T) {
	if os.Getenv("CLIPD_TEST_SSHD") != "1" {
		t.Skip("set CLIPD_TEST_SSHD=1 to run a temporary sshd")
	}
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Fatal(err)
	}
	sshd, err = filepath.Abs(sshd)
	if err != nil {
		t.Fatal(err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "clipd-ssh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	run := func(name string, args ...string) []byte {
		t.Helper()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		return out
	}
	hostKey := filepath.Join(dir, "host_key")
	key := filepath.Join(dir, "client_key")
	for _, path := range []string{hostKey, key} {
		run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path)
	}
	// Reserve an ephemeral port; sshd must bind it after the reservation closes.
	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portListener.Addr().(*net.TCPAddr).Port
	portListener.Close()
	remoteDir := filepath.Join(dir, "remote")
	if err := os.Mkdir(remoteDir, 0700); err != nil {
		t.Fatal(err)
	}
	remoteSocket := filepath.Join(remoteDir, "socket")
	localSocket := filepath.Join(dir, "receiver")
	config := fmt.Sprintf(`ListenAddress 127.0.0.1
Port %d
HostKey %s
PidFile %s
AuthorizedKeysFile %s.pub
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
PermitRootLogin prohibit-password
AllowUsers %s
AllowStreamLocalForwarding yes
StreamLocalBindUnlink no
StreamLocalBindMask 0177
LogLevel VERBOSE
`, port, hostKey, filepath.Join(dir, "pid"), key, current.Username)
	configPath := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "sshd.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	daemon := exec.CommandContext(ctx, sshd, "-D", "-e", "-f", configPath)
	daemon.Stdout = log
	daemon.Stderr = log
	if err := daemon.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	defer func() {
		daemon.Process.Kill()
		daemon.Wait()
		log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("sshd: %s", data)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("temporary sshd did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	fake := &clipboard.Fake{}
	srv, err := server.New(server.Options{Clipboard: fake, DropDir: filepath.Join(dir, "received"), MaxPayload: 1024, MaxTransfer: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := server.Listen(localSocket)
	if err != nil {
		t.Fatal(err)
	}
	receiverCtx, stopReceiver := context.WithCancel(ctx)
	defer stopReceiver()
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- srv.Serve(receiverCtx, ln) }()
	defer func() { stopReceiver(); <-receiverDone }()
	block, err := sshBlockFor("fixture", current.Username, remoteSocket, localSocket)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig := fmt.Sprintf("Host fixture\n HostName 127.0.0.1\n Port %d\n User %s\n IdentityFile %s\n IdentitiesOnly yes\n BatchMode yes\n StrictHostKeyChecking no\n UserKnownHostsFile /dev/null\n GlobalKnownHostsFile /dev/null\n ExitOnForwardFailure yes\n", port, current.Username, key) + block
	clientPath := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(clientPath, []byte(clientConfig), 0600); err != nil {
		t.Fatal(err)
	}
	startForward := func() func() {
		t.Helper()
		f, err := os.Create(filepath.Join(dir, "ssh.log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, "ssh", "-F", clientPath, "-N", "fixture")
		cmd.Stdout = f
		cmd.Stderr = f
		if err := cmd.Start(); err != nil {
			f.Close()
			t.Fatal(err)
		}
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				cmd.Process.Kill()
				cmd.Wait()
				f.Close()
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := server.Ping("unix", remoteSocket); err == nil {
				return stop
			}
			if time.Now().After(deadline) {
				stop()
				data, _ := os.ReadFile(filepath.Join(dir, "ssh.log"))
				t.Fatalf("forward failed: %s", data)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	stopForward := startForward()
	client := localTransport(t)
	shell := shellFunction(client, remoteSocket, true)
	cmd := exec.CommandContext(ctx, "sh", "-c", shell+"\nprintf 'through ssh' | clipd\nprintf 'file through ssh' | clipd drop --name received.txt\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("transfer: %v %s", err, out)
	}
	if string(fake.Data()) != "through ssh" {
		t.Fatal("clipboard payload mismatch")
	}
	data, err := os.ReadFile(filepath.Join(dir, "received", "received.txt"))
	if err != nil || string(data) != "file through ssh" {
		t.Fatal("drop payload mismatch")
	}
	info, err := os.Stat(remoteSocket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket mode: %v %v", info, err)
	}
	stopForward()
	// Wait for sshd to release its listener while retaining the socket file.
	deadline = time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", remoteSocket, 100*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("forward listener stayed alive")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(remoteSocket); err != nil {
		t.Fatalf("expected leftover socket: %v", err)
	}
	retryCtx, stopRetry := context.WithTimeout(ctx, 3*time.Second)
	out, err := exec.CommandContext(retryCtx, "ssh", "-F", clientPath, "-N", "fixture").CombinedOutput()
	stopRetry()
	if err == nil || !strings.Contains(string(out), "forwarding failed") {
		t.Fatalf("stale socket did not refuse new forward: %v %s", err, out)
	}
	probe := exec.CommandContext(ctx, "sh", "-c", "_clipd_sock="+shellQuote(remoteSocket)+"\n"+staleSocketScript(client)+"\n[ \"$_clipd_stale\" = 1 ]")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("cleanup: %v %s", err, out)
	}
	stopForward = startForward()
	defer stopForward()
	// A live SSH forward must survive when the receiving daemon is unavailable.
	stopReceiver()
	deadline = time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(localSocket); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("receiver did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	probe = exec.CommandContext(ctx, "sh", "-c", "_clipd_sock="+shellQuote(remoteSocket)+"\n"+staleSocketScript(client)+"\n[ \"$_clipd_stale\" = 0 ]")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("live forward cleanup: %v %s", err, out)
	}
	if _, err := os.Stat(remoteSocket); err != nil {
		t.Fatalf("live forward removed: %v", err)
	}
}
