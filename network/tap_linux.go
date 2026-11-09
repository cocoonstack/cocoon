//go:build linux

package network

import (
	"fmt"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	// TAPTxQueueLen absorbs UDP bursts; the kernel default of 1000 is too small.
	TAPTxQueueLen = 10000

	// GROMaxSize matches the maximum virtio-net segment size.
	GROMaxSize = 65536
)

// CreateTAP adds a persistent TAP with numQueues/2 queue pairs and returns its index; the VMM reopens it by name.
func CreateTAP(name string, numQueues int) (int, error) {
	// The VMM opens a multi-queue TAP with IFF_MULTI_QUEUE, so the flags must match.
	queuePairs := max(1, numQueues/2)
	flags := netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_NO_PI
	if queuePairs <= 1 {
		flags |= netlink.TUNTAP_ONE_QUEUE
	} else {
		flags |= netlink.TUNTAP_MULTI_QUEUE_DEFAULTS
	}
	tap := &netlink.Tuntap{
		Name:   name,
		Mode:   netlink.TUNTAP_MODE_TAP,
		Queues: queuePairs,
		Flags:  flags,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return 0, fmt.Errorf("add tap %s: %w", name, err)
	}
	// Netlink drops the index-lookup error; on index 0, clear persistence so the TAP dies with the fds.
	index := tap.Attrs().Index
	if index == 0 {
		_ = unix.IoctlSetInt(int(tap.Fds[0].Fd()), unix.TUNSETPERSIST, 0)
	}
	for _, fd := range tap.Fds {
		_ = fd.Close()
	}
	if index == 0 {
		return 0, fmt.Errorf("resolve index of tap %s", name)
	}
	if queuePairs > 1 {
		_ = netlink.QdiscReplace(&netlink.GenericQdisc{LinkIndex: index, Parent: netlink.HANDLE_ROOT, QdiscType: "noqueue"})
	}
	return index, nil
}
