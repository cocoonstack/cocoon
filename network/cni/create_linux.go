package cni

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"

	cns "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/cocoonstack/cocoon/utils"
)

// createNetns creates a named network namespace at /run/netns/{name}.
func createNetns(name string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer origNS.Close() //nolint:errcheck

	ns, err := netns.NewNamed(name)
	if err != nil {
		return fmt.Errorf("create netns %s: %w", name, err)
	}
	_ = ns.Close()

	if err := netns.Set(origNS); err != nil {
		return fmt.Errorf("restore netns: %w", err)
	}
	return nil
}

// deleteNetns removes a named network namespace, retrying briefly after VM exit.
func deleteNetns(ctx context.Context, name string) error {
	return utils.WaitFor(ctx, time.Second, 100*time.Millisecond, func() (bool, error) { //nolint:mnd
		err := netns.DeleteNamed(name)
		return err == nil || os.IsNotExist(err), nil
	})
}

// setupTCRedirect wires ifName <-> tapName inside the target netns and returns ifName's MAC.
func setupTCRedirect(nsPath, ifName, tapName string, queues int, overrideMAC string) (string, error) {
	var mac string
	err := cns.WithNetNSPath(nsPath, func(_ cns.NetNS) error {
		var nsErr error
		mac, nsErr = tcRedirectInNS(ifName, tapName, queues, overrideMAC)
		return nsErr
	})
	return mac, err
}

// tcRedirectInNS runs inside the target netns.
func tcRedirectInNS(ifName, tapName string, queues int, overrideMAC string) (string, error) {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return "", fmt.Errorf("find %s: %w", ifName, err)
	}

	// Recovery restores the persisted MAC before traffic flows again.
	if overrideMAC != "" {
		hwAddr, parseErr := net.ParseMAC(overrideMAC)
		if parseErr != nil {
			return "", fmt.Errorf("parse MAC %s: %w", overrideMAC, parseErr)
		}
		if setErr := netlink.LinkSetHardwareAddr(link, hwAddr); setErr != nil {
			return "", fmt.Errorf("set MAC on %s: %w", ifName, setErr)
		}
	}

	// Link attrs are stale after LinkSetHardwareAddr.
	if overrideMAC != "" {
		link, err = netlink.LinkByName(ifName)
		if err != nil {
			return "", fmt.Errorf("re-read link %s: %w", ifName, err)
		}
	}
	mac := link.Attrs().HardwareAddr.String()

	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return "", fmt.Errorf("list addrs on %s: %w", ifName, err)
	}
	for _, addr := range addrs {
		if delErr := netlink.AddrDel(link, &addr); delErr != nil {
			return "", fmt.Errorf("flush addr %s on %s: %w", addr.IPNet, ifName, delErr)
		}
	}

	// CH uses queue pairs (TX+RX): queue_pairs = num_queues / 2.
	// Multi-queue requires queue_pairs > 1, i.e. num_queues > 2.
	queuePairs := max(1, queues/2) //nolint:mnd
	flags := netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_NO_PI
	if queuePairs <= 1 {
		flags |= netlink.TUNTAP_ONE_QUEUE
	} else {
		flags |= netlink.TUNTAP_MULTI_QUEUE_DEFAULTS
	}
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: tapName},
		Mode:      netlink.TUNTAP_MODE_TAP,
		Queues:    queuePairs,
		Flags:     flags,
	}
	if addErr := netlink.LinkAdd(tap); addErr != nil {
		return "", fmt.Errorf("add tap %s: %w", tapName, addErr)
	}
	for _, fd := range tap.Fds {
		_ = fd.Close()
	}
	tapLink, err := netlink.LinkByName(tapName)
	if err != nil {
		return "", fmt.Errorf("find tap %s: %w", tapName, err)
	}

	// Keep tap MTU aligned with the veth.
	if mtu := link.Attrs().MTU; mtu > 0 {
		if mtuErr := netlink.LinkSetMTU(tapLink, mtu); mtuErr != nil {
			return "", fmt.Errorf("set tap %s mtu %d: %w", tapName, mtu, mtuErr)
		}
	}

	for _, l := range []netlink.Link{link, tapLink} {
		if upErr := netlink.LinkSetUp(l); upErr != nil {
			return "", fmt.Errorf("set %s up: %w", l.Attrs().Name, upErr)
		}
	}

	for _, l := range []netlink.Link{link, tapLink} {
		qdisc := &netlink.Ingress{
			QdiscAttrs: netlink.QdiscAttrs{
				LinkIndex: l.Attrs().Index,
				Parent:    netlink.HANDLE_INGRESS,
			},
		}
		if qdiscErr := netlink.QdiscAdd(qdisc); qdiscErr != nil {
			return "", fmt.Errorf("add ingress qdisc on %s: %w", l.Attrs().Name, qdiscErr)
		}
	}

	if err := addTCRedirect(link, tapLink); err != nil {
		return "", fmt.Errorf("redirect %s -> %s: %w", ifName, tapName, err)
	}
	if err := addTCRedirect(tapLink, link); err != nil {
		return "", fmt.Errorf("redirect %s -> %s: %w", tapName, ifName, err)
	}
	return mac, nil
}

// addTCRedirect redirects all ingress packets from one link to another.
func addTCRedirect(from, to netlink.Link) error {
	filter := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: from.Attrs().Index,
			Parent:    netlink.HANDLE_INGRESS,
			Priority:  1,
			Protocol:  syscall.ETH_P_ALL,
		},
		Sel: &netlink.TcU32Sel{
			Flags: netlink.TC_U32_TERMINAL,
			Keys: []netlink.TcU32Key{
				{Mask: 0x0, Val: 0x0, Off: 0, OffMask: 0x0},
			},
		},
		Actions: []netlink.Action{
			&netlink.MirredAction{
				ActionAttrs:  netlink.ActionAttrs{Action: netlink.TC_ACT_STOLEN},
				MirredAction: netlink.TCA_EGRESS_REDIR,
				Ifindex:      to.Attrs().Index,
			},
		},
	}
	return netlink.FilterAdd(filter)
}
