package cni

import (
	"cmp"
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

// setLinkStateInNetns brings ifNames up or down inside nsPath. A missing netns or link is success: Quiesce/Unquiesce run across stop/restart and partial teardown, where the plumbing may already be gone.
func setLinkStateInNetns(nsPath string, ifNames []string, up bool) error {
	transition := netlink.LinkSetDown
	if up {
		transition = netlink.LinkSetUp
	}
	err := cns.WithNetNSPath(nsPath, func(_ cns.NetNS) error {
		for _, name := range ifNames {
			link, err := netlink.LinkByName(name)
			if err != nil {
				if _, ok := errors.AsType[netlink.LinkNotFoundError](err); ok {
					continue
				}
				return fmt.Errorf("find %s: %w", name, err)
			}
			if err := transition(link); err != nil {
				return fmt.Errorf("set %s state: %w", name, err)
			}
		}
		return nil
	})
	if _, ok := errors.AsType[cns.NSPathNotExistErr](err); ok {
		return nil
	}
	return err
}

func setupTCRedirect(nsPath, ifName, tapName string, queues int, overrideMAC string) (string, int, error) {
	var (
		mac string
		mtu int
	)
	err := cns.WithNetNSPath(nsPath, func(_ cns.NetNS) error {
		var nsErr error
		mac, mtu, nsErr = tcRedirectInNS(ifName, tapName, queues, overrideMAC)
		return nsErr
	})
	return mac, mtu, err
}

func tcRedirectInNS(ifName, tapName string, queues int, overrideMAC string) (string, int, error) {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return "", 0, fmt.Errorf("find %s: %w", ifName, err)
	}

	if overrideMAC != "" {
		hwAddr, parseErr := net.ParseMAC(overrideMAC)
		if parseErr != nil {
			return "", 0, fmt.Errorf("parse MAC %s: %w", overrideMAC, parseErr)
		}
		if setErr := netlink.LinkSetHardwareAddr(link, hwAddr); setErr != nil {
			return "", 0, fmt.Errorf("set MAC on %s: %w", ifName, setErr)
		}
	}

	mac := cmp.Or(overrideMAC, link.Attrs().HardwareAddr.String())

	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return "", 0, fmt.Errorf("list addrs on %s: %w", ifName, err)
	}
	for _, addr := range addrs {
		if delErr := netlink.AddrDel(link, &addr); delErr != nil {
			return "", 0, fmt.Errorf("flush addr %s on %s: %w", addr.IPNet, ifName, delErr)
		}
	}

	if _, tapErr := network.CreateTAP(tapName, queues); tapErr != nil {
		return "", 0, tapErr
	}
	tapLink, err := netlink.LinkByName(tapName)
	if err != nil {
		return "", 0, fmt.Errorf("find tap %s: %w", tapName, err)
	}

	_ = network.TuneTAP(tapLink)

	if mtu := link.Attrs().MTU; mtu > 0 {
		if mtuErr := netlink.LinkSetMTU(tapLink, mtu); mtuErr != nil {
			return "", 0, fmt.Errorf("set tap %s mtu %d: %w", tapName, mtu, mtuErr)
		}
	}

	for _, l := range []netlink.Link{link, tapLink} {
		if upErr := netlink.LinkSetUp(l); upErr != nil {
			return "", 0, fmt.Errorf("set %s up: %w", l.Attrs().Name, upErr)
		}
	}

	for _, l := range []netlink.Link{link, tapLink} {
		qdisc := &netlink.Ingress{
			LinkIndex: l.Attrs().Index,
			Parent:    netlink.HANDLE_INGRESS,
		}
		if qdiscErr := netlink.QdiscAdd(qdisc); qdiscErr != nil {
			return "", 0, fmt.Errorf("add ingress qdisc on %s: %w", l.Attrs().Name, qdiscErr)
		}
	}

	if err := addTCRedirect(link, tapLink); err != nil {
		return "", 0, fmt.Errorf("redirect %s -> %s: %w", ifName, tapName, err)
	}
	if err := addTCRedirect(tapLink, link); err != nil {
		return "", 0, fmt.Errorf("redirect %s -> %s: %w", tapName, ifName, err)
	}
	return mac, link.Attrs().MTU, nil
}

// addTCRedirect redirects all ingress packets from one link to another.
func addTCRedirect(from, to netlink.Link) error {
	filter := &netlink.U32{
		LinkIndex: from.Attrs().Index,
		Parent:    netlink.HANDLE_INGRESS,
		Priority:  1,
		Protocol:  syscall.ETH_P_ALL,
		Sel: &netlink.TcU32Sel{
			Flags: netlink.TC_U32_TERMINAL,
			Keys: []netlink.TcU32Key{
				{Mask: 0x0, Val: 0x0, Off: 0, OffMask: 0x0},
			},
		},
		Actions: []netlink.Action{
			&netlink.MirredAction{
				Action:       netlink.TC_ACT_STOLEN,
				MirredAction: netlink.TCA_EGRESS_REDIR,
				Ifindex:      to.Attrs().Index,
			},
		},
	}
	return netlink.FilterAdd(filter)
}
