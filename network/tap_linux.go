//go:build linux

package network

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

const (
	// tapTxQueueLen absorbs traffic bursts (especially UDP) without
	// dropping; the kernel default of 1000 is too small for VM workloads.
	tapTxQueueLen = 10000

	// groMaxSize matches the maximum virtio-net segment size, allowing
	// the kernel to aggregate inbound packets before CH reads them.
	groMaxSize = 65536
)

// CreateTAP adds a multi-queue TAP sized for numQueues virtio-net queues, then closes the kernel fds (CH/QEMU reopen it by name).
func CreateTAP(name string, numQueues int) error {
	// queue_pairs = num_queues / 2 (TX+RX pair); multi-queue needs >1 and must match the VMM's IFF_MULTI_QUEUE expectation.
	queuePairs := max(1, numQueues/2) //nolint:mnd
	flags := netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_NO_PI
	if queuePairs <= 1 {
		flags |= netlink.TUNTAP_ONE_QUEUE
	} else {
		flags |= netlink.TUNTAP_MULTI_QUEUE_DEFAULTS
	}
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		Mode:      netlink.TUNTAP_MODE_TAP,
		Queues:    queuePairs,
		Flags:     flags,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("add tap %s: %w", name, err)
	}
	for _, fd := range tap.Fds {
		_ = fd.Close()
	}
	return nil
}

// TuneTAP applies best-effort performance tuning to a TAP device.
func TuneTAP(link netlink.Link) error {
	if err := netlink.LinkSetTxQLen(link, tapTxQueueLen); err != nil {
		return err
	}
	return netlink.LinkSetGROMaxSize(link, groMaxSize)
}
