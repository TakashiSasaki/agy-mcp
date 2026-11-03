//go:build linux || darwin

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

type servingLogWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *servingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), "serving over stdio") {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func (w *servingLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// Without serve's context cancellation check, a normal systemd stop exits 1
// and emits "stdio serve: context canceled", which the tunnel logs as a crash.
func TestStdioServerSignalShutdownSucceeds(t *testing.T) {
	t.Parallel()
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			t.Parallel()
			bin, err := buildBinary()
			if err != nil {
				t.Fatal(err)
			}
			agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{})
			cmd := exec.CommandContext(t.Context(), bin)
			cmd.Env = append(os.Environ(), "AGY_MCP_AGY_PATH="+agy,
				"AGY_MCP_STATE_DIR="+t.TempDir(), "AGY_MCP_USAGE_INTERVAL=0")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stdin.Close() }()
			logs := &servingLogWriter{ready: make(chan struct{})}
			cmd.Stderr, cmd.Stdout = logs, io.Discard
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill() }()
			wait := make(chan error, 1)
			go func() { wait <- cmd.Wait() }()
			select {
			case <-logs.ready:
			case err := <-wait:
				t.Fatalf("server exited before readiness: %v; %s", err, logs)
			case <-time.After(10 * time.Second):
				t.Fatalf("server did not become ready: %s", logs)
			}
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-wait:
				if err != nil || strings.Contains(logs.String(), "stdio serve:") {
					t.Fatalf("normal signal shutdown reported failure: %v; %s", err, logs)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("server did not stop: %s", logs)
			}
		})
	}
}
