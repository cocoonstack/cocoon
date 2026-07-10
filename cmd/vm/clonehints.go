package vm

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/types"
)

type nicHint struct {
	mac, ip, gw string
	prefix      int
}

func printPostCloneHints(vm *types.VM) {
	if vm.Config.Windows {
		fmt.Println()
		fmt.Println("Windows clone: NICs hot-swapped with new MAC addresses.")
		fmt.Println("Run inside the guest if no IPv4 (DHCP):")
		fmt.Println()
		fmt.Println(`  powershell -nop -c '$x=Get-PnpDevice -Class Net -PresentOnly;$x|Disable-PnpDevice -Confirm:$false;$x|Enable-PnpDevice -Confirm:$false'`)
		fmt.Println()
		fmt.Println("Static IP: run inside the guest after the rebind above, e.g.:")
		fmt.Println()
		fmt.Println(`  netsh interface ipv4 set address name="Ethernet" static <IP> <MASK> <GATEWAY>`)
		fmt.Println()
		return
	}

	isCloudimg := vm.Config.ImageType == types.ImageTypeCloudImg

	fmt.Println()
	fmt.Println("Run inside the guest to finish setup:")
	fmt.Println()
	fmt.Println("  # Release memory for balloon")
	fmt.Println("  echo 3 > /proc/sys/vm/drop_caches")

	// FC clone: guest MAC is baked in vmstate; change it before networkd config.
	if vm.Hypervisor == string(config.HypervisorFirecracker) {
		printFCMACHints(vm.NetworkConfigs)
	}

	fmt.Println()
	fmt.Println("  # Clean old network configs from snapshot and write new ones (MAC-based)")
	fmt.Println("  rm -f /etc/systemd/network/10-*.network")

	if isCloudimg {
		printCloudimgNetworkHints()
	} else {
		printOCINetworkHints(vm)
	}
	fmt.Println()
}

func printFCMACHints(networkConfigs []*types.NetworkConfig) {
	fmt.Println()
	fmt.Println("  # Fix guest MAC addresses (FC clone retains source VM's MAC)")
	for i, nc := range networkConfigs {
		if nc == nil || nc.MAC == "" {
			continue
		}
		fmt.Printf("  ip link set dev eth%d down\n", i)
		fmt.Printf("  ip link set dev eth%d address %s\n", i, nc.MAC)
		fmt.Printf("  ip link set dev eth%d up\n", i)
	}
}

func printCloudimgNetworkHints() {
	fmt.Println("  cloud-init clean --logs --seed --configs network && cloud-init init --local && cloud-init init")
	fmt.Println("  cloud-init modules --mode=config && systemctl restart systemd-networkd")
}

func printOCINetworkHints(vm *types.VM) {
	fmt.Println()
	fmt.Printf("  # Set hostname\n")
	fmt.Printf("  hostnamectl set-hostname %s\n", vm.Config.Name)

	var staticNICs []nicHint
	var dhcpMACs []string
	for _, nc := range vm.NetworkConfigs {
		if nc == nil || nc.MAC == "" {
			continue
		}
		if nc.Network != nil && nc.Network.IP != "" {
			staticNICs = append(staticNICs, nicHint{
				mac:    nc.MAC,
				ip:     nc.Network.IP,
				prefix: nc.Network.Prefix,
				gw:     nc.Network.Gateway,
			})
		} else {
			dhcpMACs = append(dhcpMACs, nc.MAC)
		}
	}

	if len(staticNICs) == 0 && len(dhcpMACs) == 0 {
		return
	}

	if len(staticNICs) > 0 {
		printBashArray("macs", staticNICs, func(n nicHint) string { return n.mac })
		printBashArray("addrs", staticNICs, func(n nicHint) string { return fmt.Sprintf("%s/%d", n.ip, n.prefix) })

		hasGW := slices.ContainsFunc(staticNICs, func(n nicHint) bool { return n.gw != "" })
		if hasGW {
			printBashArray("gws", staticNICs, func(n nicHint) string { return n.gw })
		}

		fmt.Println("  for i in \"${!macs[@]}\"; do")
		fmt.Println("    f=\"/etc/systemd/network/10-${macs[$i]//:/}.network\"")
		writeNet := `    printf '[Match]\nMACAddress=` + `%s\n\n[Network]\nAddress=%s\n' "${macs[$i]}" "${addrs[$i]}" > "$f"`
		fmt.Println(writeNet)
		if hasGW {
			writeGW := `    [ -n "${gws[$i]}" ] && printf 'Gateway=` + `%s\n' "${gws[$i]}" >> "$f"`
			fmt.Println(writeGW)
		}
		fmt.Println("  done")
	}

	if len(dhcpMACs) > 0 {
		fmt.Println("  # DHCP NICs")
		for _, mac := range dhcpMACs {
			sanitized := strings.ReplaceAll(mac, ":", "")
			writeDHCP := fmt.Sprintf(`  printf '[Match]\nMACAddress=%s\n\n[Network]\nDHCP=ipv4\n'`+` > "/etc/systemd/network/10-%s.network"`, mac, sanitized)
			fmt.Println(writeDHCP)
		}
	}

	fmt.Println("  systemctl restart systemd-networkd")
}

func printBashArray(name string, nics []nicHint, field func(nicHint) string) {
	fmt.Printf("  %s=(", name)
	for i, n := range nics {
		if i > 0 {
			fmt.Print(" ")
		}
		fmt.Printf("'%s'", field(n))
	}
	fmt.Println(")")
}
