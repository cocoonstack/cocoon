package main

import (
	"context"
	"os"

	"github.com/cocoonstack/cocoon/cmd"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
)

func main() {
	ctx := context.Background()
	// Internal: console relay mode for Firecracker PTY bridge.
	// Started as a background process by FC launchProcess.
	if firecracker.IsRelayMode() {
		firecracker.RunRelay(ctx)
		return
	}
	if err := cmd.Execute(ctx); err != nil {
		os.Exit(1)
	}
}
