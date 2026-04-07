package main

import (
	"os"

	"github.com/cocoonstack/cocoon/cmd"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
)

func main() {
	// Internal: console relay mode for Firecracker PTY bridge.
	// Started as a background process by FC launchProcess.
	if firecracker.IsRelayMode() {
		firecracker.RunRelay()
		return
	}
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
