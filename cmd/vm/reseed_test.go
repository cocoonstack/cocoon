package vm

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocoonstack/cocoon-agent/agent"
	"github.com/cocoonstack/cocoon/types"
)

func TestSignalReseed_SkipsWindows(t *testing.T) {
	vm := &types.VM{ID: "vm1", Config: types.VMConfig{Config: types.Config{Windows: true}}, VsockSocket: "/tmp/whatever.sock"}
	if signalReseed(t.Context(), vm, true) {
		t.Error("attempted reseed on a Windows guest, want skip")
	}
}

func TestSignalReseed_SkipsNoVsock(t *testing.T) {
	vm := &types.VM{ID: "vm1"}
	if signalReseed(t.Context(), vm, true) {
		t.Error("attempted reseed with no vsock, want skip")
	}
}

func TestReseedVM_NoVsock(t *testing.T) {
	vm := &types.VM{ID: "vm1"}
	err := reseedVM(t.Context(), vm, false)
	if !errors.Is(err, ErrVsockNotConfigured) {
		t.Fatalf("got err=%v, want ErrVsockNotConfigured", err)
	}
}

func TestReseedVM_CtxCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	vm := &types.VM{ID: "vm1", VsockSocket: "/tmp/does-not-matter.sock"}
	if err := reseedVM(ctx, vm, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("got err=%v, want context.Canceled", err)
	}
}

func TestReseedVM_AgentRejectionNotRetried(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rsd")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	var accepts atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			br := bufio.NewReader(conn)
			_, _ = br.ReadString('\n')
			_, _ = io.WriteString(conn, "OK 1\n")
			_, _ = br.ReadString('\n')
			_ = agent.NewEncoder(conn).Encode(agent.Message{Type: agent.MsgError, Message: "version skew"})
			_ = conn.Close()
		}
	}()

	vm := &types.VM{ID: "vm1", VsockSocket: sock}
	err = reseedVM(t.Context(), vm, false)
	if err == nil || !strings.Contains(err.Error(), "version skew") {
		t.Fatalf("err = %v, want it to carry the agent's rejection", err)
	}
	_ = ln.Close()
	<-done
	if n := accepts.Load(); n != 1 {
		t.Errorf("agent accepted %d times; a live rejection must not be retried (want 1)", n)
	}
}
