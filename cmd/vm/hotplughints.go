package vm

import (
	"fmt"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/extend/netresize"
)

const (
	pciRescanHint = "echo 1 > /sys/bus/pci/rescan"
	// pciDiskRemoveHint: the block name is only known inside the guest, so the placeholder stays.
	pciDiskRemoveHint = "echo 1 > /sys/block/<vdX>/device/../remove"
	pciNICRemoveHint  = "echo 1 > /sys/class/net/eth%d/device/../remove"
)

// isFirecracker reports whether typ names the Firecracker backend.
func isFirecracker(typ string) bool {
	return typ == string(config.HypervisorFirecracker)
}

func fcNetHints(res netresize.Result) []string {
	var hints []string
	if len(res.Added) > 0 {
		hints = append(hints, pciRescanHint)
	}
	for _, nic := range res.Removed {
		hints = append(hints, fmt.Sprintf(pciNICRemoveHint, nic.Index))
	}
	return hints
}

func printGuestHints(hints []string) {
	if len(hints) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("Run inside the guest (PCI transport: the guest discovers and releases devices itself):")
	fmt.Println()
	for _, h := range hints {
		fmt.Printf("  %s\n", h)
	}
	fmt.Println()
}
