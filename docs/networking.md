# Networking

CNI-backed TAP networking with TC redirect, bridge mode, and NIC hot-resize.

## Overview

Cocoon uses [CNI](https://www.cni.dev/) for VM networking. Each NIC is backed by a TAP device wired to the CNI veth via TC ingress redirect — no bridge sits in the data path.

### Architecture

```
Guest virtio-net  ←→  TAP (multi-queue)  ←TC redirect→  veth  ←→  CNI bridge/overlay
```

- **Multi-queue**: each TAP device is created with one queue pair per boot vCPU (`num_queues = 2 × vCPU` in Cloud Hypervisor), enabling per-CPU TX/RX rings for better throughput. Ring depth per queue is configurable via `--queue-size` (default 512; larger values improve bulk download throughput, smaller values improve RPC latency)
- **Offload**: TSO, UFO, and checksum offload are enabled on the virtio-net device; TAP uses `VNET_HDR` for zero-copy GSO passthrough
- **MAC passthrough**: the guest NIC inherits the CNI veth's MAC address, satisfying anti-spoofing requirements of Cilium, Calico eBPF, and VPC ENI plugins
- **MTU sync**: TAP MTU is automatically synced to the veth to prevent silent large-packet drops in overlay or jumbo-frame setups
- **IPv4 only**: cocoon records the first IPv4 address from the CNI result; IPv6-only plugin results are not persisted (a warning is logged and the NIC carries no network info)

### Options

- **Default**: 1 NIC with automatic IP assignment via CNI
- **No network**: `--nics 0` creates a VM with no network interfaces
- **Multi-NIC**: `--nics N` creates N interfaces; for cloudimg VMs all NICs are auto-configured via Netplan. For OCI images the kernel `ip=` parameter configures only the last NIC on a cold boot (each `ip=` overrides the previous one) — configure the rest inside the guest (see [known issues](known-issues.md#oci-vm-multi-nic-kernel-ip-limitation))
- **Multi-network**: `--network <name>` selects a specific CNI conflist by name (e.g., `--network macvlan`); omitting uses the first conflist alphabetically. The network name is stored in the VM record for recovery after host reboot. Clone allows `--network` override; restore reuses the existing network.
- **Bridge mode**: `--bridge <device>` creates TAP devices directly on an existing Linux bridge (e.g., `--bridge cni0`), bypassing CNI and TC redirect. VMs get IP via DHCP from the bridge. Mutually exclusive with `--network`
- **DNS**: Use `--dns` to set custom DNS servers (comma separated)

### Host Device Namespaces

Cocoon owns two host-wide name families that [GC](gc.md) sweeps by name: bridge-mode TAPs `bt<vmid8>-<nic>` in the host netns and per-VM CNI netns `cocoon-<vmid>` under `/var/run/netns` (the `rm<vmid8>-<nic>` TAPs a clone restore hands to CH are transient — CH destroys them itself and no sweep touches them). GC reclaims every entry in its families whose VM it does not know, so two installations sharing a host — a second `root_dir`, or a cocoon-derived runner such as cocoon-macos that provisions through cocoon's bridge/CNI backends — must live in different families, or each sweep tears down the other's live guests. `net_scope` re-keys an installation: two alphanumerics (`mt` gives `mt<vmid8>-<nic>` and `mt-<vmid>`); the fixed length keeps distinct scopes from being prefixes of each other, and `bt` / `rm` are rejected because they alias the legacy and restore families. Set it before the first VM is created — existing devices keep their old names.

### CNI Configuration

All `.conflist` files in `--cni-conf-dir` (default `/etc/cni/net.d`) are loaded at startup. Use `--network <name>` to select one by its `name` field; omitting defaults to the first file alphabetically. A typical bridge config:

```json
{
  "cniVersion": "1.0.0",
  "name": "cocoon",
  "type": "bridge",
  "bridge": "cni0",
  "isGateway": true,
  "ipMasq": true,
  "ipam": {
    "type": "host-local",
    "subnet": "10.22.0.0/16",
    "routes": [{ "dst": "0.0.0.0/0" }]
  }
}
```

## NIC Hot-Resize (Cloud Hypervisor, or Firecracker with `--pci`)

`cocoon vm net --nics N VM` brings the running VM's NIC count to `N`. To add NICs, cocoon allocates new host TAP/CNI/bridge plumbing and hot-plugs a fresh NIC into the guest. To remove NICs, it pops from the tail (LIFO) via `vm.remove-device` and tears down the host plumbing.

```bash
# Add a second NIC (or any number).
cocoon vm net my-vm --nics 2

# Remove a NIC (LIFO from the tail).
cocoon vm net my-vm --nics 1
```

On NIC removal, cocoon waits for the guest to ACK B0EJ (CH polls `device_tree` until the device disappears) before tearing down the host TAP / veth / CNI lease. If the guest never ACKs within the eject timeout, the command fails and leaves the cocoon record + host plumbing intact so the operator can quiesce the guest (driver unbind, NetworkManager removal, Windows NDIS halt) and retry.

On Firecracker `--pci` VMs the VMM adds and drops devices without telling the guest: after an add the guest runs `echo 1 > /sys/bus/pci/rescan`, after a remove it drops the stale node with `echo 1 > /sys/class/net/ethN/device/../remove`. `cocoon vm net` prints both (and returns them as `hints` with `--output json`); down the NIC inside the guest before reducing the count. MMIO Firecracker VMs are rejected.

Resize from zero is supported: under CNI, `--nics 0` still provisions a per-VM netns at boot (CH lives in it from the start), so a later `cocoon vm net --nics N` hot-plugs into the same namespace. Bridge mode keeps CH in the host netns regardless of NIC count, so 0→N adds TAPs onto the configured bridge.
