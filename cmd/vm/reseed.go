package vm

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon-agent/client"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

const (
	reseedEntropyBytes   = 32
	reseedMaxAttempts    = 3
	reseedRetryDelay     = 2 * time.Second
	reseedAttemptTimeout = 4 * time.Second
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

// reseedAfterResume fires the best-effort reseed, re-inspecting only when the in-process record lacks VsockSocket — a zero value would silently no-op.
// It hands the reseed to a detached child by default: the vsock dial waits out the guest's post-resume wakeup (tens of ms, growing with snapshot age), which would otherwise sit on every clone/restore critical path.
func (h Handler) reseedAfterResume(ctx context.Context, conf *config.Config, hyper hypervisor.Hypervisor, vm *types.VM, regenMachineID bool) {
	if vm.VsockSocket == "" {
		vm = refreshVM(ctx, hyper, vm)
	}
	if vm.VsockSocket == "" || vm.Config.Windows || !detachReseed(ctx, conf, vm, regenMachineID) {
		signalReseed(ctx, vm, regenMachineID)
	}
}

// reseedVM pushes fresh entropy and a CRNG reseed order over vsock; only a failed dial retries (the agent re-listens shortly after resume) — a live agent's reply is final.
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
		attemptCtx, cancel := context.WithTimeout(ctx, reseedAttemptTimeout)
		conn, err := dialHybridVsock(attemptCtx, vm.VsockSocket, hypervisor.VsockAgentPort)
		if err != nil {
			cancel()
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
		err = client.Reseed(attemptCtx, conn, entropy, regenMachineID)
		conn.Close() //nolint:errcheck,gosec
		cancel()
		if err != nil {
			return fmt.Errorf("reseed: %w", err)
		}
		return nil
	}
	return fmt.Errorf("reseed: dial agent: %w", dialErr)
}

// detachReseed re-execs `cocoon vm reseed` as a session-detached child and reports whether the hand-off started; resolved dirs travel as flags so file- or flag-configured parents behave like env-configured ones.
func detachReseed(ctx context.Context, conf *config.Config, vm *types.VM, regenMachineID bool) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	args := []string{"--root-dir", conf.RootDir, "--run-dir", conf.RunDir, "--log-dir", conf.LogDir, "vm", "reseed"}
	if regenMachineID {
		args = append(args, "--machine-id")
	}
	c := exec.Command(exe, append(args, vm.ID)...) //nolint:gosec // self re-exec: path from os.Executable, args are internal flags/IDs
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		log.WithFunc("cmd.vm.reseed").Warnf(ctx, "detached reseed spawn failed, reseeding inline: %v", err)
		return false
	}
	go c.Wait() //nolint:errcheck
	return true
}

// signalReseed is the non-fatal clone/restore wrapper: reports whether a reseed was attempted (false = Windows or no vsock) and never fails the calling command.
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

// refreshVM re-inspects to recover runtime-only fields Clone/Restore's in-process return value lacks; keeps the original on error.
func refreshVM(ctx context.Context, hyper hypervisor.Hypervisor, vm *types.VM) *types.VM {
	info, err := hyper.Inspect(ctx, vm.ID)
	if err != nil {
		log.WithFunc("cmd.vm.reseed").Debugf(ctx, "refresh VM %s before reseed: %v", vm.ID, err)
		return vm
	}
	return info
}
