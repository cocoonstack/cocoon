//go:build linux

package network

import (
	"github.com/vishvananda/netlink"
)

const (
	// tapTxQueueLen increases the default TX queue length (1000) to
	// absorb traffic bursts without dropping, especially for UDP.
	tapTxQueueLen = 10000
	// groMaxSize is the GRO aggregation ceiling in bytes.
	// 65536 matches the maximum virtio-net segment size, allowing
	// the kernel to aggregate inbound packets before CH reads them.
	groMaxSize = 65536
)

// TuneTAP applies performance tuning to a TAP device after creation:
//   - txqueuelen 10000 to absorb bursts
//   - GRO max size 64K to aggregate inbound packets before CH reads them
func TuneTAP(link netlink.Link) error {
	if err := netlink.LinkSetTxQLen(link, tapTxQueueLen); err != nil {
		return err
	}
	return netlink.LinkSetGROMaxSize(link, groMaxSize)
}
