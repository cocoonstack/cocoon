# Known Issues

## Post-clone IP conflict window

After `cocoon vm clone`, the cloned VM resumes with the **original VM's IP address** configured inside the guest, even though CNI has allocated a new IP for the clone's network namespace. MAC addresses are handled automatically — during clone, the snapshot's old NICs are hot-swapped (removed and re-added with new MACs) while the VM is paused, so the guest wakes up with correct MACs. The clone can still reach the network during the IP conflict window because:

- The entire data path is **L2** (TC ingress redirect + bridge) — no component checks whether the guest's source IP matches the CNI-allocated IP.
- Standard **bridge CNI does not enforce IP ↔ veth binding** at the data plane. The `host-local` IPAM only tracks allocations in its control-plane state files; it does not install data-plane rules.

**Consequence**: if the original VM is still running, both VMs advertise the same IP via ARP with different MACs. The upstream gateway flaps between the two MACs, causing **intermittent connectivity loss for both VMs** until the clone's guest IP is reconfigured.

**Mitigation**: run the post-clone guest setup commands printed by `cocoon vm clone` as soon as possible (see [Post-Clone Guest Setup](README.md#post-clone-guest-setup)). For cloudimg VMs this means re-running `cloud-init`; for OCI VMs this means `ip addr flush` + reconfigure with the new IP.

## Clone resource constraints

Clone resources (CPU, memory, storage, NICs) can only be **increased**, never decreased below the snapshot's original values. See [Clone Constraints](README.md#clone-constraints) for details.

## Restore requires a running VM

`cocoon vm restore` only works on running VMs — it relies on the existing network namespace (netns, tap devices, TC redirect) surviving the CH process restart. A stopped VM's network state may not be intact (e.g., after host reboot the netns is gone). For stopped VMs or cross-VM restore, use `cocoon vm clone` which creates fresh network resources. See [Restore Constraints](README.md#restore-constraints) for all requirements.

## OCI VM multi-NIC kernel IP limitation

OCI VMs use the kernel `ip=` boot parameter for network configuration. While multiple `ip=` parameters can be specified, the Linux kernel only reliably configures **one interface** via this mechanism — subsequent `ip=` parameters may be silently ignored or produce inconsistent results depending on kernel version.

**Consequence**: on a cold boot (stop + start) of an OCI VM with multiple NICs, only the first NIC receives its IP from the kernel `ip=` parameter. Additional NICs must be configured by the guest init system (e.g., systemd-networkd `.network` files written by the post-clone hints).

**Workaround**: the post-clone setup hints write persistent MAC-based systemd-networkd configs for **all** NICs. These survive reboots and correctly configure every interface regardless of the kernel `ip=` limitation.

## Cloud image UEFI boot compatibility

Cocoon uses [rust-hypervisor-firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware) (`CLOUDHV.fd`) for cloud image UEFI boot. This firmware implements a minimal EFI specification and does **not** support the `InstallMultipleProtocolInterfaces()` call required by newer distributions.

**Affected images** (kernel panic on boot — GRUB loads kernel but not initrd):

- Ubuntu 24.04 (Noble) and later
- Debian 13 (Trixie) and later

**Working images**:

- Ubuntu 22.04 (Jammy)

This is an upstream issue tracked in [rust-hypervisor-firmware#333](https://github.com/cloud-hypervisor/rust-hypervisor-firmware/issues/333) and [cloud-hypervisor#7356](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7356). As a workaround, use **OCI VM images** for Ubuntu 24.04 — OCI images use direct kernel boot and are not affected.

## DHCP networks should not use DHCP IPAM in CNI

When using a DHCP-based network (e.g., macvlan attached to a network with an external DHCP server), the CNI conflist should **not** use the `dhcp` IPAM plugin. Instead, configure the CNI plugin with **no IPAM** (or `"ipam": {}`) and let the guest obtain its IP directly from the external DHCP server.

The `dhcp` IPAM plugin runs a host-side DHCP client that competes with the guest's own DHCP client, causing:

- **Duplicate DHCP requests** — both host-side (CNI IPAM) and guest-side DHCP clients request leases for the same MAC, confusing DHCP servers and leading to lease conflicts.
- **IP mismatch** — the host-side DHCP client may obtain a different IP than the guest, so Cocoon's recorded IP does not match the guest's actual IP.
- **Lease renewal failures** — the CNI `dhcp` daemon must remain running to renew leases; if it crashes or is restarted, the host-side lease expires while the guest keeps using the IP.

This applies to **all CNI plugins** where the upstream network provides DHCP (bridge with external DHCP, macvlan, ipvlan, etc.). The correct approach is:

```json
{
  "cniVersion": "1.0.0",
  "name": "my-dhcp-network",
  "plugins": [
    {
      "type": "macvlan",
      "master": "eth0",
      "mode": "bridge",
      "ipam": {}
    }
  ]
}
```

Cocoon detects when CNI returns no IP allocation and automatically configures the guest for DHCP — cloudimg VMs get `DHCP=ipv4` in their Netplan config, and OCI VMs get DHCP systemd-networkd units generated by the initramfs.
