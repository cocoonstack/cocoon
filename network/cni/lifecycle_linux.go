package cni

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"runtime"
	"syscall"
	"time"

	cns "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/utils"
)

// netnsDeleteRetryInterval polls for async kernel cleanup of a named netns.
const netnsDeleteRetryInterval = 100 * time.Millisecond

// createNetns creates a named netns at /var/run/netns/{name}.
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

// deleteNetns removes a named netns with retry for async kernel cleanup.
func deleteNetns(ctx context.Context, name string) error {
	return utils.WaitFor(ctx, time.Second, netnsDeleteRetryInterval, func() (bool, error) {
		err := netns.DeleteNamed(name)
		return err == nil || errors.Is(err, fs.ErrNotExist), nil
	})
}

func tapPresentInNetns(nsPath, tapName string) error {
	return cns.WithNetNSPath(nsPath, func(_ cns.NetNS) error {
		if _, err := netlink.LinkByName(tapName); err != nil {
			return fmt.Errorf("tap %s: %w", tapName, err)
		}
		return nil
	})
}

// deleteTAPInNetns is idempotent: an absent TAP or an already-deleted netns is success, or every teardown retry after a partial failure would wedge on the missing device.
func deleteTAPInNetns(nsPath, tapName string) error {
	err := cns.WithNetNSPath(nsPath, func(_ cns.NetNS) error {
		link, err := netlink.LinkByName(tapName)
		if err != nil {
			if _, ok := errors.AsType[netlink.LinkNotFoundError](err); ok {
				return nil
			}
			return fmt.Errorf("find %s: %w", tapName, err)
		}
		return netlink.LinkDel(link)
	})
	if _, ok := errors.AsType[cns.NSPathNotExistErr](err); ok {
		return nil
	}
	return err
}

// setupTCRedirect wires ifName <-> tapName inside target netns, returns MAC.
func setupTCRedirect(nsPath, ifName, tapName string, queues int, overrideMAC string) (string, error) {
	var mac string
	err := cns.WithNetNSPath(nsPath, func(_ cns.NetNS) error {
		var nsErr error
		mac, nsErr = tcRedirectInNS(ifName, tapName, queues, overrideMAC)
		return nsErr
	})
	return mac, err
}

// tcRedirectInNS runs TC redirect setup inside target netns.
func tcRedirectInNS(ifName, tapName string, queues int, overrideMAC string) (string, error) {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return "", fmt.Errorf("find %s: %w", ifName, err)
	}

	if overrideMAC != "" {
		hwAddr, parseErr := net.ParseMAC(overrideMAC)
		if parseErr != nil {
			return "", fmt.Errorf("parse MAC %s: %w", overrideMAC, parseErr)
		}
		if setErr := netlink.LinkSetHardwareAddr(link, hwAddr); setErr != nil {
			return "", fmt.Errorf("set MAC on %s: %w", ifName, setErr)
		}
	}

	mac := link.Attrs().HardwareAddr.String()
	if overrideMAC != "" {
		mac = overrideMAC
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return "", fmt.Errorf("list addrs on %s: %w", ifName, err)
	}
	for _, addr := range addrs {
		if delErr := netlink.AddrDel(link, &addr); delErr != nil {
			return "", fmt.Errorf("flush addr %s on %s: %w", addr.IPNet, ifName, delErr)
		}
	}

	if tapErr := network.CreateTAP(tapName, queues); tapErr != nil {
		return "", tapErr
	}
	tapLink, err := netlink.LinkByName(tapName)
	if err != nil {
		return "", fmt.Errorf("find tap %s: %w", tapName, err)
	}

	_ = network.TuneTAP(tapLink)

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
