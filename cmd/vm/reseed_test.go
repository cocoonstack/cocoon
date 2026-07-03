package vm

import (
	"context"
	"errors"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestSignalReseed_SkipsWindows(t *testing.T) {
	// VsockSocket is set: a regressed Windows guard would fall through to a dial instead of skipping.
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
