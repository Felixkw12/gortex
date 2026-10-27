package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDaemonShutdownCleanupFailureIsWarningAndRemovesRuntime(t *testing.T) {
	ctrl := &fakeController{shutdownErr: errors.New("backend close failed")}
	_, socket := newDaemon(t, ctrl)
	pidFile := PIDFilePath()
	c, err := DialTo(socket, Handshake{Mode: ModeControl})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	resp, err := c.Control(ControlShutdown, nil)
	require.NoError(t, err)
	require.True(t, resp.OK, "shutdown is accepted despite cleanup errors")
	require.Empty(t, resp.ErrorCode)
	require.Equal(t, []string{"backend close failed"}, resp.Warnings)
	require.Eventually(t, func() bool { return !fileExists(socket) && !fileExists(pidFile) }, 3*time.Second, time.Millisecond)
}

type shutdownFailedAckConn struct {
	net.Conn
	attempted chan struct{}
}

func (c *shutdownFailedAckConn) Write([]byte) (int, error) {
	close(c.attempted)
	return 0, errors.New("client disappeared before acknowledgement")
}

func TestDaemonShutdownCleanupFailureAndFailedAckStillRemoveRuntime(t *testing.T) {
	srv, socket := newDaemon(t, &fakeController{shutdownErr: errors.New("lifecycle close failed after joining")})
	pidFile := PIDFilePath()
	server, client := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	failed := &shutdownFailedAckConn{Conn: server, attempted: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveControl(failed, bufio.NewReader(server), nil)
	}()
	require.NoError(t, client.SetWriteDeadline(time.Now().Add(3*time.Second)))
	require.NoError(t, WriteJSONLine(client, ControlRequest{Kind: ControlShutdown}))
	select {
	case <-failed.attempted:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown acknowledgement was not attempted")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("failed acknowledgement stranded the control handler")
	}
	require.Eventually(t, func() bool { return !fileExists(socket) && !fileExists(pidFile) }, 3*time.Second, time.Millisecond)
}

func TestControlShutdownWarningsAreOptionalProtocolFields(t *testing.T) {
	old, err := json.Marshal(ControlResponse{OK: true})
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(old))
	var receipt ControlResponse
	require.NoError(t, json.Unmarshal([]byte(`{"ok":true,"warnings":["cleanup failed"]}`), &receipt))
	require.True(t, receipt.OK)
	require.Equal(t, []string{"cleanup failed"}, receipt.Warnings)
}
