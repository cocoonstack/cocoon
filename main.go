package main

import (
	"context"
	"errors"
	"os"

	"github.com/cocoonstack/cocoon/cmd"
	cmdvm "github.com/cocoonstack/cocoon/cmd/vm"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
)

func main() {
	ctx := context.Background()
	// Console relay mode: this binary is re-exec'd as a background process by FC launchProcess for the PTY bridge.
	if firecracker.IsRelayMode() {
		firecracker.RunRelay(ctx)
		return
	}
	if err := cmd.Execute(ctx); err != nil {
		if exitErr, ok := errors.AsType[*cmdvm.ExecExitError](err); ok {
			os.Exit(exitErr.Code)
		}
		os.Exit(1)
	}
}
