package main

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/testenv"
)

// This is a real rejected handshake, including availability probes that close
// before sending a frame. It never starts or touches a user daemon.
func serveMismatchAck(t *testing.T, ack daemon.HandshakeAck) string {
	t.Helper()
	socket := filepath.Join(testenv.ShortTempDir(t), "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active := make(map[net.Conn]struct{})
	var conns sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			active[c] = struct{}{}
			mu.Unlock()
			conns.Add(1)
			go func() {
				defer conns.Done()
				defer func() { _ = c.Close(); mu.Lock(); delete(active, c); mu.Unlock() }()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := bufio.NewReader(c).ReadBytes('\n'); err == nil {
					_ = daemon.WriteJSONLine(c, ack)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-stopped
		mu.Lock()
		for c := range active {
			_ = c.Close()
		}
		mu.Unlock()
		conns.Wait()
	})
	return socket
}

func TestRunMCPMismatchNeverCreatesEmbeddedStore(t *testing.T) {
	zero, two := 0, daemon.ProtocolVersion+1
	for _, tc := range []struct {
		name             string
		protocol         *int
		version, message string
	}{
		{"older", &zero, "v0.64.10", "daemon expects protocol 0, client sent 1"},
		{"legacy", nil, "", "daemon expects protocol 0, client sent 1"},
		{"newer protocol", &two, "v0.64.10", "daemon expects protocol 2, client sent 1"},
		{"newer release", &zero, "v0.64.13", "daemon expects protocol 0, client sent 1"},
	} {
		for _, allow := range []bool{false, true} {
			name := tc.name + "/deny"
			if allow {
				name = tc.name + "/allow"
			}
			t.Run(name, func(t *testing.T) {
				testenv.Sandbox(t)
				tmp := redirectTempRoot(t)
				repo := filepath.Join(tmp, "repo")
				if err := os.Mkdir(repo, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, "example.go"), []byte("package example\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Chdir(repo)
				stubUnavailableMCPDaemon(t)
				t.Cleanup(restoreSeams)
				binaryVersion = func() string { return "v0.64.12" }
				isDaemonRunning = daemon.IsRunning
				runningDaemonInfo = dialRunningDaemonIdentity
				probeDaemonAvailability = daemon.ProbeAvailability
				socket := serveMismatchAck(t, daemon.HandshakeAck{ErrorCode: daemon.ErrProtocolMismatch, ErrorMsg: tc.message, DaemonVersion: tc.version, DaemonProtocol: tc.protocol})
				t.Setenv("GORTEX_DAEMON_SOCKET", socket)
				path := config.DefaultGlobalConfigPath()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				value := "false"
				if allow {
					value = "true"
				}
				if err := os.WriteFile(path, []byte("mcp:\n  allow_embedded: "+value+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := &cobra.Command{}
				cmd.SetContext(t.Context())
				err := runMCP(cmd, nil)
				if !errors.Is(err, daemon.ErrProtocolVersionMismatch) {
					t.Fatalf("runMCP = %v", err)
				}
				for _, want := range []string{"protocol", "CLI v0.64.12", "daemon "} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("diagnostic %q missing %q", err, want)
					}
				}
				matches, globErr := filepath.Glob(filepath.Join(tmp, embeddedStoreDirPattern))
				if globErr != nil || len(matches) != 0 {
					t.Fatalf("mismatch allocated embedded store: %v (%v)", matches, globErr)
				}
				if _, err := os.Stat(filepath.Join(repo, ".gortex")); !os.IsNotExist(err) {
					t.Fatalf("mismatch created repository state: %v", err)
				}
			})
		}
	}
}
