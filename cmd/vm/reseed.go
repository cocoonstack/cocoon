package vm

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon-agent/client"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

const (
	reseedEntropyBytes = 32
	reseedMaxAttempts  = 3
	reseedRetryDelay   = 2 * time.Second
)

func (h Handler) Reseed(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	ref := args[0]
	regenMachineID, _ := cmd.Flags().GetBool("machine-id")

	hyper, err := cmdcore.FindHypervisor(ctx, conf, ref)
	if err != nil {
		return fmt.Errorf("reseed: %w", err)
	}
	vm, err := hyper.Inspect(ctx, ref)
	if err != nil {
		return fmt.Errorf("reseed: inspect: %w", err)
	}
	if vm.State != types.VMStateRunning {
		return fmt.Errorf("reseed: %w", hypervisor.ErrNotRunning)
	}
	return reseedVM(ctx, vm, regenMachineID)
}

// reseedVM pushes fresh entropy and a CRNG reseed order over vsock, retrying because the
// guest agent re-listens shortly after a clone/restore resume.
func reseedVM(ctx context.Context, vm *types.VM, regenMachineID bool) error {
	if vm.VsockSocket == "" {
		return fmt.Errorf("reseed: %w (recreate the VM to enable agent reseed)", ErrVsockNotConfigured)
	}
	entropy := make([]byte, reseedEntropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("reseed: generate entropy: %w", err)
	}

	var err error
	for attempt := 1; attempt <= reseedMaxAttempts; attempt++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		var conn io.ReadWriteCloser
		if conn, err = dialHybridVsock(ctx, vm.VsockSocket, hypervisor.VsockAgentPort); err == nil {
			err = client.Reseed(ctx, conn, entropy, regenMachineID)
			conn.Close() //nolint:errcheck,gosec
			if err == nil {
				return nil
			}
		}
		if attempt < reseedMaxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(reseedRetryDelay):
			}
		}
	}
	return fmt.Errorf("reseed: %w", err)
}

// signalReseed is the non-fatal wrapper called from clone/restore paths: skips silently on
// Windows guests (agent verb is Linux-only) or legacy VMs without vsock, and never fails the
// calling command on agent version skew.
func signalReseed(ctx context.Context, vm *types.VM, regenMachineID bool) {
	logger := log.WithFunc("cmd.vm.reseed")
	if vm.Config.Windows {
		logger.Debug(ctx, "skip reseed signal: Windows guest")
		return
	}
	if vm.VsockSocket == "" {
		logger.Debug(ctx, "skip reseed signal: vsock not configured")
		return
	}
	if err := reseedVM(ctx, vm, regenMachineID); err != nil {
		logger.Warnf(ctx, "reseed signal failed (agent >= v0.1.6 required): %v; run 'cocoon vm reseed %s' after fixing", err, vm.ID)
	}
}
