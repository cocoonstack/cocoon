//go:build linux

package network

import (
	"fmt"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const (
	// tapTxQueueLen absorbs traffic bursts (especially UDP) without dropping; the kernel default of 1000 is too small for VM workloads.
	tapTxQueueLen = 10000

	// groMaxSize matches the maximum virtio-net segment size so the kernel aggregates inbound packets before CH reads them.
	groMaxSize = 65536
)

// CreateTAP adds a multi-queue TAP sized for numQueues virtio-net queues and returns its index, then closes the kernel fds (CH/QEMU reopen it by name).
// TUNSETIFF carries no index, so netlink resolves one after the ioctl and swallows a failure to; an unresolved index must not reach a caller as valid.
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
	index := tap.Attrs().Index
	if index == 0 {
		return 0, fmt.Errorf("resolve index of tap %s", name)
	}
	return index, nil
}

// AttachBridgeUp enslaves a TAP to a bridge, applies the queue and GRO tuning and brings it up, in a single RTM_SETLINK.
// Every netlink write serializes on the kernel's node-wide rtnl lock, so on a dense fill the op count, not the work, is the cost.
func AttachBridgeUp(tapIndex, bridgeIndex, mtu int) error {
	req := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(tapIndex)
	msg.Flags = unix.IFF_UP
	msg.Change = unix.IFF_UP
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.IFLA_MASTER, nl.Uint32Attr(uint32(bridgeIndex))))
	req.AddData(nl.NewRtAttr(unix.IFLA_TXQLEN, nl.Uint32Attr(tapTxQueueLen)))
	req.AddData(nl.NewRtAttr(unix.IFLA_GRO_MAX_SIZE, nl.Uint32Attr(groMaxSize)))
	if mtu > 0 {
		req.AddData(nl.NewRtAttr(unix.IFLA_MTU, nl.Uint32Attr(uint32(mtu))))
	}
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("attach tap %d to bridge %d: %w", tapIndex, bridgeIndex, err)
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
