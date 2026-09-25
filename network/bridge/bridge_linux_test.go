//go:build linux

package bridge

import (
	"errors"
	"fmt"
	"os"
	"testing"

	cns "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

func TestVerifyRequiresTheTAPOnTheBridge(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for netns and netlink")
	}
	testNS, err := cns.TempNetNS()
	if err != nil {
		t.Skipf("create netns: %v", err)
	}
	t.Cleanup(func() { _ = testNS.Close() })

	err = testNS.Do(func(cns.NetNS) error {
		if err := netlink.LinkAdd(&netlink.Bridge{Name: "cbr0"}); err != nil {
			return fmt.Errorf("add bridge: %w", err)
		}
		b, err := New(&config.Config{}, "cbr0")
		if err != nil {
			return err
		}
		vmID := "0123456789abcdef"
		tap := network.TAPName(b.tapPrefix, vmID, 0)
		if _, err := network.CreateTAP(tap, 1); err != nil {
			return err
		}
		expected := []*types.NetworkConfig{{TAP: tap, MAC: "02:00:00:00:00:01", NumQueues: 1}}
		if err := b.Verify(t.Context(), vmID, expected); err == nil {
			return errors.New("verify passed a TAP that is not on the bridge")
		}
		if _, err := b.Add(t.Context(), vmID, &types.VMConfig{CPU: 1}, network.AddRecover(expected)...); err != nil {
			return fmt.Errorf("recover add: %w", err)
		}
		return b.Verify(t.Context(), vmID, expected)
	})
	if err != nil {
		t.Fatal(err)
	}
}
