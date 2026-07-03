package vm

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon-agent/client"
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

	vm, err := h.resolveRunningVM(ctx, conf, "reseed", ref)
	if err != nil {
		return err
	}
	return reseedVM(ctx, vm, regenMachineID)
}

// reseedAfterResume re-inspects then fires the best-effort reseed. Clone/Restore return a
// *types.VM built in-process that never passed through ToVM, so its VsockSocket is zero;
// pairing refresh with signal keeps a caller from silently no-op-ing on a stale value.
func (h Handler) reseedAfterResume(ctx context.Context, hyper hypervisor.Hypervisor, vm *types.VM, regenMachineID bool) {
	signalReseed(ctx, refreshVM(ctx, hyper, vm), regenMachineID)
}

// reseedVM pushes fresh entropy and a CRNG reseed order over vsock. Only a failed
// dial is retried — the guest agent re-listens shortly after a clone/restore resume;
// once a live agent answers, its reply (success, version-skew rejection, or failure)
// is final, so an old agent isn't billed the whole retry budget.
func reseedVM(ctx context.Context, vm *types.VM, regenMachineID bool) error {
	if vm.VsockSocket == "" {
		return fmt.Errorf("reseed: %w (recreate the VM to enable agent reseed)", ErrVsockNotConfigured)
	}
	entropy := make([]byte, reseedEntropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("reseed: generate entropy: %w", err)
	}

	var dialErr error
	for attempt := 1; attempt <= reseedMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := dialHybridVsock(ctx, vm.VsockSocket, hypervisor.VsockAgentPort)
		if err != nil {
			dialErr = err
			if attempt < reseedMaxAttempts {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(reseedRetryDelay):
				}
			}
			continue
		}
		err = client.Reseed(ctx, conn, entropy, regenMachineID)
		conn.Close() //nolint:errcheck,gosec
		if err != nil {
			return fmt.Errorf("reseed: %w", err)
		}
		return nil
	}
	return fmt.Errorf("reseed: dial agent: %w", dialErr)
}

// signalReseed is the non-fatal wrapper for clone/restore paths: it reports whether a reseed
// was attempted (false = skipped for a Windows guest or a legacy VM without vsock) and never
// fails the calling command on agent version skew.
func signalReseed(ctx context.Context, vm *types.VM, regenMachineID bool) bool {
	logger := log.WithFunc("cmd.vm.reseed")
	if vm.Config.Windows {
		logger.Debug(ctx, "skip reseed signal: Windows guest")
		return false
	}
	if vm.VsockSocket == "" {
		// Not Debug: a legacy VM silently keeps the snapshot's CRNG state.
		logger.Warnf(ctx, "skip reseed for %s: no vsock; this clone keeps the snapshot's CRNG state — recreate the VM to enable the agent", vm.ID)
		return false
	}
	if err := reseedVM(ctx, vm, regenMachineID); err != nil {
		logger.Warnf(ctx, "reseed signal failed (agent >= v0.1.6 required): %v; run 'cocoon vm reseed %s' after fixing", err, vm.ID)
	}
	return true
}

// refreshVM re-inspects to recover runtime-only fields (VsockSocket, PID, SocketPath) that
// ToVM sets but Clone/Restore's in-process return value lacks; keeps the original on error.
func refreshVM(ctx context.Context, hyper hypervisor.Hypervisor, vm *types.VM) *types.VM {
	info, err := hyper.Inspect(ctx, vm.ID)
	if err != nil {
		log.WithFunc("cmd.vm.reseed").Debugf(ctx, "refresh VM %s before reseed: %v", vm.ID, err)
		return vm
	}
	return info
}
