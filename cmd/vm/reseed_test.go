package vm

import (
	"context"
	"errors"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestSignalReseed_SkipsWindows(t *testing.T) {
	vm := &types.VM{ID: "vm1", Config: types.VMConfig{Config: types.Config{Windows: true}}, VsockSocket: "/tmp/whatever.sock"}
	signalReseed(t.Context(), vm, true)
}

func TestSignalReseed_SkipsNoVsock(t *testing.T) {
	vm := &types.VM{ID: "vm1"}
	signalReseed(t.Context(), vm, true)
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
