//go:build linux

package network

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

const (
	// TAPTxQueueLen absorbs traffic bursts (especially UDP) without dropping; the kernel default of 1000 is too small for VM workloads.
	TAPTxQueueLen = 10000

	// GROMaxSize matches the maximum virtio-net segment size so the kernel aggregates inbound packets before CH reads them.
	GROMaxSize = 65536
)

// CreateTAP adds a multi-queue TAP sized for numQueues virtio-net queues and returns its index, then closes the kernel fds (CH/QEMU reopen it by name).
func CreateTAP(name string, numQueues int) (int, error) {
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
		return 0, fmt.Errorf("add tap %s: %w", name, err)
	}
	for _, fd := range tap.Fds {
		_ = fd.Close()
	}
	// netlink's post-TUNSETIFF index lookup drops its error, so 0 can land here.
	index := tap.Attrs().Index
	if index == 0 {
		return 0, fmt.Errorf("resolve index of tap %s", name)
	}
	return index, nil
}

// TuneTAP applies best-effort performance tuning to a TAP device.
func TuneTAP(link netlink.Link) error {
	if err := netlink.LinkSetTxQLen(link, TAPTxQueueLen); err != nil {
		return err
	}
	return netlink.LinkSetGROMaxSize(link, GROMaxSize)
}
